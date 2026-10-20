package gateway

import (
	"strings"
	"testing"

	"github.com/zazulam/rdna-gateway/internal/prefix"
)

func testRouter(ws ...WorkerConfig) *Router {
	r := &Router{index: prefix.New(100, 1000), minMatchBlocks: 1}
	for _, c := range ws {
		if c.Weight == 0 {
			c.Weight = 1
		}
		r.workers = append(r.workers, newWorker(c))
	}
	return r
}

func setSpeeds(w *Worker, pp, svc float64) {
	w.mu.Lock()
	w.prefillTPS, w.serviceS = pp, svc
	w.mu.Unlock()
}

func TestCostAwarePrefersWarmWorker(t *testing.T) {
	r := testRouter(WorkerConfig{Name: "a", Slots: 2}, WorkerConfig{Name: "b", Slots: 2})
	text := strings.Repeat("x", 35_000) // 10k tokens at 3.5 bytes/token
	chain := r.index.Chain(text)
	r.index.Insert(chain, "b")
	for i := 0; i < 5; i++ {
		d, _ := r.Pick(PolicyCostAware, "m", chain, len(text), nil)
		if d.Worker.Name != "b" || d.Reason != "cost" {
			t.Fatalf("want warm b, got %s (%s)", d.Worker.Name, d.Reason)
		}
	}
}

func TestCostAwareLeavesQueuedWarmWorkerForShortPrompt(t *testing.T) {
	r := testRouter(WorkerConfig{Name: "a", Slots: 2}, WorkerConfig{Name: "b", Slots: 2})
	a, b := r.workers[0], r.workers[1]
	setSpeeds(a, 1000, 10)
	setSpeeds(b, 1000, 10)
	text := strings.Repeat("y", 3_500) // ~1k tokens: 1s to prefill cold
	chain := r.index.Chain(text)
	r.index.Insert(chain, "b")
	b.inflight.Store(4) // two queued behind full slots: ~15s wait
	d, _ := r.Pick(PolicyCostAware, "m", chain, len(text), nil)
	if d.Worker != a {
		t.Fatalf("a 1s cold prefill beats a 15s queue; got %s", d.Worker.Name)
	}
}

func TestCostAwareWaitsForWarmWorkerWhenPrefillIsExpensive(t *testing.T) {
	r := testRouter(WorkerConfig{Name: "a", Slots: 2}, WorkerConfig{Name: "b", Slots: 2})
	a, b := r.workers[0], r.workers[1]
	setSpeeds(a, 300, 4) // slow card
	setSpeeds(b, 1200, 4)
	text := strings.Repeat("z", 70_000) // ~20k tokens: ~67s on a, cached on b
	chain := r.index.Chain(text)
	r.index.Insert(chain, "b")
	b.inflight.Store(3) // one queued: ~4s wait
	d, _ := r.Pick(PolicyCostAware, "m", chain, len(text), nil)
	if d.Worker != b {
		t.Fatalf("a short queue on the warm worker beats a long cold prefill; got %s", d.Worker.Name)
	}
	// cache-aware, by contrast, overflows as soon as b has no free slot.
	d, _ = r.Pick(PolicyCacheAware, "m", chain, len(text), nil)
	if d.Worker != a || d.Reason != "overflow" {
		t.Fatalf("cache-aware should overflow; got %s (%s)", d.Worker.Name, d.Reason)
	}
}

func TestLearnedSpeedsMoveTowardObservations(t *testing.T) {
	w := newWorker(WorkerConfig{Name: "a", Slots: 1, Weight: 1})
	for i := 0; i < 30; i++ {
		w.observe(500, 400, 2)
	}
	pp, svc := w.speeds()
	if pp < 390 || pp > 410 || svc < 1.95 || svc > 2.05 {
		t.Fatalf("pp %.1f svc %.2f", pp, svc)
	}
	w.observe(10, 99999, 0) // tiny prefill: ignored
	if p2, _ := w.speeds(); p2 != pp {
		t.Fatalf("tiny prefill moved the estimate: %.1f -> %.1f", pp, p2)
	}
}

func TestFallbackOnlyWhenPrimariesDown(t *testing.T) {
	r := testRouter(WorkerConfig{Name: "gpu", Slots: 4}, WorkerConfig{Name: "laptop", Slots: 1, Fallback: true})
	for _, p := range []string{PolicyCostAware, PolicyCacheAware, PolicyRoundRobin, PolicyLeastLoaded} {
		r.workers[0].inflight.Store(40) // even badly overloaded, the primary wins
		d, _ := r.Pick(p, "m", nil, 100, nil)
		if d.Worker.Name != "gpu" {
			t.Fatalf("%s: fallback used while primary up", p)
		}
	}
	r.workers[0].healthy.Store(false)
	d, err := r.Pick(PolicyCostAware, "m", nil, 100, nil)
	if err != nil || d.Worker.Name != "laptop" || d.Reason != "fallback" {
		t.Fatalf("got %+v %v", d, err)
	}
}

func TestLeastLoadedKeepsAChatOnItsWarmWorker(t *testing.T) {
	ix := prefix.New(64, 1000)
	r := &Router{index: ix, minMatchBlocks: 1}
	a, b := newWorker(WorkerConfig{Name: "a", Slots: 4, Weight: 1}), newWorker(WorkerConfig{Name: "b", Slots: 4, Weight: 1})
	a.healthy.Store(true)
	b.healthy.Store(true)
	r.workers = []*Worker{a, b}
	chain := ix.Chain(strings.Repeat("system prompt and history ", 40))

	// Cold: rotation picks some worker; remember it.
	d, err := r.Pick(PolicyLeastLoaded, "", chain, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	ix.Insert(chain, d.Worker.Name)
	for i := 0; i < 5; i++ {
		got, _ := r.Pick(PolicyLeastLoaded, "", chain, 1000, nil)
		if got.Worker != d.Worker || got.Matched == 0 {
			t.Fatalf("turn %d went to %s (matched %d), want warm %s", i, got.Worker.Name, got.Matched, d.Worker.Name)
		}
	}
	// Load still wins: a busier warm worker loses to an idle cold one.
	d.Worker.inflight.Add(1)
	if got, _ := r.Pick(PolicyLeastLoaded, "", chain, 1000, nil); got.Worker == d.Worker {
		t.Fatalf("busy warm worker %s chosen over an idle one", d.Worker.Name)
	}
}
