package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zazulam/rdna-gateway/internal/otlp"
	"github.com/zazulam/rdna-gateway/internal/otlp/otlptest"
)

// Text that must never leave the gateway in telemetry.
const (
	secretPrompt     = "PROMPT-SECRET-7f3a"
	secretToolArg    = "TOOL-ARG-SECRET-91bc"
	secretCompletion = "COMPLETION-SECRET-c0de"
	secretToolResult = "TOOL-RESULT-SECRET-5e5e"
)

// llamaStub imitates llama-server's OpenAI endpoints closely enough for the
// telemetry: a role chunk, then tokens after firstTokenDelay, then a final
// chunk with finish_reason, usage and timings. It records request headers.
type llamaStub struct {
	name            string
	srv             *httptest.Server
	firstTokenDelay time.Duration

	mu      sync.Mutex
	headers []http.Header
}

func newLlamaStub(t *testing.T, name string) *llamaStub {
	s := &llamaStub{name: name}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *llamaStub) lastHeader() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.headers) == 0 {
		return nil
	}
	return s.headers[len(s.headers)-1]
}

const stubTimings = `"usage":{"completion_tokens":5,"prompt_tokens":100,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":64}},` +
	`"timings":{"cache_n":64,"prompt_n":36,"prompt_ms":40.5,"prompt_per_second":888.8,"predicted_n":5,"predicted_ms":50,"predicted_per_second":100}`

func (s *llamaStub) serve(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		io.WriteString(rw, `{"status":"ok"}`)
		return
	case "/v1/models":
		io.WriteString(rw, `{"data":[{"id":"qwen"}]}`)
		return
	case "/v1/embeddings":
		s.record(r)
		io.WriteString(rw, `{"model":"qwen","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`+"\n")
		return
	case "/v1/chat/completions":
	default:
		http.NotFound(rw, r)
		return
	}
	s.record(r)
	var req struct {
		Stream bool `json:"stream"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if !req.Stream {
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(rw, `{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":%q}}],"created":1,"model":"qwen","object":"chat.completion","id":"chatcmpl-xyz",%s}`+"\n", secretCompletion, stubTimings)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	fl := rw.(http.Flusher)
	chunk := func(body string) {
		fmt.Fprintf(rw, "data: {\"choices\":[%s],\"created\":1,\"id\":\"chatcmpl-abc\",\"model\":\"qwen\",\"object\":\"chat.completion.chunk\"}\n\n", body)
		fl.Flush()
	}
	chunk(`{"finish_reason":null,"index":0,"delta":{"role":"assistant","content":null}}`)
	time.Sleep(s.firstTokenDelay)
	chunk(fmt.Sprintf(`{"finish_reason":null,"index":0,"delta":{"content":%q}}`, secretCompletion))
	chunk(fmt.Sprintf(`{"finish_reason":null,"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":%q}}]}}`, `{"path":"`+secretToolResult+`"}`))
	chunk(`{"finish_reason":"tool_calls","index":0,"delta":{}}`)
	fmt.Fprintf(rw, "data: {\"choices\":[],\"created\":1,\"id\":\"chatcmpl-abc\",\"model\":\"qwen\",\"object\":\"chat.completion.chunk\",%s}\n\n", stubTimings)
	io.WriteString(rw, "data: [DONE]\n\n")
}

func (s *llamaStub) record(r *http.Request) {
	s.mu.Lock()
	s.headers = append(s.headers, r.Header.Clone())
	s.mu.Unlock()
}

// tracedGateway starts a gateway in front of workers (stubs or plain URLs),
// exporting to an in-process OTLP receiver when rcv is non-nil.
func tracedGateway(t *testing.T, rcv *otlptest.Receiver, cfgMod func(*Config), urls map[string]string) (*Gateway, *httptest.Server, *otlp.Exporter) {
	t.Helper()
	cfg := &Config{Policy: PolicyLeastLoaded}
	for _, name := range []string{"a", "b", "dead"} {
		if u, ok := urls[name]; ok {
			cfg.Workers = append(cfg.Workers, WorkerConfig{Name: name, URL: u, Slots: 2, Models: []string{"qwen"}})
		}
	}
	if cfgMod != nil {
		cfgMod(cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.logf = t.Logf
	var exp *otlp.Exporter
	if rcv != nil {
		exp = otlp.NewExporter(otlp.Config{Endpoint: rcv.URL}, "test", "rdnagw", otlp.Options{Logf: t.Logf})
		t.Cleanup(func() { exp.Shutdown(context.Background()) })
	}
	g.SetExporter(exp)
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv, exp
}

func chatBody(stream bool) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": "qwen", "stream": stream, "max_tokens": 256, "temperature": 0.2,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful. " + secretPrompt},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "t1", "type": "function",
				"function": map[string]any{"name": "search", "arguments": `{"q":"` + secretToolArg + `"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "t1", "content": secretToolResult},
			map[string]any{"role": "user", "content": "go on"},
		},
	})
	return b
}

func post(t *testing.T, url, path string, body []byte, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+path, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

const clientTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// Without an exporter, the caller's trace context reaches the worker as-is.
func TestTraceContextPassesThrough(t *testing.T) {
	a := newLlamaStub(t, "a")
	_, srv, _ := tracedGateway(t, nil, nil, map[string]string{"a": a.srv.URL})

	resp, _ := post(t, srv.URL, "/v1/chat/completions", chatBody(false), map[string]string{
		"traceparent": clientTP, "tracestate": "vendor=opaque", "baggage": "team=lab"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := a.lastHeader()
	if h.Get("traceparent") != clientTP || h.Get("tracestate") != "vendor=opaque" || h.Get("baggage") != "team=lab" {
		t.Errorf("worker got traceparent=%q tracestate=%q baggage=%q", h.Get("traceparent"), h.Get("tracestate"), h.Get("baggage"))
	}
	if resp.Header.Get("traceparent") != "" {
		t.Error("no gateway span, so no traceparent response header")
	}

	// No trace context at all is fine too.
	resp, _ = post(t, srv.URL, "/v1/chat/completions", chatBody(false), nil)
	if resp.StatusCode != 200 || a.lastHeader().Get("traceparent") != "" {
		t.Errorf("status %d, traceparent %q", resp.StatusCode, a.lastHeader().Get("traceparent"))
	}
}

// With an exporter, the gateway's span sits between the caller and the
// worker: same trace, the caller's span as parent, the gateway's span as the
// worker's parent, and the response says which trace it was.
func TestGatewaySpanParentsTheWorker(t *testing.T) {
	rcv := otlptest.New(t)
	a := newLlamaStub(t, "a")
	_, srv, exp := tracedGateway(t, rcv, nil, map[string]string{"a": a.srv.URL})

	resp, _ := post(t, srv.URL, "/v1/chat/completions", chatBody(false), map[string]string{
		"traceparent": clientTP, "tracestate": "vendor=opaque"})
	exp.ForceFlush(context.Background())

	spans := rcv.Spans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || sp.ParentSpanID != "00f067aa0ba902b7" {
		t.Errorf("span trace %s parent %s", sp.TraceID, sp.ParentSpanID)
	}
	want := "00-" + sp.TraceID + "-" + sp.SpanID + "-01"
	if got := a.lastHeader().Get("traceparent"); got != want {
		t.Errorf("worker traceparent %q, want %q", got, want)
	}
	if a.lastHeader().Get("tracestate") != "vendor=opaque" {
		t.Error("tracestate should pass through")
	}
	if resp.Header.Get("traceparent") != want {
		t.Errorf("response traceparent %q, want %q", resp.Header.Get("traceparent"), want)
	}

	// A caller without trace context starts a new trace at the gateway.
	resp, _ = post(t, srv.URL, "/v1/chat/completions", chatBody(false), map[string]string{"traceparent": "garbage"})
	sc, ok := otlp.ParseTraceparent(resp.Header.Get("traceparent"))
	if !ok || sc.TraceID.String() == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("want a fresh trace, got %q", resp.Header.Get("traceparent"))
	}
}

func TestStreamingSpan(t *testing.T) {
	rcv := otlptest.New(t)
	a := newLlamaStub(t, "a")
	a.firstTokenDelay = 150 * time.Millisecond
	_, srv, exp := tracedGateway(t, rcv, nil, map[string]string{"a": a.srv.URL})

	resp, body := post(t, srv.URL, "/v1/chat/completions", chatBody(true), nil)
	if resp.StatusCode != 200 || !strings.Contains(body, secretCompletion) || !strings.Contains(body, "[DONE]") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	exp.ForceFlush(context.Background())
	spans := rcv.Spans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Name != "chat qwen" || sp.Kind != int(otlp.KindClient) || sp.StatusCode == int(otlp.StatusError) {
		t.Errorf("span %s kind %d status %d", sp.Name, sp.Kind, sp.StatusCode)
	}
	for k, v := range map[string]any{
		"gen_ai.operation.name": "chat", "gen_ai.request.model": "qwen", "gen_ai.response.model": "qwen",
		"gen_ai.response.id": "chatcmpl-abc", "gen_ai.request.max_tokens": "256", "gen_ai.request.temperature": 0.2,
		"gen_ai.usage.input_tokens": "100", "gen_ai.usage.output_tokens": "5", "gen_ai.usage.cache_read.input_tokens": "64",
		"server.address": "a", "rdnagw.route": "least-loaded", "rdnagw.matched_blocks": "0", "rdnagw.retries": "0",
		"rdnagw.outcome": "ok", "http.response.status_code": "200", "rdnagw.worker.prompt_ms": 40.5,
	} {
		if sp.Attrs[k] != v {
			t.Errorf("%s = %#v, want %#v", k, sp.Attrs[k], v)
		}
	}
	if fr, _ := sp.Attrs["gen_ai.response.finish_reasons"].([]any); len(fr) != 1 || fr[0] != "tool_calls" {
		t.Errorf("finish reasons %v", sp.Attrs["gen_ai.response.finish_reasons"])
	}
	// The role-only chunk arrives at once; the first token after the delay.
	if ttft, _ := sp.Attrs["gen_ai.server.time_to_first_token"].(float64); ttft < 0.15 || ttft > 2 {
		t.Errorf("time to first token %v", sp.Attrs["gen_ai.server.time_to_first_token"])
	}
}

func TestNonStreamingSpan(t *testing.T) {
	rcv := otlptest.New(t)
	a := newLlamaStub(t, "a")
	g, srv, exp := tracedGateway(t, rcv, nil, map[string]string{"a": a.srv.URL})

	post(t, srv.URL, "/v1/chat/completions", chatBody(false), nil)
	post(t, srv.URL, "/v1/embeddings", []byte(`{"model":"qwen","input":"`+secretPrompt+`"}`), nil)
	exp.ForceFlush(context.Background())
	spans := rcv.Spans()
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	chat, emb := spans[0], spans[1]
	if chat.Attrs["gen_ai.usage.output_tokens"] != "5" || chat.Attrs["gen_ai.response.id"] != "chatcmpl-xyz" {
		t.Errorf("chat attrs %v", chat.Attrs)
	}
	if _, ok := chat.Attrs["gen_ai.server.time_to_first_token"]; ok {
		t.Error("no time to first token without a stream")
	}
	if emb.Name != "embeddings qwen" || emb.Attrs["gen_ai.usage.input_tokens"] != "4" {
		t.Errorf("embeddings span %s %v", emb.Name, emb.Attrs)
	}
	// The counters behind /metrics still add up.
	if w := g.router.workers[0]; w.outputTokens.Load() != 5 || w.cachedTokens.Load() != 64 || w.promptTokens.Load() != 104 {
		t.Errorf("counters out=%d cached=%d prompt=%d", w.outputTokens.Load(), w.cachedTokens.Load(), w.promptTokens.Load())
	}
}

// A worker that refuses the connection becomes a retry event, and the span
// reports the worker that answered.
func TestRetryIsASpanEvent(t *testing.T) {
	rcv := otlptest.New(t)
	b := newLlamaStub(t, "b")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	_, srv, exp := tracedGateway(t, rcv, func(c *Config) { c.Policy = PolicyRoundRobin },
		map[string]string{"dead": deadURL, "b": b.srv.URL})

	for i := 0; i < 2; i++ { // round-robin: one of the two starts on the dead worker
		post(t, srv.URL, "/v1/chat/completions", chatBody(false), nil)
	}
	exp.ForceFlush(context.Background())
	var found bool
	for _, sp := range rcv.Spans() {
		if sp.Attrs["server.address"] != "b" {
			t.Errorf("answered by %v", sp.Attrs["server.address"])
		}
		if sp.Attrs["rdnagw.retries"] == "1" {
			found = true
			if len(sp.Events) != 1 || sp.Events[0].Name != "rdnagw.retry" || sp.Events[0].Attrs["server.address"] != "dead" ||
				sp.Events[0].Attrs["error.type"] != "unreachable" || sp.Attrs["rdnagw.route"] != "retry" {
				t.Errorf("retry span %+v", sp)
			}
		}
	}
	if !found {
		t.Error("no span recorded the retry")
	}
}

func TestStallMarksSpan(t *testing.T) {
	rcv := otlptest.New(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	a := newLlamaStub(t, "a")
	inner := a.srv.Config.Handler
	a.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			inner.ServeHTTP(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		rw.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	_, srv, exp := tracedGateway(t, rcv, func(c *Config) { c.StallTimeout = Duration(150 * time.Millisecond) },
		map[string]string{"a": a.srv.URL})
	post(t, srv.URL, "/v1/chat/completions", chatBody(true), nil)
	var sp otlptest.Span
	if !otlptest.WaitFor(2*time.Second, func() bool {
		exp.ForceFlush(context.Background())
		s := rcv.Spans()
		if len(s) > 0 {
			sp = s[0]
		}
		return len(s) > 0
	}) {
		t.Fatal("no span")
	}
	if sp.Attrs["error.type"] != "stalled" || sp.Attrs["rdnagw.outcome"] != "stalled" || sp.StatusCode != int(otlp.StatusError) ||
		len(sp.Events) != 1 || sp.Events[0].Name != "rdnagw.stall" {
		t.Errorf("stalled span %+v", sp)
	}
}

func TestNoWorkerIsAnError(t *testing.T) {
	rcv := otlptest.New(t)
	a := newLlamaStub(t, "a")
	_, srv, exp := tracedGateway(t, rcv, nil, map[string]string{"a": a.srv.URL})
	resp, _ := post(t, srv.URL, "/v1/chat/completions", []byte(`{"model":"nope","messages":[]}`), nil)
	exp.ForceFlush(context.Background())
	sp := rcv.Spans()
	if resp.StatusCode != 503 || len(sp) != 1 || sp[0].Attrs["error.type"] != "no_worker" {
		t.Fatalf("status %d spans %+v", resp.StatusCode, sp)
	}
}

// No prompt, completion, tool argument or tool result text ever reaches the
// exporter, streamed or not.
func TestTelemetryCarriesNoContent(t *testing.T) {
	rcv := otlptest.New(t)
	a := newLlamaStub(t, "a")
	_, srv, exp := tracedGateway(t, rcv, nil, map[string]string{"a": a.srv.URL})
	post(t, srv.URL, "/v1/chat/completions", chatBody(true), map[string]string{"traceparent": clientTP})
	post(t, srv.URL, "/v1/chat/completions", chatBody(false), nil)
	post(t, srv.URL, "/v1/embeddings", []byte(`{"model":"qwen","input":"`+secretPrompt+`"}`), nil)
	exp.ForceFlush(context.Background())
	raw := rcv.Raw("/v1/traces") + rcv.Raw("/v1/logs")
	if len(rcv.Spans()) != 3 {
		t.Fatalf("want 3 spans, got %d", len(rcv.Spans()))
	}
	for _, s := range []string{secretPrompt, secretToolArg, secretCompletion, secretToolResult, "You are helpful", "go on"} {
		if strings.Contains(raw, s) {
			t.Errorf("exported telemetry contains %q", s)
		}
	}
}
