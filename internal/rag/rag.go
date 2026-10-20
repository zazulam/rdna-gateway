package rag

import (
	"context"
	"crypto/sha1"
	"fmt"
	"sort"
	"time"
)

// Retriever ties an embedder, an optional reranker and a store together.
type Retriever struct {
	E *Embedder
	R *Reranker // nil: vector search only
	S *Store
	// ChunkSize/Overlap in bytes (defaults 1200/200). Candidates is how many
	// vector hits the reranker rescores (default 30).
	ChunkSize, Overlap, Candidates int
}

func (r *Retriever) chunking() (int, int) {
	size, overlap := r.ChunkSize, r.Overlap
	if size == 0 {
		size = 1200
	}
	if overlap == 0 {
		overlap = 200
	}
	return size, overlap
}

// pointID is stable per (collection, source, chunk), so re-indexing a source
// overwrites its chunks instead of duplicating them.
func pointID(collection, source string, i int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s\x00%s\x00%d", collection, source, i)))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// Index replaces source's chunks in collection. extra is merged into every
// chunk's payload. It returns the number of chunks stored.
func (r *Retriever) Index(ctx context.Context, collection, source, text string, extra map[string]any) (int, error) {
	size, overlap := r.chunking()
	chunks := Chunk(text, size, overlap)
	if len(chunks) == 0 {
		return 0, nil
	}
	vecs, err := r.E.Embed(ctx, chunks, false)
	if err != nil {
		return 0, err
	}
	if err := r.S.Ensure(ctx, collection, len(vecs[0])); err != nil {
		return 0, err
	}
	if err := r.S.DeleteSource(ctx, collection, source); err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	pts := make([]Point, len(chunks))
	for i, c := range chunks {
		p := map[string]any{"source": source, "chunk": i, "text": c, "indexed_at": now}
		for k, v := range extra {
			p[k] = v
		}
		pts[i] = Point{ID: pointID(collection, source, i), Vector: vecs[i], Payload: p}
	}
	return len(pts), r.S.Upsert(ctx, collection, pts)
}

// Search returns the k best chunks for query: vector search, then (with a
// reranker) rescoring of the top Candidates by the cross-encoder.
func (r *Retriever) Search(ctx context.Context, collection, query string, k int) ([]Hit, error) {
	vecs, err := r.E.Embed(ctx, []string{query}, true)
	if err != nil {
		return nil, err
	}
	n := k
	if r.R != nil {
		n = r.Candidates
		if n == 0 {
			n = 30
		}
		n = max(n, k)
	}
	hits, err := r.S.Search(ctx, collection, vecs[0], n)
	if err != nil || r.R == nil || len(hits) == 0 {
		return hits[:min(k, len(hits))], err
	}
	docs := make([]string, len(hits))
	for i, h := range hits {
		docs[i] = h.Text()
	}
	scores, err := r.R.Scores(ctx, query, docs)
	if err != nil {
		return nil, err
	}
	for i := range hits {
		hits[i].Score = scores[i]
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits[:min(k, len(hits))], nil
}
