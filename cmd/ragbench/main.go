// ragbench measures retrieval and agent tools: retrieval quality per embedder
// and reranker, retrieval vs. long-context answering, web-search answering,
// and embedding/rerank speed. Each subcommand appends JSON lines to -out.
//
//	ragbench corpus    -dir llama.cpp -chunks chunks.jsonl
//	ragbench qa        -chunks chunks.jsonl -qa qa.jsonl -n 150 -llm URL -model gemma4:12b
//	ragbench retrieval -chunks chunks.jsonl -qa qa.jsonl -embed name=url=model ... -rerank name=url=model ...
//	ragbench e2e       -chunks chunks.jsonl -qa qa.jsonl -n 40 -embed ... -rerank ... -llm URL -model gemma4:12b
//	ragbench web       -mcp URL -llm URL -model gemma4:12b
//	ragbench speed     -chunks chunks.jsonl -embed ... -rerank ... -label cpu
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zazulam/rdna-gateway/internal/mcp"
	"github.com/zazulam/rdna-gateway/internal/rag"
)

type chunk struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Index  int    `json:"index"`
	Text   string `json:"text"`
}

type qa struct {
	ChunkID  string `json:"chunk_id"`
	Source   string `json:"source"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type service struct{ name, url, model string }

type services []service

func (s *services) String() string { return fmt.Sprint(*s) }
func (s *services) Set(v string) error {
	p := strings.SplitN(v, "=", 3)
	if len(p) != 3 {
		return fmt.Errorf("want name=url=model, got %q", v)
	}
	*s = append(*s, service{p[0], p[1], p[2]})
	return nil
}

var (
	fl        = flag.NewFlagSet("ragbench", flag.ExitOnError)
	dir       = fl.String("dir", "", "corpus root (corpus)")
	chunksF   = fl.String("chunks", "chunks.jsonl", "chunk file")
	qaF       = fl.String("qa", "qa.jsonl", "question file")
	n         = fl.Int("n", 150, "questions to generate / use")
	seed      = fl.Int64("seed", 7, "sampling seed")
	llm       = fl.String("llm", "http://100.64.0.10:8080", "chat endpoint base URL")
	model     = fl.String("model", "gemma4:12b", "chat model")
	judge     = fl.String("judge", "gemma4:12b", "judge model")
	qdrant    = fl.String("qdrant", "http://127.0.0.1:6333", "Qdrant URL")
	mcpURL    = fl.String("mcp", "", "rdnamcp URL (web)")
	out       = fl.String("out", "rag.jsonl", "results file")
	label     = fl.String("label", "", "label for results")
	stuffChar = fl.Int("stuff-chars", 80000, "long-context budget in chars (e2e)")
	conc      = fl.Int("c", 2, "concurrent requests (speed)")
	embeds    services
	reranks   services
)

func main() {
	fl.Var(&embeds, "embed", "embedder name=url=model (repeatable)")
	fl.Var(&reranks, "rerank", "reranker name=url=model (repeatable)")
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ragbench corpus|qa|retrieval|e2e|web|speed [flags]")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fl.Parse(os.Args[2:])
	ctx := context.Background()
	switch cmd {
	case "corpus":
		corpus()
	case "qa":
		genQA(ctx)
	case "retrieval":
		retrieval(ctx)
	case "e2e":
		e2e(ctx)
	case "web":
		web(ctx)
	case "speed":
		speed(ctx)
	default:
		log.Fatalf("unknown subcommand %s", cmd)
	}
}

// ---- files ----

func readJSONL[T any](path string) []T {
	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func writeJSONL(path string, rows ...any) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	for _, r := range rows {
		b, _ := json.Marshal(r)
		f.Write(append(b, '\n'))
	}
}

func emit(kind string, m map[string]any) {
	m["kind"], m["label"], m["time"] = kind, *label, time.Now().UTC().Format(time.RFC3339)
	writeJSONL(*out, m)
	b, _ := json.Marshal(m)
	fmt.Println(string(b))
}

// ---- corpus ----

func corpus() {
	os.Remove(*chunksF)
	files, total := 0, 0
	filepath.WalkDir(*dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil || len(b) < 400 {
			return nil
		}
		src, _ := filepath.Rel(*dir, path)
		for i, c := range rag.Chunk(string(b), 1200, 200) {
			writeJSONL(*chunksF, chunk{ID: fmt.Sprintf("%s#%d", src, i), Source: src, Index: i, Text: c})
			total++
		}
		files++
		return nil
	})
	log.Printf("%d files, %d chunks -> %s", files, total, *chunksF)
}

// ---- LLM ----

type chatMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatResp struct {
	Msg     chatMsg
	Prompt  int
	Output  int
	Prefill float64 // ms
	Elapsed time.Duration
}

var httpc = &http.Client{Timeout: 10 * time.Minute}

func chat(ctx context.Context, mdl string, msgs []chatMsg, tools []any, maxTokens int, extra map[string]any) (*chatResp, error) {
	body := map[string]any{"model": mdl, "messages": msgs, "max_tokens": maxTokens, "temperature": 0,
		"chat_template_kwargs": map[string]any{"enable_thinking": false, "reasoning_effort": "low"}}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(*llm, "/")+"/v1/chat/completions", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chat: %s: %.300s", resp.Status, raw)
	}
	var r struct {
		Choices []struct {
			Message chatMsg `json:"message"`
		} `json:"choices"`
		Usage struct {
			Prompt int `json:"prompt_tokens"`
			Output int `json:"completion_tokens"`
		} `json:"usage"`
		Timings struct {
			PromptMS float64 `json:"prompt_ms"`
		} `json:"timings"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.Choices) == 0 {
		return nil, fmt.Errorf("chat: bad response %.300s", raw)
	}
	return &chatResp{Msg: r.Choices[0].Message, Prompt: r.Usage.Prompt, Output: r.Usage.Output,
		Prefill: r.Timings.PromptMS, Elapsed: time.Since(start)}, nil
}

// ---- QA generation ----

var reMeta = regexp.MustCompile(`(?i)\b(this|the) (passage|text|document|section|excerpt)\b`)

func genQA(ctx context.Context) {
	chunks := readJSONL[chunk](*chunksF)
	rng := rand.New(rand.NewSource(*seed))
	rng.Shuffle(len(chunks), func(i, j int) { chunks[i], chunks[j] = chunks[j], chunks[i] })
	os.Remove(*qaF)
	made := 0
	for _, c := range chunks {
		if made >= *n {
			break
		}
		if len(c.Text) < 500 {
			continue
		}
		prompt := fmt.Sprintf(`Below is a passage from the llama.cpp documentation (file %s).

Write ONE question a developer might ask that this passage answers specifically. Use concrete names, flags, numbers or commands from it, so the question could not be answered from a different part of the docs. Never refer to "the passage" or "this document". Then give the answer in under 25 words, taken from the passage.

Reply with JSON only: {"question": "...", "answer": "..."}

Passage:
%s`, c.Source, c.Text)
		r, err := chat(ctx, *model, []chatMsg{{Role: "user", Content: prompt}}, nil, 300,
			map[string]any{"response_format": map[string]any{"type": "json_object"}})
		if err != nil {
			log.Printf("qa %s: %v", c.ID, err)
			continue
		}
		var q qa
		if json.Unmarshal([]byte(r.Msg.Content), &q) != nil || len(q.Question) < 15 || q.Answer == "" || reMeta.MatchString(q.Question) {
			continue
		}
		q.ChunkID, q.Source = c.ID, c.Source
		writeJSONL(*qaF, q)
		made++
	}
	log.Printf("%d questions -> %s", made, *qaF)
}

// ---- retrieval quality ----

func collectionName(s service) string {
	return "bench_" + regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(s.name), "_")
}

// indexChunks stores the pre-chunked corpus (so every embedder sees the same chunks).
func indexChunks(ctx context.Context, e *rag.Embedder, st *rag.Store, coll string, chunks []chunk) (time.Duration, error) {
	st.Drop(ctx, coll)
	start := time.Now()
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vecs, err := embedParallel(ctx, e, texts, *conc)
	if err != nil {
		return 0, err
	}
	took := time.Since(start)
	if err := st.Ensure(ctx, coll, len(vecs[0])); err != nil {
		return 0, err
	}
	pts := make([]rag.Point, len(chunks))
	for i, c := range chunks {
		pts[i] = rag.Point{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), Vector: vecs[i],
			Payload: map[string]any{"id": c.ID, "source": c.Source, "chunk": c.Index, "text": c.Text}}
	}
	return took, st.Upsert(ctx, coll, pts)
}

func embedParallel(ctx context.Context, e *rag.Embedder, texts []string, c int) ([][]float32, error) {
	out := make([][]float32, len(texts))
	type job struct{ lo, hi int }
	jobs := make(chan job)
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for w := 0; w < max(1, c); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				v, err := e.Embed(ctx, texts[j.lo:j.hi], false)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				copy(out[j.lo:j.hi], v)
				mu.Unlock()
			}
		}()
	}
	for lo := 0; lo < len(texts); lo += e.Batch {
		jobs <- job{lo, min(lo+e.Batch, len(texts))}
	}
	close(jobs)
	wg.Wait()
	return out, firstErr
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(p*float64(len(s))))]
}

func retrieval(ctx context.Context) {
	chunks := readJSONL[chunk](*chunksF)
	qs := readJSONL[qa](*qaF)
	if len(qs) > *n {
		qs = qs[:*n]
	}
	st := rag.NewStore(*qdrant)
	for _, es := range embeds {
		e := rag.NewEmbedder(es.url, es.model)
		coll := collectionName(es)
		took, err := indexChunks(ctx, e, st, coll, chunks)
		if err != nil {
			log.Fatalf("index %s: %v", es.name, err)
		}
		emit("index", map[string]any{"embedder": es.name, "chunks": len(chunks), "seconds": took.Seconds(),
			"chunks_per_s": float64(len(chunks)) / took.Seconds()})

		// One vector search per question (top 30), reused by every reranker.
		type cand struct {
			hits []rag.Hit
			ms   float64
		}
		cands := make([]cand, len(qs))
		for i, q := range qs {
			t := time.Now()
			v, err := e.Embed(ctx, []string{q.Question}, true)
			if err != nil {
				log.Fatal(err)
			}
			h, err := st.Search(ctx, coll, v[0], 30)
			if err != nil {
				log.Fatal(err)
			}
			cands[i] = cand{h, float64(time.Since(t).Microseconds()) / 1000}
		}
		score := func(rr string, depth int, ranks []int, ms []float64) {
			var r1, r5, r10, mrr float64
			for _, r := range ranks {
				if r == 1 {
					r1++
				}
				if r >= 1 && r <= 5 {
					r5++
				}
				if r >= 1 && r <= 10 {
					r10++
					mrr += 1 / float64(r)
				}
			}
			k := float64(len(ranks))
			emit("retrieval", map[string]any{"embedder": es.name, "reranker": rr, "candidates": depth, "questions": len(ranks),
				"recall_at_1": 100 * r1 / k, "recall_at_5": 100 * r5 / k, "recall_at_10": 100 * r10 / k, "mrr_at_10": mrr / k,
				"latency_p50_ms": pct(ms, 0.5), "latency_p95_ms": pct(ms, 0.95)})
		}
		rank := func(hits []rag.Hit, gold string) int {
			for i, h := range hits {
				if h.Payload["id"] == gold {
					return i + 1
				}
			}
			return 0
		}
		ranks, ms := make([]int, len(qs)), make([]float64, len(qs))
		for i, q := range qs {
			ranks[i], ms[i] = rank(cands[i].hits, q.ChunkID), cands[i].ms
		}
		score("none", 0, ranks, ms)
		// The cross-encoder scores each passage on its own, so one call over the
		// top 30 also gives the ranking for reranking only the top 10 or 20.
		depths := []int{10, 20, 30}
		for _, rs := range reranks {
			rr := rag.NewReranker(rs.url, rs.model)
			dRanks := map[int][]int{}
			for _, d := range depths {
				dRanks[d] = make([]int, len(qs))
			}
			for i, q := range qs {
				t := time.Now()
				docs := make([]string, len(cands[i].hits))
				for j, h := range cands[i].hits {
					docs[j] = h.Text()
				}
				sc, err := rr.Scores(ctx, q.Question, docs)
				if err != nil {
					log.Fatalf("rerank %s: %v", rs.name, err)
				}
				ms[i] = cands[i].ms + float64(time.Since(t).Microseconds())/1000
				for _, d := range depths {
					hits := append([]rag.Hit(nil), cands[i].hits[:min(d, len(cands[i].hits))]...)
					for j := range hits {
						hits[j].Score = sc[j]
					}
					sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
					dRanks[d][i] = rank(hits, q.ChunkID)
				}
			}
			for _, d := range depths {
				// Latency is measured at 30; rerank time scales about linearly with depth.
				lat := ms
				if d != 30 {
					lat = nil
				}
				score(rs.name, d, dRanks[d], lat)
			}
		}
	}
}

// ---- end-to-end answering ----

func grade(ctx context.Context, q qa, answer string) bool {
	p := fmt.Sprintf(`Question: %s
Reference answer: %s
Candidate answer: %s

Does the candidate answer the question correctly, agreeing with the reference on the facts that matter? Extra detail is fine; missing or contradicting the key fact is not. Reply with exactly one word: CORRECT or INCORRECT.`, q.Question, q.Answer, answer)
	r, err := chat(ctx, *judge, []chatMsg{{Role: "user", Content: p}}, nil, 5, nil)
	return err == nil && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(r.Msg.Content)), "CORRECT")
}

func e2e(ctx context.Context) {
	chunks := readJSONL[chunk](*chunksF)
	qs := readJSONL[qa](*qaF)
	if len(qs) > *n {
		qs = qs[:*n]
	}
	bySource := map[string][]chunk{}
	var sources []string
	for _, c := range chunks {
		if bySource[c.Source] == nil {
			sources = append(sources, c.Source)
		}
		bySource[c.Source] = append(bySource[c.Source], c)
	}
	fileText := func(src string) string {
		var b strings.Builder
		for i, c := range bySource[src] {
			if i == 0 {
				b.WriteString(c.Text)
				continue
			}
			// Chunks overlap; append only what the previous chunk didn't cover.
			prev := bySource[src][i-1].Text
			if j := overlapAt(prev, c.Text); j > 0 {
				b.WriteString(c.Text[j:])
			} else {
				b.WriteString("\n\n" + c.Text)
			}
		}
		return b.String()
	}
	if len(embeds) != 1 {
		log.Fatal("e2e wants exactly one -embed (and at most one -rerank)")
	}
	r := &rag.Retriever{E: rag.NewEmbedder(embeds[0].url, embeds[0].model), S: rag.NewStore(*qdrant)}
	coll := collectionName(embeds[0])
	rrName := "none"
	if len(reranks) > 0 {
		r.R, rrName = rag.NewReranker(reranks[0].url, reranks[0].model), reranks[0].name
	}
	rng := rand.New(rand.NewSource(*seed))
	sys := "Answer the developer's question about llama.cpp in one or two sentences. If the provided documentation doesn't say, answer from your own knowledge."
	modes := []string{"closed-book", "oracle-file", "long-context", "retrieval"}
	type acc struct {
		correct, prompt, ttft, total []float64
		retrMS                       []float64
		failed                       int // e.g. the prompt didn't fit the slot: counts as a wrong answer
	}
	res := map[string]*acc{}
	for _, m := range modes {
		res[m] = &acc{}
	}
	for qi, q := range qs {
		for _, m := range modes {
			var ctxText string
			var retrMS float64
			switch m {
			case "oracle-file":
				ctxText = fileText(q.Source)
			case "long-context":
				// The right file plus random others, shuffled, up to the budget: what an
				// agent does when it pastes "the docs" instead of retrieving.
				docs := []string{q.Source}
				size := len(fileText(q.Source))
				for _, i := range rng.Perm(len(sources)) {
					s := sources[i]
					if s == q.Source {
						continue
					}
					if l := len(fileText(s)); size+l < *stuffChar {
						docs, size = append(docs, s), size+l
					}
				}
				rng.Shuffle(len(docs), func(i, j int) { docs[i], docs[j] = docs[j], docs[i] })
				var b strings.Builder
				for _, d := range docs {
					fmt.Fprintf(&b, "=== %s ===\n%s\n\n", d, fileText(d))
				}
				ctxText = b.String()
			case "retrieval":
				t := time.Now()
				hits, err := r.Search(ctx, coll, q.Question, 5)
				if err != nil {
					log.Fatal(err)
				}
				retrMS = float64(time.Since(t).Microseconds()) / 1000
				var b strings.Builder
				for _, h := range hits {
					fmt.Fprintf(&b, "=== %s ===\n%s\n\n", h.Payload["source"], h.Text())
				}
				ctxText = b.String()
			}
			user := q.Question
			if ctxText != "" {
				user = "Documentation:\n\n" + ctxText + "\nQuestion: " + q.Question
			}
			resp, err := chat(ctx, *model, []chatMsg{{Role: "system", Content: sys}, {Role: "user", Content: user}}, nil, 200,
				map[string]any{"cache_prompt": false})
			if err != nil {
				log.Printf("%s q%d: %v", m, qi, err)
				res[m].failed++
				continue
			}
			ok := grade(ctx, q, resp.Msg.Content)
			a := res[m]
			a.correct = append(a.correct, map[bool]float64{true: 1, false: 0}[ok])
			a.prompt = append(a.prompt, float64(resp.Prompt))
			a.ttft = append(a.ttft, resp.Prefill/1000)
			a.total = append(a.total, resp.Elapsed.Seconds()+retrMS/1000)
			a.retrMS = append(a.retrMS, retrMS)
		}
		log.Printf("e2e %d/%d", qi+1, len(qs))
	}
	mean := func(xs []float64) float64 {
		s := 0.0
		for _, x := range xs {
			s += x
		}
		return s / float64(max(1, len(xs)))
	}
	for _, m := range modes {
		a := res[m]
		right := mean(a.correct) * float64(len(a.correct))
		emit("e2e", map[string]any{"mode": m, "embedder": embeds[0].name, "reranker": rrName, "model": *model,
			"questions": len(a.correct) + a.failed, "failed": a.failed,
			"accuracy": 100 * right / float64(max(1, len(a.correct)+a.failed)), "accuracy_answered": 100 * mean(a.correct),
			"prompt_tokens_mean": mean(a.prompt),
			"prefill_s_mean":     mean(a.ttft), "total_s_mean": mean(a.total), "total_s_p95": pct(a.total, 0.95),
			"retrieval_ms_mean": mean(a.retrMS)})
	}
}

// overlapAt returns where next starts repeating the tail of prev (0 if it doesn't).
func overlapAt(prev, next string) int {
	for l := min(len(prev), len(next), 400); l >= 40; l-- {
		if strings.HasSuffix(prev, next[:l]) {
			return l
		}
	}
	return 0
}

// ---- web search ----

type webQ struct {
	Q, Truth string
}

// truths fetches current answers from canonical sources, so the questions
// stay valid as releases happen.
func truths(ctx context.Context) []webQ {
	get := func(url string, into any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "ragbench")
		resp, err := httpc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if s, ok := into.(*string); ok {
			b, _ := io.ReadAll(resp.Body)
			*s = string(b)
			return nil
		}
		return json.NewDecoder(resp.Body).Decode(into)
	}
	var out []webQ
	var gov string
	if get("https://go.dev/VERSION?m=text", &gov) == nil {
		out = append(out, webQ{"What is the latest stable release of the Go programming language?", strings.TrimPrefix(strings.Fields(gov)[0], "go")})
	}
	gh := func(repo, what string) {
		var r struct {
			Tag string `json:"tag_name"`
		}
		if get("https://api.github.com/repos/"+repo+"/releases/latest", &r) == nil && r.Tag != "" {
			out = append(out, webQ{"What is the version number of the latest release of " + what + "?", strings.TrimPrefix(r.Tag, "v")})
		}
	}
	gh("ggml-org/llama.cpp", "llama.cpp (ggml-org/llama.cpp; the latest stable release, not a bNNNN prerelease build)")
	gh("qdrant/qdrant", "the Qdrant vector database")
	gh("ollama/ollama", "Ollama")
	gh("grafana/grafana", "Grafana")
	gh("prometheus/prometheus", "Prometheus")
	eol := func(product, what string) {
		var r []struct {
			Latest string `json:"latest"`
		}
		if get("https://endoflife.date/api/"+product+".json", &r) == nil && len(r) > 0 && r[0].Latest != "" {
			out = append(out, webQ{"What is the latest released version of " + what + "?", r[0].Latest})
		}
	}
	eol("python", "Python")
	eol("nodejs", "Node.js")
	eol("kubernetes", "Kubernetes")
	eol("postgresql", "PostgreSQL")
	eol("rust", "Rust")
	return out
}

func web(ctx context.Context) {
	qs := truths(ctx)
	token := os.Getenv("RDNAMCP_TOKEN")
	// Tool schemas straight from the MCP server, as OpenAI function tools.
	var tools []any
	{
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, *mcpURL, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpc.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		var r struct {
			Result struct {
				Tools []struct {
					Name        string         `json:"name"`
					Description string         `json:"description"`
					InputSchema map[string]any `json:"inputSchema"`
				} `json:"tools"`
			} `json:"result"`
		}
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		for _, t := range r.Result.Tools {
			if t.Name == "web_search" || t.Name == "fetch_url" {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.InputSchema}})
			}
		}
	}
	today := time.Now().Format("January 2, 2006")
	for _, mode := range []string{"closed-book", "web-tools"} {
		var correct, calls, secs []float64
		for _, q := range qs {
			sys := "Today is " + today + ". Answer with the version number and nothing else."
			if mode == "web-tools" {
				sys = "Today is " + today + ". Use web_search (and fetch_url when snippets aren't enough) to find current facts; " +
					"prefer official sources. Then answer with the version number and nothing else."
			}
			msgs := []chatMsg{{Role: "system", Content: sys}, {Role: "user", Content: q.Q}}
			start := time.Now()
			nCalls := 0
			var answer string
			for step := 0; step < 6; step++ {
				var t []any
				if mode == "web-tools" && step < 5 {
					t = tools
				}
				r, err := chat(ctx, *model, msgs, t, 400, nil)
				if err != nil {
					log.Printf("web %s: %v", q.Q, err)
					break
				}
				if len(r.Msg.ToolCalls) == 0 {
					answer = r.Msg.Content
					break
				}
				msgs = append(msgs, r.Msg)
				for _, tc := range r.Msg.ToolCalls {
					nCalls++
					var args map[string]any
					json.Unmarshal([]byte(tc.Function.Arguments), &args)
					res, err := mcp.Call(ctx, httpc, *mcpURL, token, tc.Function.Name, args)
					if err != nil {
						res = "error: " + err.Error()
					}
					if len(res) > 12000 {
						res = res[:12000]
					}
					msgs = append(msgs, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: res})
				}
			}
			ok := strings.Contains(answer, q.Truth)
			log.Printf("%s | %s | truth %s | got %q | %v", mode, q.Q[:min(50, len(q.Q))], q.Truth, strings.TrimSpace(answer), ok)
			correct = append(correct, map[bool]float64{true: 1, false: 0}[ok])
			calls = append(calls, float64(nCalls))
			secs = append(secs, time.Since(start).Seconds())
		}
		mean := func(xs []float64) float64 {
			s := 0.0
			for _, x := range xs {
				s += x
			}
			return s / float64(max(1, len(xs)))
		}
		emit("web", map[string]any{"mode": mode, "model": *model, "questions": len(correct), "accuracy": 100 * mean(correct),
			"tool_calls_mean": mean(calls), "seconds_mean": mean(secs), "seconds_p95": pct(secs, 0.95)})
	}
}

// ---- speed ----

func speed(ctx context.Context) {
	chunks := readJSONL[chunk](*chunksF)
	if len(chunks) > 1024 {
		chunks = chunks[:1024]
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	for _, es := range embeds {
		e := rag.NewEmbedder(es.url, es.model)
		e.Embed(ctx, texts[:8], false) // warm up
		start := time.Now()
		if _, err := embedParallel(ctx, e, texts, *conc); err != nil {
			log.Fatal(err)
		}
		s := time.Since(start).Seconds()
		var q []float64
		for _, c := range chunks[:50] {
			t := time.Now()
			e.Embed(ctx, []string{c.Text[:min(200, len(c.Text))]}, true)
			q = append(q, float64(time.Since(t).Microseconds())/1000)
		}
		emit("speed-embed", map[string]any{"embedder": es.name, "chunks": len(texts), "chunks_per_s": float64(len(texts)) / s,
			"query_p50_ms": pct(q, 0.5), "concurrency": *conc})
	}
	for _, rs := range reranks {
		rr := rag.NewReranker(rs.url, rs.model)
		var ms []float64
		for i := 0; i < 20; i++ {
			docs := make([]string, 30)
			for j := range docs {
				docs[j] = texts[(i*30+j)%len(texts)]
			}
			t := time.Now()
			if _, err := rr.Scores(ctx, "how do I enable flash attention on ROCm?", docs); err != nil {
				log.Fatal(err)
			}
			ms = append(ms, float64(time.Since(t).Microseconds())/1000)
		}
		emit("speed-rerank", map[string]any{"reranker": rs.name, "candidates": 30, "p50_ms": pct(ms, 0.5), "p95_ms": pct(ms, 0.95)})
	}
}
