package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(b)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

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
func (e *Embedder) Embed(ctx context.Context, texts []string, query bool) ([][]float32, error) {
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
		}
		if err := postJSON(ctx, e.Client, http.MethodPost, e.URL+"/v1/embeddings", map[string]any{"model": e.Model, "input": in}, &r); err != nil {
			return nil, err
		}
		if len(r.Data) != len(batch) {
			return nil, fmt.Errorf("embeddings: sent %d inputs, got %d vectors", len(batch), len(r.Data))
		}
		vecs := make([][]float32, len(batch))
		for _, d := range r.Data {
			vecs[d.Index] = d.Embedding
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
func (r *Reranker) Scores(ctx context.Context, query string, docs []string) ([]float64, error) {
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
func (s *Store) DeleteSource(ctx context.Context, name, source string) error {
	f := map[string]any{"filter": map[string]any{"must": []any{map[string]any{"key": "source", "match": map[string]any{"value": source}}}}}
	return postJSON(ctx, s.Client, http.MethodPost, s.URL+"/collections/"+name+"/points/delete?wait=true", f, nil)
}

func (s *Store) Upsert(ctx context.Context, name string, pts []Point) error {
	for i := 0; i < len(pts); i += 256 {
		batch := pts[i:min(i+256, len(pts))]
		if err := postJSON(ctx, s.Client, http.MethodPut, s.URL+"/collections/"+name+"/points?wait=true",
			map[string]any{"points": batch}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Search(ctx context.Context, name string, vec []float32, limit int) ([]Hit, error) {
	var r struct {
		Result []Hit `json:"result"`
	}
	err := postJSON(ctx, s.Client, http.MethodPost, s.URL+"/collections/"+name+"/points/search",
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
