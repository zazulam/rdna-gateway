// rdnamcp serves agent tools over MCP (Streamable HTTP): web search, page
// fetch, document search, a long-term memory, and read-only lab metrics,
// backed by SearXNG, a llama-server embedder and reranker, Qdrant and
// Prometheus (plus the Grafana dashboard JSON that charts it).
//
//	rdnamcp serve                      # config from env, see below
//	rdnamcp index -url http://host:8090/mcp -collection docs ~/notes ./README.md
//
// Env for serve: RDNAMCP_LISTEN (default 127.0.0.1:8090), RDNAMCP_TOKEN,
// SEARXNG_URL, EMBED_URL, EMBED_MODEL, RERANK_URL, RERANK_MODEL, RERANK_CANDIDATES
// (default 20), QDRANT_URL, PROMETHEUS_URL (default http://127.0.0.1:9090) and
// GRAFANA_DASHBOARDS (a directory of dashboard JSON; lab_dashboard is off without it).
// The index subcommand reads RDNAMCP_TOKEN too.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zazulam/rdna-gateway/internal/mcp"
	"github.com/zazulam/rdna-gateway/internal/rag"
	"github.com/zazulam/rdna-gateway/internal/tools"
)

const memoryCollection = "memory"

var validCollection = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "index" {
		index(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: rdnamcp [serve | index -url URL -collection NAME paths...]")
		os.Exit(2)
	}
	serve()
}

func serve() {
	r := &rag.Retriever{
		E: rag.NewEmbedder(env("EMBED_URL", "http://127.0.0.1:8083"), env("EMBED_MODEL", "qwen3-embedding")),
		S: rag.NewStore(env("QDRANT_URL", "http://127.0.0.1:6333")),
		// Reranking 20 vector hits found 85% of answers in the top 5 vs 87% for 30,
		// at two-thirds the cost (docs/BENCHMARKS.md).
		Candidates: 20,
	}
	if v, err := strconv.Atoi(os.Getenv("RERANK_CANDIDATES")); err == nil && v > 0 {
		r.Candidates = v
	}
	if u := os.Getenv("RERANK_URL"); u != "" {
		r.R = rag.NewReranker(u, env("RERANK_MODEL", "reranker"))
	}
	searx := env("SEARXNG_URL", "http://127.0.0.1:8888")
	token := os.Getenv("RDNAMCP_TOKEN")
	if token == "" {
		log.Print("warning: RDNAMCP_TOKEN is empty; the tools are open to anyone who can reach the port")
	}
	ts := append(toolset(r, searx), metricsTools(tools.NewProm(env("PROMETHEUS_URL", "http://127.0.0.1:9090")),
		os.Getenv("GRAFANA_DASHBOARDS"))...)
	s := &mcp.Server{Name: "rdnamcp", Version: "1.0", Token: token, Tools: ts}
	mux := http.NewServeMux()
	mux.Handle("/mcp", s)
	mux.HandleFunc("/metrics", s.Metrics)
	mux.HandleFunc("/health", func(rw http.ResponseWriter, _ *http.Request) { rw.Write([]byte(`{"status":"ok"}`)) })
	addr := env("RDNAMCP_LISTEN", "127.0.0.1:8090")
	log.Printf("rdnamcp on %s (reranker: %v)", addr, r.R != nil)
	log.Fatal((&http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
}

func schema(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(hi, v))
}

func collection(name, def string) (string, error) {
	if name == "" {
		name = def
	}
	if !validCollection.MatchString(name) {
		return "", fmt.Errorf("collection must match %s", validCollection)
	}
	return name, nil
}

func formatHits(hits []rag.Hit) string {
	if len(hits) == 0 {
		return "No matches."
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "[%d] %s (chunk %v, score %.3f)\n%s\n\n", i+1, h.Source(), h.Payload["chunk"], h.Score, h.Text())
	}
	return strings.TrimSpace(b.String())
}

func toolset(r *rag.Retriever, searx string) []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "web_search",
			Description: "Search the web. Returns titles, URLs and snippets; use fetch_url to read a result.",
			Schema:      schema(map[string]any{"query": str("search query"), "max_results": num("1-10, default 5")}, "query"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var p struct {
					Query      string `json:"query"`
					MaxResults int    `json:"max_results"`
				}
				if err := json.Unmarshal(a, &p); err != nil || strings.TrimSpace(p.Query) == "" {
					return "", errors.New("query is required")
				}
				res, err := tools.Search(ctx, searx, p.Query, clamp(p.MaxResults, 1, 10, 5))
				if err != nil {
					return "", err
				}
				if len(res) == 0 {
					return "No results.", nil
				}
				var b strings.Builder
				for i, x := range res {
					fmt.Fprintf(&b, "%d. %s\n%s\n%s\n\n", i+1, x.Title, x.URL, x.Snippet)
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:        "fetch_url",
			Description: "Fetch a public web page and return its readable text.",
			Schema:      schema(map[string]any{"url": str("http(s) URL"), "max_chars": num("default 8000, max 40000")}, "url"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var p struct {
					URL      string `json:"url"`
					MaxChars int    `json:"max_chars"`
				}
				json.Unmarshal(a, &p)
				pg, err := tools.Fetch(ctx, p.URL, clamp(p.MaxChars, 500, 40000, 8000))
				if err != nil {
					return "", err
				}
				out := fmt.Sprintf("URL: %s\nTitle: %s\n\n%s", pg.URL, pg.Title, pg.Text)
				if pg.Truncated {
					out += "\n\n[truncated; ask for more with a larger max_chars]"
				}
				return out, nil
			},
		},
		{
			Name:        "search_docs",
			Description: "Search indexed documents by meaning. Returns the most relevant passages with their source.",
			Schema: schema(map[string]any{"query": str("what you're looking for, as a question or phrase"),
				"collection": str("document collection, default \"docs\""), "k": num("passages, 1-10, default 5")}, "query"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var p struct {
					Query, Collection string
					K                 int
				}
				json.Unmarshal(a, &p)
				c, err := collection(p.Collection, "docs")
				if err != nil {
					return "", err
				}
				hits, err := r.Search(ctx, c, p.Query, clamp(p.K, 1, 10, 5))
				if err != nil {
					return "", err
				}
				return formatHits(hits), nil
			},
		},
		{
			Name:        "list_collections",
			Description: "List document collections and how many passages each holds.",
			Schema:      schema(map[string]any{}),
			Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
				cs, err := r.S.Collections(ctx)
				if err != nil {
					return "", err
				}
				b, _ := json.Marshal(cs)
				return string(b), nil
			},
		},
		{
			Name:        "index_document",
			Description: "Add or replace a document in a collection so search_docs can find it.",
			Schema: schema(map[string]any{"collection": str("collection name"), "source": str("document id, e.g. its path or URL"),
				"text": str("full document text")}, "collection", "source", "text"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var p struct{ Collection, Source, Text string }
				json.Unmarshal(a, &p)
				c, err := collection(p.Collection, "")
				if err != nil {
					return "", err
				}
				if c == memoryCollection {
					return "", errors.New("use remember for the memory collection")
				}
				if p.Source == "" {
					return "", errors.New("source is required")
				}
				n, err := r.Index(ctx, c, p.Source, p.Text, nil)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("indexed %s into %s: %d passages", p.Source, c, n), nil
			},
		},
		{
			Name: "remember",
			Description: "Save a fact, decision or preference to long-term memory, shared by every agent and session. " +
				"Write it so it stands alone, e.g. \"The desktop gateway listens on 100.64.0.10:8080\".",
			Schema: schema(map[string]any{"text": str("the thing to remember"), "topic": str("optional short topic")}, "text"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var p struct{ Text, Topic string }
				json.Unmarshal(a, &p)
				if strings.TrimSpace(p.Text) == "" {
					return "", errors.New("text is required")
				}
				src := "memory/" + time.Now().UTC().Format("20060102T150405.000000000")
				if _, err := r.Index(ctx, memoryCollection, src, p.Text, map[string]any{"topic": p.Topic}); err != nil {
					return "", err
				}
				return "remembered", nil
			},
		},
		{
			Name:        "recall",
			Description: "Search long-term memory for facts saved with remember.",
			Schema:      schema(map[string]any{"query": str("what to look up"), "k": num("1-10, default 5")}, "query"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var p struct {
					Query string
					K     int
				}
				json.Unmarshal(a, &p)
				hits, err := r.Search(ctx, memoryCollection, p.Query, clamp(p.K, 1, 10, 5))
				if err != nil {
					if strings.Contains(err.Error(), "404") {
						return "Memory is empty.", nil
					}
					return "", err
				}
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "- %s (saved %v)\n", h.Text(), h.Payload["indexed_at"])
				}
				if b.Len() == 0 {
					return "Nothing relevant in memory.", nil
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
	}
}

// index walks paths and sends each text file to the server's index_document tool.
func index(args []string) {
	fl := flag.NewFlagSet("index", flag.ExitOnError)
	url := fl.String("url", "http://127.0.0.1:8090/mcp", "rdnamcp endpoint")
	coll := fl.String("collection", "docs", "collection to index into")
	exts := fl.String("ext", ".md,.txt,.rst,.go,.rs,.py,.ts,.tsx,.js,.sh,.toml,.yaml,.yml,.json", "file extensions to include")
	root := fl.String("root", "", "strip this prefix from stored source paths")
	fl.Parse(args)
	want := map[string]bool{}
	for _, e := range strings.Split(*exts, ",") {
		want[strings.TrimSpace(e)] = true
	}
	token := os.Getenv("RDNAMCP_TOKEN")
	client := &http.Client{Timeout: 10 * time.Minute}
	ctx := context.Background()
	files, chunks := 0, 0
	start := time.Now()
	for _, p := range fl.Args() {
		filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "target", "vendor", "dist", "build", ".venv":
					return filepath.SkipDir
				}
				return nil
			}
			if !want[filepath.Ext(path)] {
				return nil
			}
			info, _ := d.Info()
			if info == nil || info.Size() > 1<<20 || info.Size() == 0 {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil || !utf8.Valid(b) {
				return nil
			}
			src := strings.TrimPrefix(path, *root)
			out, err := mcp.Call(ctx, client, *url, token, "index_document",
				map[string]any{"collection": *coll, "source": src, "text": string(b)})
			if err != nil {
				log.Printf("%s: %v", path, err)
				return nil
			}
			var n int
			fmt.Sscanf(out[strings.LastIndex(out, ":")+1:], "%d", &n)
			files++
			chunks += n
			return nil
		})
	}
	log.Printf("indexed %d files, %d passages into %s in %s", files, chunks, *coll, time.Since(start).Round(time.Millisecond))
}
