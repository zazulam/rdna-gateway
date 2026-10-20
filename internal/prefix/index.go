// Package prefix is an approximate radix index of prompt prefixes per worker,
// in the spirit of SGLang's cache-aware router.
//
// The gateway never sees the workers' KV caches, so it keeps its own guess:
// every prompt it routes is cut into fixed-size blocks, and each block's hash is
// chained with its predecessor's, so one hash names the whole prefix up to that
// block. A worker that served a prompt is assumed to still hold those prefixes
// in its cache until they age out of the (bounded, LRU) index.
package prefix

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

// Index maps chained prefix hashes to the workers believed to have them cached.
// It is safe for concurrent use.
type Index struct {
	mu        sync.Mutex
	blockSize int
	maxNodes  int
	nodes     map[uint64]*node
	lru       *list.List // front = most recently touched
	now       func() time.Time
}

type node struct {
	key     uint64
	workers map[string]time.Time
	elem    *list.Element
}

// New returns an index cutting prompts into blockSize-byte blocks and holding
// at most maxNodes prefix nodes (oldest evicted first).
func New(blockSize, maxNodes int) *Index {
	if blockSize <= 0 {
		blockSize = 512
	}
	if maxNodes <= 0 {
		maxNodes = 200_000
	}
	return &Index{
		blockSize: blockSize,
		maxNodes:  maxNodes,
		nodes:     make(map[uint64]*node),
		lru:       list.New(),
		now:       time.Now,
	}
}

// BlockSize is the block length in bytes.
func (ix *Index) BlockSize() int { return ix.blockSize }

// Chain hashes the full blocks of text, each hash covering everything before it.
// A trailing partial block is ignored: it isn't a stable boundary yet, and the
// next turn of the same conversation will extend it into a full one.
func (ix *Index) Chain(text string) []uint64 {
	n := len(text) / ix.blockSize
	out := make([]uint64, 0, n)
	var prev [8]byte
	for i := 0; i < n; i++ {
		h := fnv.New64a()
		h.Write(prev[:])
		h.Write([]byte(text[i*ix.blockSize : (i+1)*ix.blockSize]))
		sum := h.Sum64()
		out = append(out, sum)
		for b := 0; b < 8; b++ {
			prev[b] = byte(sum >> (8 * b))
		}
	}
	return out
}

// Match returns, per worker, how many leading blocks of chain it is believed to
// hold. Workers with no match are absent.
func (ix *Index) Match(chain []uint64) map[string]int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make(map[string]int)
	for i, key := range chain {
		nd, ok := ix.nodes[key]
		if !ok {
			break // no deeper prefix can be present for anyone
		}
		for w := range nd.workers {
			// Count only contiguous matches: a worker must hold every block so far.
			if out[w] == i {
				out[w] = i + 1
			}
		}
	}
	return out
}

// Insert records that worker now holds every prefix in chain.
func (ix *Index) Insert(chain []uint64, worker string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	t := ix.now()
	for _, key := range chain {
		nd, ok := ix.nodes[key]
		if !ok {
			nd = &node{key: key, workers: make(map[string]time.Time, 1)}
			nd.elem = ix.lru.PushFront(nd)
			ix.nodes[key] = nd
		} else {
			ix.lru.MoveToFront(nd.elem)
		}
		nd.workers[worker] = t
	}
	for len(ix.nodes) > ix.maxNodes {
		back := ix.lru.Back()
		nd := back.Value.(*node)
		ix.lru.Remove(back)
		delete(ix.nodes, nd.key)
	}
}

// ForgetWorker drops every prefix attributed to worker — used when a worker
// restarts or reloads, since its KV cache is gone.
func (ix *Index) ForgetWorker(worker string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for key, nd := range ix.nodes {
		delete(nd.workers, worker)
		if len(nd.workers) == 0 {
			ix.lru.Remove(nd.elem)
			delete(ix.nodes, key)
		}
	}
}

// Len is the number of prefix nodes held.
func (ix *Index) Len() int {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return len(ix.nodes)
}
