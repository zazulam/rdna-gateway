package rag

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestChunkCoversTextWithOverlap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("Sentence number ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(". ")
		if i%10 == 9 {
			b.WriteString("\n\n")
		}
	}
	text := b.String()
	chunks := Chunk(text, 300, 60)
	if len(chunks) < 5 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if len(c) > 300 {
			t.Fatalf("chunk %d is %d bytes", i, len(c))
		}
		if !strings.Contains(text, c) {
			t.Fatalf("chunk %d isn't a substring of the input", i)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(text), chunks[len(chunks)-1]) {
		t.Fatal("last chunk doesn't reach the end")
	}
	if Chunk("  ", 100, 10) != nil {
		t.Fatal("blank input should give no chunks")
	}
}

// fakeServices is an embedder (bag of letters), reranker (word overlap) and a
// minimal in-memory Qdrant, on one test server.
type fakeServices struct {
	mu   sync.Mutex
	pts  map[string]map[string]Point
	seen []string // embedded inputs
}

func vec(s string) []float32 {
	v := make([]float32, 26)
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			v[r-'a']++
		}
	}
	return v
}

func cos(a, b []float32) float64 {
	var d, na, nb float64
	for i := range a {
		d += float64(a[i] * b[i])
		na += float64(a[i] * a[i])
		nb += float64(b[i] * b[i])
	}
	return d / (math.Sqrt(na*nb) + 1e-9)
}

func (f *fakeServices) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]any
	json.NewDecoder(r.Body).Decode(&in)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	out := map[string]any{}
	switch {
	case r.URL.Path == "/v1/embeddings":
		var data []any
		for i, s := range in["input"].([]any) {
			f.seen = append(f.seen, s.(string))
			data = append(data, map[string]any{"index": i, "embedding": vec(s.(string))})
		}
		out["data"] = data
	case r.URL.Path == "/v1/rerank":
		q := strings.Fields(in["query"].(string))
		var res []any
		for i, d := range in["documents"].([]any) {
			n := 0
			for _, w := range q {
				if strings.Contains(d.(string), w) {
					n++
				}
			}
			res = append(res, map[string]any{"index": i, "relevance_score": float64(n)})
		}
		out["results"] = res
	case len(parts) == 3 && parts[2] == "exists":
		_, ok := f.pts[parts[1]]
		out["result"] = map[string]any{"exists": ok}
	case len(parts) == 2 && r.Method == http.MethodPut:
		f.pts[parts[1]] = map[string]Point{}
	case len(parts) == 4 && parts[3] == "delete":
		src := in["filter"].(map[string]any)["must"].([]any)[0].(map[string]any)["match"].(map[string]any)["value"]
		for id, p := range f.pts[parts[1]] {
			if p.Payload["source"] == src {
				delete(f.pts[parts[1]], id)
			}
		}
	case len(parts) == 3 && parts[2] == "points" && r.Method == http.MethodPut:
		b, _ := json.Marshal(in["points"])
		var pts []Point
		json.Unmarshal(b, &pts)
		for _, p := range pts {
			f.pts[parts[1]][p.ID] = p
		}
	case len(parts) == 4 && parts[3] == "search":
		b, _ := json.Marshal(in["vector"])
		var q []float32
		json.Unmarshal(b, &q)
		var hits []Hit
		for _, p := range f.pts[parts[1]] {
			hits = append(hits, Hit{Score: cos(q, p.Vector), Payload: p.Payload})
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		hits = hits[:min(int(in["limit"].(float64)), len(hits))]
		out["result"] = hits
	default:
		http.NotFound(rw, r)
		return
	}
	json.NewEncoder(rw).Encode(out)
}

func TestIndexReplacesSourceAndSearchReranks(t *testing.T) {
	f := &fakeServices{pts: map[string]map[string]Point{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	r := &Retriever{E: NewEmbedder(srv.URL, "nomic-embed"), S: NewStore(srv.URL), ChunkSize: 40, Overlap: 8}

	if _, err := r.Index(ctx, "c", "a.md", "zebra zebra zebra.\n\napple banana cherry.\n\nquokka quokka.", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.seen[0], "search_document: ") {
		t.Fatalf("doc prefix missing: %q", f.seen[0])
	}
	n1 := len(f.pts["c"])
	if _, err := r.Index(ctx, "c", "a.md", "zebra zebra zebra.", nil); err != nil {
		t.Fatal(err)
	}
	if len(f.pts["c"]) >= n1 || len(f.pts["c"]) == 0 {
		t.Fatalf("re-index should replace the source's chunks: %d then %d", n1, len(f.pts["c"]))
	}
	r.Index(ctx, "c", "b.md", "apple banana cherry.\n\nzebra apple.", nil)

	hits, err := r.Search(ctx, "c", "zebra", 1)
	if err != nil || len(hits) != 1 || !strings.Contains(hits[0].Text(), "zebra") {
		t.Fatalf("vector search: %v %+v", err, hits)
	}
	if q := f.seen[len(f.seen)-1]; q != "search_query: zebra" {
		t.Fatalf("query prefix: %q", q)
	}
	r.R = NewReranker(srv.URL, "rr")
	hits, err = r.Search(ctx, "c", "banana cherry", 1)
	if err != nil || len(hits) != 1 || hits[0].Source() != "b.md" && !strings.Contains(hits[0].Text(), "banana") {
		t.Fatalf("reranked search: %v %+v", err, hits)
	}
}
