package gateway

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/zazulam/rdna-gateway/internal/prefix"
)

const (
	PolicyCacheAware  = "cache-aware"
	PolicyRoundRobin  = "round-robin"
	PolicyLeastLoaded = "least-loaded"
	PolicyCostAware   = "cost-aware"
)

func parsePolicy(s string) (string, error) {
	switch s {
	case PolicyCacheAware, PolicyRoundRobin, PolicyLeastLoaded, PolicyCostAware:
		return s, nil
	}
	return "", fmt.Errorf("unknown policy %q (want cost-aware, cache-aware, round-robin or least-loaded)", s)
}

// Worker is a backend's live state.
type Worker struct {
	WorkerConfig
	inflight atomic.Int64
	healthy  atomic.Bool

	mu     sync.Mutex
	models []string // configured, or discovered from /v1/models

	// Counters for /metrics.
	requests     atomic.Int64
	failures     atomic.Int64
	promptTokens atomic.Int64
	cachedTokens atomic.Int64
	outputTokens atomic.Int64

	// Learned speeds (EWMA), guarded by mu. Seeded from Weight until observed.
	prefillTPS float64 // uncached prompt tokens/s, as measured under real load
	serviceS   float64 // seconds a request occupies a slot
}

// ewmaAlpha weights each new observation; ~10 requests to adapt.
const ewmaAlpha = 0.2

func ewma(old, v float64) float64 {
	if old == 0 {
		return v
	}
	return old + ewmaAlpha*(v-old)
}

func newWorker(c WorkerConfig) *Worker {
	w := &Worker{WorkerConfig: c, models: append([]string(nil), c.Models...),
		prefillTPS: 1000 * c.Weight, serviceS: 5}
	// Optimistic until the first health check says otherwise, so requests that
	// arrive right at startup aren't refused.
	w.healthy.Store(true)
	return w
}

// load is in-flight requests per unit of weighted capacity.
func (w *Worker) load() float64 {
	return float64(w.inflight.Load()) / (float64(w.Slots) * w.Weight)
}

func (w *Worker) hasFreeSlot() bool { return w.inflight.Load() < int64(w.Slots) }

// observe folds one finished request's timings into the learned speeds.
func (w *Worker) observe(prefillTokens int64, prefillTPS, serviceS float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if prefillTokens >= 64 && prefillTPS > 0 { // tiny prefills give noisy rates
		w.prefillTPS = ewma(w.prefillTPS, prefillTPS)
	}
	if serviceS > 0 {
		w.serviceS = ewma(w.serviceS, serviceS)
	}
}

func (w *Worker) speeds() (prefillTPS, serviceS float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prefillTPS, w.serviceS
}

// estimate is the expected seconds until this worker starts generating a
// prompt of promptTok tokens of which cachedTok are already in its KV cache:
// queueing behind full slots, then prefilling the uncached remainder.
func (w *Worker) estimate(promptTok, cachedTok float64) float64 {
	pp, svc := w.speeds()
	wait := 0.0
	if q := w.inflight.Load() - int64(w.Slots) + 1; q > 0 {
		wait = float64(q) * svc / float64(w.Slots)
	}
	return wait + math.Max(0, promptTok-cachedTok)/pp
}

func (w *Worker) Models() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.models...)
}

func (w *Worker) setModels(m []string) {
	w.mu.Lock()
	w.models = m
	w.mu.Unlock()
}

func (w *Worker) serves(model string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.models) == 0 {
		return true // unknown yet: let it try rather than refuse
	}
	for _, m := range w.models {
		if m == model {
			return true
		}
	}
	return false
}

// Router picks a worker for each request.
type Router struct {
	workers        []*Worker
	index          *prefix.Index
	minMatchBlocks int
	rr             atomic.Uint64

	cptMu sync.Mutex
	cpt   float64 // prompt bytes per token, learned from usage
}

// observeCPT learns how many prompt bytes make a token for the served model mix.
func (r *Router) observeCPT(chars, tokens int64) {
	if chars <= 0 || tokens <= 0 {
		return
	}
	r.cptMu.Lock()
	r.cpt = ewma(r.cpt, float64(chars)/float64(tokens))
	r.cptMu.Unlock()
}

func (r *Router) charsPerToken() float64 {
	r.cptMu.Lock()
	defer r.cptMu.Unlock()
	if r.cpt == 0 {
		return 3.5
	}
	return r.cpt
}

// Decision explains a routing choice (surfaced in X-Gateway-* response headers).
type Decision struct {
	Worker  *Worker
	Matched int    // prefix blocks believed cached on Worker
	Reason  string // "prefix", "least-loaded", "round-robin", "overflow", "cost", "fallback"
}

// candidates returns healthy workers that serve model, in config order.
// Fallback workers are only offered when no primary one is available.
func (r *Router) candidates(model string, exclude map[*Worker]bool) (out []*Worker, fallback bool) {
	for _, tier := range []bool{false, true} {
		for _, w := range r.workers {
			if w.Fallback == tier && w.healthy.Load() && !exclude[w] && w.serves(model) {
				out = append(out, w)
			}
		}
		if len(out) > 0 {
			return out, tier
		}
	}
	return nil, false
}

// Pick chooses a worker for a prompt whose prefix chain is chain.
//
// cache-aware: go to the worker holding the longest cached prefix, unless it
// has no free slot while another candidate does — then overflow to the least
// loaded one (a queued request on a warm worker loses to an idle cold one once
// prompts are processed at >1k tok/s). Short or unmatched prompts go least-loaded.
//
// cost-aware: estimate, per worker, seconds until the first token — queueing
// behind full slots plus prefilling whatever isn't cached there, from speeds
// learned off the workers' own timings — and take the cheapest.
func (r *Router) Pick(policy, model string, chain []uint64, promptChars int, exclude map[*Worker]bool) (Decision, error) {
	cands, fallback := r.candidates(model, exclude)
	if len(cands) == 0 {
		return Decision{}, fmt.Errorf("no healthy worker serves model %q", model)
	}
	d, err := r.pick(policy, cands, chain, promptChars)
	if fallback {
		d.Reason = "fallback"
	}
	return d, err
}

func (r *Router) pick(policy string, cands []*Worker, chain []uint64, promptChars int) (Decision, error) {
	switch policy {
	case PolicyCostAware:
		return r.cheapest(cands, chain, promptChars), nil
	case PolicyRoundRobin:
		i := r.rr.Add(1) - 1
		return Decision{Worker: cands[i%uint64(len(cands))], Reason: "round-robin"}, nil
	case PolicyLeastLoaded:
		matches := r.index.Match(chain)
		w := r.leastLoadedPreferring(cands, matches)
		return Decision{Worker: w, Matched: matches[w.Name], Reason: "least-loaded"}, nil
	}

	matches := r.index.Match(chain)
	ranked := append([]*Worker(nil), cands...)
	sort.SliceStable(ranked, func(i, j int) bool {
		mi, mj := matches[ranked[i].Name], matches[ranked[j].Name]
		if mi != mj {
			return mi > mj
		}
		return ranked[i].load() < ranked[j].load()
	})
	best := ranked[0]
	m := matches[best.Name]
	if m < r.minMatchBlocks {
		return Decision{Worker: r.leastLoaded(cands), Reason: "least-loaded"}, nil
	}
	if !best.hasFreeSlot() {
		for _, w := range cands {
			if w.hasFreeSlot() {
				ll := r.leastLoaded(cands)
				return Decision{Worker: ll, Matched: matches[ll.Name], Reason: "overflow"}, nil
			}
		}
	}
	return Decision{Worker: best, Matched: m, Reason: "prefix"}, nil
}

func (r *Router) cheapest(cands []*Worker, chain []uint64, promptChars int) Decision {
	matches := r.index.Match(chain)
	cpt := r.charsPerToken()
	block := float64(r.index.BlockSize())
	promptTok := float64(promptChars) / cpt
	start := int(r.rr.Add(1) % uint64(len(cands))) // rotate ties
	var best Decision
	bestCost := math.Inf(1)
	for i := range cands {
		w := cands[(start+i)%len(cands)]
		m := matches[w.Name]
		cost := w.estimate(promptTok, float64(m)*block/cpt)
		if cost < bestCost-1e-9 {
			best, bestCost = Decision{Worker: w, Matched: m, Reason: "cost"}, cost
		}
	}
	return best
}

// leastLoadedPreferring is leastLoaded, except that among workers tied for the
// lowest load it picks the one already holding the longest cached prefix of
// this prompt. Without it, an idle pool alternates a chat's turns between
// workers and every turn re-processes the whole prompt. Load still wins when
// it differs, so busy pools keep spreading.
func (r *Router) leastLoadedPreferring(ws []*Worker, matches map[string]int) *Worker {
	best := r.leastLoaded(ws)
	min := best.load()
	for _, w := range ws {
		if w.load() == min && matches[w.Name] >= r.minMatchBlocks && matches[w.Name] > matches[best.Name] {
			best = w
		}
	}
	return best
}

// leastLoaded picks the lowest-load worker, rotating the starting point so
// ties (e.g. an idle pool) spread cold prompts instead of piling onto the first.
func (r *Router) leastLoaded(ws []*Worker) *Worker {
	start := int(r.rr.Add(1) % uint64(len(ws)))
	best := ws[start]
	for i := 1; i < len(ws); i++ {
		w := ws[(start+i)%len(ws)]
		if w.load() < best.load() {
			best = w
		}
	}
	return best
}
