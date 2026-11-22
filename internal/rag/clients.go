package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zazulam/rdna-gateway/internal/tracing"
)

var defaultClient = &http.Client{Timeout: 5 * time.Minute}

func postJSON(ctx context.Context, c *http.Client, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{resp.StatusCode, fmt.Sprintf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(b)))}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// peer is the server.address and server.port attributes for a client span.
func peer(raw string) []tracing.Attr {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	a := []tracing.Attr{tracing.String("server.address", u.Hostname())}
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = "443"
	} else if port == "" {
		port = "80"
	}
	if p, err := strconv.Atoi(port); err == nil {
		a = append(a, tracing.Int("server.port", p))
	}
	return a
}

// endSpan records err's class (not its message, which can quote the input) and ends sp.
func endSpan(sp tracing.Span, err error) {
	if err != nil {
		et := "error"
		var ue *url.Error
		var se *statusError
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			et = "timeout"
		case errors.Is(err, context.Canceled):
			et = "canceled"
		case errors.As(err, &ue):
			et = "connection_error"
		case errors.As(err, &se):
			et = strconv.Itoa(se.code)
		}
		sp.SetError(et)
	}
	sp.End()
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// Embedder calls an OpenAI-compatible /v1/embeddings endpoint (llama-server
// with --embedding). Embedding models are trained with task prefixes; the
// right ones are chosen from the model name by NewEmbedder.
type Embedder struct {
	URL, Model             string
	QueryPrefix, DocPrefix string
	Batch                  int
	Client                 *http.Client
}

// NewEmbedder picks the query/document prefixes each model family expects.
func NewEmbedder(url, model string) *Embedder {
	e := &Embedder{URL: strings.TrimRight(url, "/"), Model: model, Batch: 32, Client: defaultClient}
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "qwen3"):
		e.QueryPrefix = "Instruct: Given a question, retrieve passages that answer it\nQuery: "
	case strings.Contains(m, "gemma"):
		e.QueryPrefix, e.DocPrefix = "task: search result | query: ", "title: none | text: "
	case strings.Contains(m, "nomic"):
		e.QueryPrefix, e.DocPrefix = "search_query: ", "search_document: "
	}
	return e
}

// Embed returns one vector per text. query selects the query prefix.
func (e *Embedder) Embed(ctx context.Context, texts []string, query bool) (_ [][]float32, err error) {
	ctx, sp := tracing.Start(ctx, "embeddings "+e.Model, tracing.Client, append(peer(e.URL),
		tracing.String("gen_ai.operation.name", "embeddings"), tracing.String("gen_ai.request.model", e.Model),
		tracing.Int("rdnamcp.embeddings.inputs", len(texts)))...)
	defer func() { endSpan(sp, err) }()
	tokens, dims := 0, 0
	defer func() {
		if tokens > 0 {
			sp.SetAttributes(tracing.Int("gen_ai.usage.input_tokens", tokens))
		}
		if dims > 0 {
			sp.SetAttributes(tracing.Int("gen_ai.embeddings.dimension.count", dims))
		}
	}()
	prefix := e.DocPrefix
	if query {
		prefix = e.QueryPrefix
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += e.Batch {
		batch := texts[i:min(i+e.Batch, len(texts))]
		in := make([]string, len(batch))
		for j, t := range batch {
			in[j] = prefix + t
		}
		var r struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := postJSON(ctx, e.Client, http.MethodPost, e.URL+"/v1/embeddings", map[string]any{"model": e.Model, "input": in}, &r); err != nil {
			return nil, err
		}
		if len(r.Data) != len(batch) {
			return nil, fmt.Errorf("embeddings: sent %d inputs, got %d vectors", len(batch), len(r.Data))
		}
		tokens += r.Usage.PromptTokens
		vecs := make([][]float32, len(batch))
		for _, d := range r.Data {
			if d.Index < 0 || d.Index >= len(vecs) {
				return nil, fmt.Errorf("embeddings: index %d out of range", d.Index)
			}
			vecs[d.Index] = d.Embedding
			dims = len(d.Embedding)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// Reranker calls llama-server's /v1/rerank (a cross-encoder run with --reranking).
type Reranker struct {
	URL, Model string
	Client     *http.Client
}

func NewReranker(url, model string) *Reranker {
	return &Reranker{URL: strings.TrimRight(url, "/"), Model: model, Client: defaultClient}
}

// Scores returns a relevance score per document, in input order.
func (r *Reranker) Scores(ctx context.Context, query string, docs []string) (_ []float64, err error) {
	ctx, sp := tracing.Start(ctx, "rerank "+r.Model, tracing.Client, append(peer(r.URL),
		tracing.String("gen_ai.operation.name", "rerank"), tracing.String("gen_ai.request.model", r.Model),
		tracing.Int("rdnamcp.rerank.documents", len(docs)))...)
	defer func() { endSpan(sp, err) }()
	var resp struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	in := map[string]any{"model": r.Model, "query": query, "documents": docs, "top_n": len(docs)}
	if err := postJSON(ctx, r.Client, http.MethodPost, r.URL+"/v1/rerank", in, &resp); err != nil {
		return nil, err
	}
	scores := make([]float64, len(docs))
	for _, x := range resp.Results {
		if x.Index >= 0 && x.Index < len(docs) {
			scores[x.Index] = x.Score
		}
	}
	return scores, nil
}

// Store is a Qdrant collection set, over its REST API.
type Store struct {
	URL    string
	Client *http.Client
}

func NewStore(url string) *Store {
	return &Store{URL: strings.TrimRight(url, "/"), Client: defaultClient}
}

// Point is one stored chunk.
type Point struct {
	ID      string         `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// Hit is a search result.
type Hit struct {
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

func (h Hit) Text() string   { s, _ := h.Payload["text"].(string); return s }
func (h Hit) Source() string { s, _ := h.Payload["source"].(string); return s }

// Ensure creates the collection with cosine distance if it doesn't exist.
func (s *Store) Ensure(ctx context.Context, name string, dim int) error {
	var info struct {
		Result struct {
			Exists bool `json:"exists"`
		} `json:"result"`
	}
	if err := postJSON(ctx, s.Client, http.MethodGet, s.URL+"/collections/"+name+"/exists", nil, &info); err != nil {
		return err
	}
	if info.Result.Exists {
		return nil
	}
	return postJSON(ctx, s.Client, http.MethodPut, s.URL+"/collections/"+name,
		map[string]any{"vectors": map[string]any{"size": dim, "distance": "Cosine"}}, nil)
}

// Drop deletes a collection (no error if it's missing).
func (s *Store) Drop(ctx context.Context, name string) error {
	return postJSON(ctx, s.Client, http.MethodDelete, s.URL+"/collections/"+name, nil, nil)
}

// DeleteSource removes every chunk stored for source.
func (s *Store) DeleteSource(ctx context.Context, name, source string) (err error) {
	ctx, sp := s.dbSpan(ctx, "delete", name)
	defer func() { endSpan(sp, err) }()
	f := map[string]any{"filter": map[string]any{"must": []any{map[string]any{"key": "source", "match": map[string]any{"value": source}}}}}
	return postJSON(ctx, s.Client, http.MethodPost, s.URL+"/collections/"+name+"/points/delete?wait=true", f, nil)
}

// dbSpan starts a client span for one Qdrant operation, named per the
// OpenTelemetry database conventions ("{operation} {collection}").
func (s *Store) dbSpan(ctx context.Context, op, collection string, attrs ...tracing.Attr) (context.Context, tracing.Span) {
	a := append(peer(s.URL), tracing.String("db.system.name", "qdrant"), tracing.String("db.operation.name", op))
	name := op
	if collection != "" {
		a = append(a, tracing.String("db.collection.name", collection))
		name += " " + collection
	}
	return tracing.Start(ctx, name, tracing.Client, append(a, attrs...)...)
}

func (s *Store) Upsert(ctx context.Context, name string, pts []Point) (err error) {
	ctx, sp := s.dbSpan(ctx, "upsert", name, tracing.Int("db.operation.batch.size", len(pts)))
	defer func() { endSpan(sp, err) }()
	for i := 0; i < len(pts); i += 256 {
		batch := pts[i:min(i+256, len(pts))]
		if err := postJSON(ctx, s.Client, http.MethodPut, s.URL+"/collections/"+name+"/points?wait=true",
			map[string]any{"points": batch}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Search(ctx context.Context, name string, vec []float32, limit int) (_ []Hit, err error) {
	ctx, sp := s.dbSpan(ctx, "search", name, tracing.Int("rdnamcp.search.limit", limit))
	var r struct {
		Result []Hit `json:"result"`
	}
	defer func() {
		sp.SetAttributes(tracing.Int("db.response.returned_rows", len(r.Result)))
		endSpan(sp, err)
	}()
	err = postJSON(ctx, s.Client, http.MethodPost, s.URL+"/collections/"+name+"/points/search",
		map[string]any{"vector": vec, "limit": limit, "with_payload": true}, &r)
	return r.Result, err
}

// Collections lists collection names with their point counts.
func (s *Store) Collections(ctx context.Context) (map[string]int, error) {
	var r struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	if err := postJSON(ctx, s.Client, http.MethodGet, s.URL+"/collections", nil, &r); err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, c := range r.Result.Collections {
		var cnt struct {
			Result struct {
				Count int `json:"count"`
			} `json:"result"`
		}
		if err := postJSON(ctx, s.Client, http.MethodPost, s.URL+"/collections/"+c.Name+"/points/count",
			map[string]any{"exact": true}, &cnt); err != nil {
			return nil, err
		}
		out[c.Name] = cnt.Result.Count
	}
	return out, nil
}
