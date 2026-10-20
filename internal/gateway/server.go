// Package gateway is an OpenAI-compatible front door for a pool of llama.cpp
// workers: it keeps a prefix index of what each worker has cached and routes
// each request to the worker most likely to skip re-processing its prompt.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zazulam/rdna-gateway/internal/prefix"
)

const maxBody = 64 << 20

// Gateway is the HTTP handler plus its background health checker.
type Gateway struct {
	cfg    *Config
	router *Router
	index  *prefix.Index
	client *http.Client
	logf   func(string, ...any)

	routed map[string]*atomic.Int64 // by Decision.Reason
}

// New builds a gateway from a normalized config.
func New(cfg *Config) (*Gateway, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	ix := prefix.New(cfg.BlockChars, cfg.MaxNodes)
	r := &Router{index: ix, minMatchBlocks: cfg.MinMatchBlocks}
	for _, wc := range cfg.Workers {
		r.workers = append(r.workers, newWorker(wc))
	}
	g := &Gateway{
		cfg:    cfg,
		router: r,
		index:  ix,
		// No overall timeout: generations stream for minutes. Dial/headers are bounded.
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			// llama-server (cpp-httplib) drops idle keep-alive connections after
			// 5s; retire ours first so a POST never lands on a closing socket.
			IdleConnTimeout:       4 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute, // long prompts are processed before headers
		}},
		logf:   log.Printf,
		routed: map[string]*atomic.Int64{},
	}
	for _, k := range []string{"prefix", "least-loaded", "round-robin", "overflow", "retry", "cost", "fallback"} {
		g.routed[k] = new(atomic.Int64)
	}
	return g, nil
}

// Run polls worker health until ctx is done. Call once, in a goroutine.
func (g *Gateway) Run(ctx context.Context) {
	g.checkAll(ctx)
	t := time.NewTicker(time.Duration(g.cfg.HealthInterval))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.checkAll(ctx)
		}
	}
}

func (g *Gateway) checkAll(ctx context.Context) {
	for _, w := range g.router.workers {
		go g.check(ctx, w)
	}
}

func (g *Gateway) check(ctx context.Context, w *Worker) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ok := g.get(ctx, w.URL+"/health", nil) == nil
	if ok && len(w.WorkerConfig.Models) == 0 {
		var lm struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if g.get(ctx, w.URL+"/v1/models", &lm) == nil {
			var ids []string
			for _, d := range lm.Data {
				ids = append(ids, d.ID)
			}
			w.setModels(ids)
		}
	}
	was := w.healthy.Swap(ok)
	if was != ok {
		if ok {
			// A worker coming back has (most likely) restarted with an empty cache.
			g.index.ForgetWorker(w.Name)
			g.logf("worker %s up (%v)", w.Name, w.Models())
		} else {
			g.logf("worker %s down", w.Name)
		}
	}
}

func (g *Gateway) get(ctx context.Context, url string, into any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	if into == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// ServeHTTP routes /v1/* to workers and serves /health and /metrics itself.
// /metrics/workers/<name> relays that worker's own llama-server /metrics, so a
// scraper can reach workers that only listen on the gateway's host.
func (g *Gateway) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/health":
		g.serveHealth(rw)
		return
	case req.URL.Path == "/metrics":
		g.serveMetrics(rw)
		return
	case req.URL.Path == "/metrics/host":
		g.serveHostMetrics(rw, req)
		return
	case strings.HasPrefix(req.URL.Path, "/metrics/workers/"):
		g.serveWorkerMetrics(rw, req, strings.TrimPrefix(req.URL.Path, "/metrics/workers/"))
		return
	case !strings.HasPrefix(req.URL.Path, "/v1/"):
		http.NotFound(rw, req)
		return
	}
	if !g.authorized(req) {
		writeErr(rw, http.StatusUnauthorized, "invalid or missing gateway API key")
		return
	}
	switch req.URL.Path {
	case "/v1/models":
		g.serveModels(rw)
	case "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/rerank":
		if req.Method != http.MethodPost {
			writeErr(rw, http.StatusMethodNotAllowed, "POST only")
			return
		}
		g.serveCompletion(rw, req)
	default:
		writeErr(rw, http.StatusNotFound, "unsupported endpoint "+req.URL.Path)
	}
}

func (g *Gateway) authorized(req *http.Request) bool {
	if g.cfg.APIKey == "" {
		return true
	}
	got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(g.cfg.APIKey)) == 1
}

func (g *Gateway) serveCompletion(rw http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		writeErr(rw, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeErr(rw, http.StatusBadRequest, "body is not JSON: "+err.Error())
		return
	}
	model, _ := parsed["model"].(string)
	policy := g.cfg.Policy
	if h := req.Header.Get("X-Gateway-Policy"); h != "" {
		if p, err := parsePolicy(h); err == nil {
			policy = p
		}
	}
	// Embeddings and rerank requests share no prefix state worth tracking.
	var text string
	var chain []uint64
	if strings.HasSuffix(req.URL.Path, "completions") {
		text = PromptText(parsed)
		chain = g.index.Chain(text)
	} else {
		policy = PolicyLeastLoaded
	}

	exclude := map[*Worker]bool{}
	var again *Worker // retry this worker once (connection dropped, not down)
	for attempt := 0; ; attempt++ {
		var d Decision
		if again != nil {
			d = Decision{Worker: again}
		} else {
			d, err = g.router.Pick(policy, model, chain, len(text), exclude)
			if err != nil {
				writeErr(rw, http.StatusServiceUnavailable, err.Error())
				return
			}
		}
		if attempt > 0 {
			d.Reason = "retry"
		}
		retry, err := g.forward(rw, req, body, chain, len(text), d)
		if err == nil || retry == noRetry {
			if err != nil {
				g.logf("forward to %s: %v", d.Worker.Name, err)
			}
			return
		}
		if retry == retrySame && again == nil {
			g.logf("worker %s dropped the connection (%v); retrying it", d.Worker.Name, err)
			again = d.Worker
			continue
		}
		g.logf("worker %s failed before responding (%v); trying another", d.Worker.Name, err)
		again = nil
		exclude[d.Worker] = true
	}
}

type retryKind int

const (
	noRetry    retryKind = iota // response already started, or client gone
	retrySame                   // connection dropped mid-handshake: worker is likely fine
	retryOther                  // worker unreachable or not ready
)

// classify decides how to retry a transport error that happened before any
// response bytes were written.
func classify(err error) retryKind {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return retryOther
	}
	// net/http doesn't export errServerClosedIdle; it means the worker closed a
	// pooled keep-alive connection as we reused it — the worker itself is fine.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		strings.Contains(err.Error(), "server closed idle connection") {
		return retrySame
	}
	return retryOther
}

// forward proxies one request to d.Worker, streaming the response back.
// retry=true means nothing was written to rw yet, so another worker may be tried.
func (g *Gateway) forward(rw http.ResponseWriter, req *http.Request, body []byte, chain []uint64, promptChars int, d Decision) (retry retryKind, err error) {
	w := d.Worker
	started := time.Now()
	w.inflight.Add(1)
	defer w.inflight.Add(-1)
	w.requests.Add(1)

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	up, _ := http.NewRequestWithContext(ctx, http.MethodPost, w.URL+req.URL.Path, bytes.NewReader(body))
	up.Header.Set("Content-Type", "application/json")
	if a := req.Header.Get("Accept"); a != "" {
		up.Header.Set("Accept", a)
	}
	resp, err := g.client.Do(up)
	if err != nil {
		w.failures.Add(1)
		if req.Context().Err() != nil {
			return noRetry, err // client went away; don't blame the worker
		}
		kind := classify(err)
		if kind == retryOther {
			w.healthy.Store(false) // the health loop brings it back
		}
		return kind, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		// llama-server answers 503 while (re)loading a model.
		w.failures.Add(1)
		io.Copy(io.Discard, resp.Body)
		return retryOther, errors.New(resp.Status)
	}

	g.routed[d.Reason].Add(1)
	if resp.StatusCode == http.StatusOK {
		// Record the prefix as soon as the worker accepts it, so a burst of
		// requests sharing a system prompt converges on the same worker.
		g.index.Insert(chain, w.Name)
	}

	h := rw.Header()
	for _, k := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	h.Set("X-Gateway-Worker", w.Name)
	h.Set("X-Gateway-Route", d.Reason)
	h.Set("X-Gateway-Matched-Blocks", strconv.Itoa(d.Matched))
	rw.WriteHeader(resp.StatusCode)

	// A stream that goes silent (worker wedged, network path dropped) would
	// otherwise hold the client and a slot forever. Any bytes reset the clock.
	stall := time.Duration(g.cfg.StallTimeout)
	stalled := time.AfterFunc(stall, cancel)
	defer stalled.Stop()

	flusher, _ := rw.(http.Flusher)
	br := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		stalled.Reset(stall)
		if len(line) > 0 {
			if _, werr := rw.Write(line); werr != nil {
				return noRetry, werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			if bytes.Contains(line, []byte(`"usage"`)) {
				g.recordUsage(w, line, promptChars, time.Since(started))
			}
		}
		if rerr == io.EOF {
			return noRetry, nil
		}
		if rerr != nil {
			if ctx.Err() != nil && req.Context().Err() == nil {
				w.failures.Add(1)
				return noRetry, fmt.Errorf("stream stalled for %s: %w", stall, rerr)
			}
			return noRetry, rerr
		}
	}
}

// recordUsage pulls token counts out of a final SSE chunk or a whole JSON body.
func (g *Gateway) recordUsage(w *Worker, line []byte, promptChars int, elapsed time.Duration) {
	line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	var u struct {
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			CacheN   int64   `json:"cache_n"`
			PromptN  int64   `json:"prompt_n"`
			PromptPS float64 `json:"prompt_per_second"`
		} `json:"timings"`
	}
	if json.Unmarshal(line, &u) != nil {
		return
	}
	cached := u.Usage.PromptTokensDetails.CachedTokens
	if u.Timings != nil && u.Timings.CacheN > cached {
		cached = u.Timings.CacheN
	}
	if u.Timings != nil {
		w.observe(u.Timings.PromptN, u.Timings.PromptPS, elapsed.Seconds())
	}
	g.router.observeCPT(int64(promptChars), u.Usage.PromptTokens)
	w.promptTokens.Add(u.Usage.PromptTokens)
	w.cachedTokens.Add(cached)
	w.outputTokens.Add(u.Usage.CompletionTokens)
}

func (g *Gateway) serveModels(rw http.ResponseWriter) {
	seen := map[string]bool{}
	type model struct {
		ID      string   `json:"id"`
		Object  string   `json:"object"`
		OwnedBy string   `json:"owned_by"`
		Workers []string `json:"workers"`
	}
	byID := map[string]*model{}
	for _, w := range g.router.workers {
		if !w.healthy.Load() {
			continue
		}
		for _, m := range w.Models() {
			if !seen[m] {
				seen[m] = true
				byID[m] = &model{ID: m, Object: "model", OwnedBy: "rdna-gateway"}
			}
			byID[m].Workers = append(byID[m].Workers, w.Name)
		}
	}
	out := struct {
		Object string   `json:"object"`
		Data   []*model `json:"data"`
	}{Object: "list", Data: []*model{}}
	for _, m := range byID {
		out.Data = append(out.Data, m)
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].ID < out.Data[j].ID })
	writeJSON(rw, http.StatusOK, out)
}

func (g *Gateway) serveHealth(rw http.ResponseWriter) {
	up := 0
	for _, w := range g.router.workers {
		if w.healthy.Load() {
			up++
		}
	}
	status := http.StatusOK
	if up == 0 {
		status = http.StatusServiceUnavailable
	}
	writeJSON(rw, status, map[string]any{"workers_up": up, "workers": len(g.router.workers)})
}

func (g *Gateway) serveMetrics(rw http.ResponseWriter) {
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
	var b strings.Builder
	gauge := func(name, worker string, v any) {
		fmt.Fprintf(&b, "rdnagw_%s{worker=%q} %v\n", name, worker, v)
	}
	for _, w := range g.router.workers {
		up := 0
		if w.healthy.Load() {
			up = 1
		}
		gauge("worker_up", w.Name, up)
		gauge("worker_inflight", w.Name, w.inflight.Load())
		gauge("worker_slots", w.Name, w.Slots)
		gauge("worker_requests_total", w.Name, w.requests.Load())
		gauge("worker_failures_total", w.Name, w.failures.Load())
		gauge("worker_prompt_tokens_total", w.Name, w.promptTokens.Load())
		gauge("worker_cached_tokens_total", w.Name, w.cachedTokens.Load())
		gauge("worker_output_tokens_total", w.Name, w.outputTokens.Load())
		pp, svc := w.speeds()
		gauge("worker_learned_prefill_tps", w.Name, fmt.Sprintf("%.1f", pp))
		gauge("worker_learned_service_seconds", w.Name, fmt.Sprintf("%.2f", svc))
	}
	reasons := make([]string, 0, len(g.routed))
	for k := range g.routed {
		reasons = append(reasons, k)
	}
	sort.Strings(reasons)
	for _, k := range reasons {
		fmt.Fprintf(&b, "rdnagw_routed_total{reason=%q} %d\n", k, g.routed[k].Load())
	}
	fmt.Fprintf(&b, "rdnagw_prefix_nodes %d\n", g.index.Len())
	io.WriteString(rw, b.String())
}

func (g *Gateway) serveWorkerMetrics(rw http.ResponseWriter, req *http.Request, name string) {
	var w *Worker
	for _, c := range g.router.workers {
		if c.Name == name {
			w = c
		}
	}
	if w == nil {
		http.NotFound(rw, req)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 4*time.Second)
	defer cancel()
	up, _ := http.NewRequestWithContext(ctx, http.MethodGet, w.URL+"/metrics", nil)
	resp, err := g.client.Do(up)
	if err != nil {
		writeErr(rw, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		rw.Header().Set("Content-Type", ct)
	}
	rw.WriteHeader(resp.StatusCode)
	io.Copy(rw, resp.Body)
}

func (g *Gateway) serveHostMetrics(rw http.ResponseWriter, req *http.Request) {
	if g.cfg.HostMetrics == "" {
		http.NotFound(rw, req)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 4*time.Second)
	defer cancel()
	up, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.HostMetrics, nil)
	resp, err := g.client.Do(up)
	if err != nil {
		writeErr(rw, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		writeErr(rw, http.StatusBadGateway, fmt.Sprintf("host metrics: %s %v", resp.Status, err))
		return
	}
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
	rw.Write(RegroupMetrics(body))
}

// RegroupMetrics makes exporter output that repeats a metric's HELP/TYPE lines
// across groups (LibreHardwareMonitor does) valid Prometheus text: one HELP and
// TYPE per metric, with all its samples together, in first-seen order.
func RegroupMetrics(in []byte) []byte {
	type family struct {
		help, typ string
		samples   []string
	}
	var order []string
	fams := map[string]*family{}
	get := func(name string) *family {
		f := fams[name]
		if f == nil {
			f = &family{}
			fams[name] = f
			order = append(order, name)
		}
		return f
	}
	for _, line := range strings.Split(string(in), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "# HELP "), strings.HasPrefix(line, "# TYPE "):
			parts := strings.SplitN(line, " ", 4)
			if len(parts) < 4 {
				continue
			}
			f := get(parts[2])
			if parts[1] == "HELP" && f.help == "" {
				f.help = line
			} else if parts[1] == "TYPE" && f.typ == "" {
				f.typ = line
			}
		case strings.HasPrefix(line, "#"):
		default:
			name := line
			if i := strings.IndexAny(line, "{ "); i >= 0 {
				name = line[:i]
			}
			// "name {labels} v" -> "name{labels} v": some parsers reject the space.
			if rest := strings.TrimLeft(line[len(name):], " "); strings.HasPrefix(rest, "{") {
				line = name + rest
			}
			f := get(name)
			f.samples = append(f.samples, line)
		}
	}
	var b strings.Builder
	for _, name := range order {
		f := fams[name]
		if len(f.samples) == 0 {
			continue
		}
		for _, l := range []string{f.help, f.typ} {
			if l != "" {
				b.WriteString(l + "\n")
			}
		}
		for _, l := range f.samples {
			b.WriteString(l + "\n")
		}
	}
	return []byte(b.String())
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	json.NewEncoder(rw).Encode(v)
}

func writeErr(rw http.ResponseWriter, status int, msg string) {
	writeJSON(rw, status, map[string]any{"error": map[string]any{"message": msg, "type": "gateway_error", "code": status}})
}
