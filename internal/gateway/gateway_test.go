package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeWorker imitates llama-server: it remembers the prompts it has seen and
// reports a prefix "cache hit" for any request extending one of them.
type fakeWorker struct {
	name string
	srv  *httptest.Server

	mu    sync.Mutex
	seen  []string
	hits  int
	block chan struct{} // if non-nil, completions wait on it
}

func newFake(t *testing.T, name string) *fakeWorker {
	f := &fakeWorker{name: name}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWorker) serve(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		io.WriteString(rw, `{"status":"ok"}`)
	case "/v1/models":
		io.WriteString(rw, `{"data":[{"id":"m"}]}`)
	case "/metrics":
		fmt.Fprintf(rw, "llamacpp:requests_processing{worker=%q} 0\n", f.name)
	case "/v1/embeddings", "/v1/rerank":
		fmt.Fprintf(rw, `{"worker":%q,"path":%q,"usage":{"prompt_tokens":4}}`, f.name, r.URL.Path)
	case "/v1/chat/completions":
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		blk := f.block
		f.mu.Unlock()
		if blk != nil {
			<-blk
		}
		text := PromptText(req)
		f.mu.Lock()
		cached := 0
		for _, s := range f.seen {
			if strings.HasPrefix(text, s) && len(s) > cached {
				cached = len(s)
			}
		}
		if cached > 0 {
			f.hits++
		}
		f.seen = append(f.seen, text)
		f.mu.Unlock()
		rw.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"hi from %s\"}}]}\n\n", f.name)
		fmt.Fprintf(rw, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":3,\"prompt_tokens_details\":{\"cached_tokens\":%d}}}\n\n", len(text), cached)
		io.WriteString(rw, "data: [DONE]\n\n")
	default:
		http.NotFound(rw, r)
	}
}

func newTestGateway(t *testing.T, cfgMod func(*Config), fakes ...*fakeWorker) (*Gateway, *httptest.Server) {
	cfg := &Config{BlockChars: 64, Policy: PolicyCacheAware}
	for _, f := range fakes {
		cfg.Workers = append(cfg.Workers, WorkerConfig{Name: f.name, URL: f.srv.URL, Slots: 2})
	}
	if cfgMod != nil {
		cfgMod(cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.logf = t.Logf
	g.checkAllSync(context.Background())
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv
}

// checkAllSync is the synchronous form of checkAll, for deterministic tests.
func (g *Gateway) checkAllSync(ctx context.Context) {
	for _, w := range g.router.workers {
		g.check(ctx, w)
	}
}

type convo struct{ msgs []map[string]any }

func newConvo(system, task string) *convo {
	return &convo{msgs: []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": task},
	}}
}

func send(t *testing.T, url string, c *convo, policy string) (worker, route, body string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": c.msgs})
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/chat/completions", bytes.NewReader(b))
	if policy != "" {
		req.Header.Set("X-Gateway-Policy", policy)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, out)
	}
	c.msgs = append(c.msgs,
		map[string]any{"role": "assistant", "content": "ok, calling a tool"},
		map[string]any{"role": "user", "content": strings.Repeat("tool output line\n", 20)},
	)
	return resp.Header.Get("X-Gateway-Worker"), resp.Header.Get("X-Gateway-Route"), string(out)
}

var system = strings.Repeat("You are a careful coding agent. ", 40)

func TestConversationSticksToItsWorker(t *testing.T) {
	a, b := newFake(t, "a"), newFake(t, "b")
	_, srv := newTestGateway(t, nil, a, b)

	// Two agents with distinct first turns (distinct prefixes) land on
	// different workers via least-loaded, then each stays put.
	c1 := newConvo("agent one: "+system, "task 1")
	c2 := newConvo("agent two: "+system, "task 2")
	w1, _, _ := send(t, srv.URL, c1, "")
	w2, _, _ := send(t, srv.URL, c2, "")
	if w1 == w2 {
		t.Fatalf("cold conversations should spread, both went to %s", w1)
	}
	for turn := 0; turn < 5; turn++ {
		g1, r1, _ := send(t, srv.URL, c1, "")
		g2, r2, _ := send(t, srv.URL, c2, "")
		if g1 != w1 || g2 != w2 || r1 != "prefix" || r2 != "prefix" {
			t.Fatalf("turn %d: c1 %s(%s) want %s, c2 %s(%s) want %s", turn, g1, r1, w1, g2, r2, w2)
		}
	}
	if a.hits+b.hits != 10 {
		t.Fatalf("every follow-up turn should hit cache, got %d", a.hits+b.hits)
	}
}

func TestRoundRobinLosesCacheHits(t *testing.T) {
	a, b := newFake(t, "a"), newFake(t, "b")
	_, srv := newTestGateway(t, nil, a, b)
	c := newConvo(system, "task")
	var got []string
	for i := 0; i < 4; i++ {
		w, r, _ := send(t, srv.URL, c, PolicyRoundRobin)
		if r != "round-robin" {
			t.Fatalf("route %s", r)
		}
		got = append(got, w)
	}
	if got[0] == got[1] || got[0] != got[2] {
		t.Fatalf("round-robin should alternate: %v", got)
	}
	// Each worker sees every other turn, so it still extends its own last
	// prompt, but the baseline cache-aware run gets strictly more hits.
	if a.hits+b.hits >= 3 {
		t.Fatalf("round-robin hits %d, expected fewer than cache-aware", a.hits+b.hits)
	}
}

func TestOverflowWhenWarmWorkerIsFull(t *testing.T) {
	a, b := newFake(t, "a"), newFake(t, "b")
	g, srv := newTestGateway(t, func(c *Config) {
		for i := range c.Workers {
			c.Workers[i].Slots = 1
		}
	}, a, b)
	c := newConvo(system, "task")
	warm, _, _ := send(t, srv.URL, c, "")
	warmFake := map[string]*fakeWorker{"a": a, "b": b}[warm]

	// Occupy the warm worker's only slot.
	gate := make(chan struct{})
	warmFake.mu.Lock()
	warmFake.block = gate
	warmFake.mu.Unlock()
	held := &convo{msgs: append([]map[string]any(nil), c.msgs...)}
	done := make(chan struct{})
	go func() { defer close(done); send(t, srv.URL, held, "") }()
	waitFor(t, func() bool { return g.router.workers[idx(warm)].inflight.Load() == 1 })

	w, r, _ := send(t, srv.URL, c, "")
	if w == warm || r != "overflow" {
		t.Fatalf("expected overflow off %s, got %s (%s)", warm, w, r)
	}
	warmFake.mu.Lock()
	warmFake.block = nil
	warmFake.mu.Unlock()
	close(gate)
	<-done
}

func idx(name string) int {
	if name == "a" {
		return 0
	}
	return 1
}

func TestFailoverToHealthyWorker(t *testing.T) {
	a, b := newFake(t, "a"), newFake(t, "b")
	g, srv := newTestGateway(t, nil, a, b)
	a.srv.Close() // dies after the health check said it was fine

	for i := 0; i < 3; i++ {
		w, _, body := send(t, srv.URL, newConvo(fmt.Sprint(i)+system, "x"), PolicyRoundRobin)
		if w != "b" || !strings.Contains(body, "hi from b") {
			t.Fatalf("request %d went to %s: %s", i, w, body)
		}
	}
	if g.router.workers[0].healthy.Load() {
		t.Fatal("dead worker should be marked unhealthy")
	}
}

func TestMetricsCountCachedTokens(t *testing.T) {
	a := newFake(t, "a")
	_, srv := newTestGateway(t, nil, a)
	c := newConvo(system, "task")
	send(t, srv.URL, c, "")
	send(t, srv.URL, c, "")
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := string(b)
	if !strings.Contains(m, `rdnagw_worker_requests_total{worker="a"} 2`) ||
		strings.Contains(m, `rdnagw_worker_cached_tokens_total{worker="a"} 0`) {
		t.Fatalf("metrics:\n%s", m)
	}
}

func TestWorkerMetricsRelay(t *testing.T) {
	a, b := newFake(t, "a"), newFake(t, "b")
	_, srv := newTestGateway(t, nil, a, b)
	resp, err := http.Get(srv.URL + "/metrics/workers/b")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `worker="b"`) {
		t.Fatalf("relay: %d %s", resp.StatusCode, body)
	}
	resp, _ = http.Get(srv.URL + "/metrics/workers/nope")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown worker: want 404, got %d", resp.StatusCode)
	}
}

func TestEmbeddingsAndRerankPassThrough(t *testing.T) {
	a := newFake(t, "a")
	_, srv := newTestGateway(t, nil, a)
	for _, path := range []string{"/v1/embeddings", "/v1/rerank"} {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{"model":"m","input":["x"]}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"path":"`+path+`"`) ||
			resp.Header.Get("X-Gateway-Route") != "least-loaded" {
			t.Fatalf("%s: %d %s route=%s", path, resp.StatusCode, b, resp.Header.Get("X-Gateway-Route"))
		}
	}
}

func TestRegroupMetrics(t *testing.T) {
	in := "# TYPE a gauge\na {\"x\"=\"1\"} 1\n# TYPE b gauge\nb 2\n# TYPE a gauge\na {\"x\"=\"2\"} 3\r\n# HELP c none\n"
	got := string(RegroupMetrics([]byte(in)))
	want := "# TYPE a gauge\na{\"x\"=\"1\"} 1\na{\"x\"=\"2\"} 3\n# TYPE b gauge\nb 2\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestHostMetricsRelay(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		io.WriteString(rw, "# TYPE t gauge\nt 1\n# TYPE t gauge\nt{s=\"b\"} 2\n")
	}))
	defer host.Close()
	a := newFake(t, "a")
	_, srv := newTestGateway(t, func(c *Config) { c.HostMetrics = host.URL }, a)
	resp, err := http.Get(srv.URL + "/metrics/host")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Count(string(b), "# TYPE t") != 1 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestModelsAndAuth(t *testing.T) {
	a := newFake(t, "a")
	_, srv := newTestGateway(t, func(c *Config) { c.APIKey = "k" }, a)

	resp, _ := http.Get(srv.URL + "/v1/models")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var lm struct {
		Data []struct{ ID string } `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&lm)
	resp.Body.Close()
	if len(lm.Data) != 1 || lm.Data[0].ID != "m" {
		t.Fatalf("models %+v", lm)
	}
}

func TestUnknownModelIsRefused(t *testing.T) {
	a := newFake(t, "a")
	_, srv := newTestGateway(t, nil, a)
	b, _ := json.Marshal(map[string]any{"model": "nope", "messages": []any{}})
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestDroppedConnectionRetriesSameWorker(t *testing.T) {
	a := newFake(t, "a")
	var drops sync.Once
	inner := a.srv.Config.Handler
	a.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			dropped := false
			drops.Do(func() {
				// Like a keep-alive socket closing under us: no response at all.
				conn, _, _ := rw.(http.Hijacker).Hijack()
				conn.Close()
				dropped = true
			})
			if dropped {
				return
			}
		}
		inner.ServeHTTP(rw, r)
	})
	g, srv := newTestGateway(t, nil, a)
	w, r, body := send(t, srv.URL, newConvo(system, "x"), "")
	if w != "a" || r != "retry" || !strings.Contains(body, "hi from a") {
		t.Fatalf("got %s (%s): %s", w, r, body)
	}
	if !g.router.workers[0].healthy.Load() {
		t.Fatal("a dropped connection must not mark the worker down")
	}
}

func TestStalledStreamIsAborted(t *testing.T) {
	a := newFake(t, "a")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	inner := a.srv.Config.Handler
	a.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			inner.ServeHTTP(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		rw.(http.Flusher).Flush()
		select { // then goes silent
		case <-release:
		case <-r.Context().Done():
		}
	})
	g, srv := newTestGateway(t, func(c *Config) { c.StallTimeout = Duration(200 * time.Millisecond) }, a)
	start := time.Now()
	_, _, body := send(t, srv.URL, newConvo(system, "x"), "")
	if !strings.Contains(body, "partial") || time.Since(start) > 3*time.Second {
		t.Fatalf("after %s: %q", time.Since(start), body)
	}
	waitFor(t, func() bool { return g.router.workers[0].inflight.Load() == 0 })
	if g.router.workers[0].failures.Load() != 1 {
		t.Fatal("a stall should count as a worker failure")
	}
}

func TestClassifyTransportErrors(t *testing.T) {
	cases := map[error]retryKind{
		io.EOF: retrySame,
		fmt.Errorf("Post x: %w", io.ErrUnexpectedEOF):        retrySame,
		errors.New("http: server closed idle connection"):    retrySame,
		&net.OpError{Op: "dial", Err: errors.New("refused")}: retryOther,
		errors.New("something else"):                         retryOther,
	}
	for err, want := range cases {
		if got := classify(err); got != want {
			t.Errorf("%v: got %d want %d", err, got, want)
		}
	}
}
