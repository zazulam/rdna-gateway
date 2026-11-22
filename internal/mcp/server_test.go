package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zazulam/rdna-gateway/internal/tracing"
)

func testServer(t *testing.T) *httptest.Server {
	srv, _ := testServerWith(t)
	return srv
}

func testServerWith(t *testing.T) (*httptest.Server, *Server) {
	s := &Server{Name: "t", Version: "1", Token: "tok", Logf: func(string, ...any) {}, Tools: []Tool{
		{Name: "echo", Description: "echo", Schema: map[string]any{"type": "object"},
			Call: func(_ context.Context, a json.RawMessage) (string, error) {
				var p struct{ Text string }
				json.Unmarshal(a, &p)
				return "echo: " + p.Text, nil
			}},
		{Name: "fail", Schema: map[string]any{"type": "object"},
			Call: func(context.Context, json.RawMessage) (string, error) { return "", errors.New("boom") }},
	}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func TestMetricsCountCalls(t *testing.T) {
	srv, s := testServerWith(t)
	ctx := context.Background()
	Call(ctx, http.DefaultClient, srv.URL, "tok", "echo", nil)
	Call(ctx, http.DefaultClient, srv.URL, "tok", "echo", nil)
	Call(ctx, http.DefaultClient, srv.URL, "tok", "fail", nil)
	Call(ctx, http.DefaultClient, srv.URL, "tok", "made-up-tool-1", nil)
	rec := httptest.NewRecorder()
	s.Metrics(rec, nil)
	m := rec.Body.String()
	for _, want := range []string{
		"# TYPE mcp_server_tool_calls_total counter",
		`mcp_server_tool_calls_total{tool="echo",outcome="ok"} 2`,
		`mcp_server_tool_calls_total{tool="fail",outcome="error"} 1`,
		`mcp_server_tool_calls_total{tool="unknown",outcome="error"} 1`,
		"# TYPE mcp_server_operation_duration_seconds histogram",
		`mcp_server_operation_duration_seconds_bucket{tool="echo",le="0.01"} 2`,
		`mcp_server_operation_duration_seconds_bucket{tool="echo",le="+Inf"} 2`,
		`mcp_server_operation_duration_seconds_count{tool="echo"} 2`,
		`mcp_server_operation_duration_seconds_sum{tool="echo"} `,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("missing %q in:\n%s", want, m)
		}
	}
	if strings.Contains(m, "made-up") {
		t.Fatal("unknown tool names must not become label values")
	}
}

func TestHistogramBuckets(t *testing.T) {
	s := &Server{}
	for _, d := range []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 150 * time.Millisecond, 3 * time.Second, 10 * time.Minute} {
		s.record("x", "ok", d)
	}
	rec := httptest.NewRecorder()
	s.Metrics(rec, nil)
	m := rec.Body.String()
	for _, want := range []string{
		`{tool="x",le="0.01"} 2`, // 5ms and exactly 10ms (le is inclusive)
		`{tool="x",le="0.1"} 2`,
		`{tool="x",le="0.2"} 3`,
		`{tool="x",le="5"} 4`,
		`{tool="x",le="300"} 4`,
		`{tool="x",le="+Inf"} 5`,
		`_sum{tool="x"} 603.165`,
	} {
		if !strings.Contains(m, want) {
			t.Fatalf("missing %q in:\n%s", want, m)
		}
	}
}

func tracedServer(t *testing.T, tools ...Tool) (*httptest.Server, *tracing.Recorder, *strings.Builder) {
	rec := &tracing.Recorder{}
	logs := &strings.Builder{}
	var mu sync.Mutex
	s := &Server{Name: "t", Version: "1", Token: "tok", Tracer: rec, Tools: tools,
		Logf: func(f string, a ...any) { mu.Lock(); fmt.Fprintf(logs, f+"\n", a...); mu.Unlock() }}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, rec, logs
}

func TestServerSpans(t *testing.T) {
	srv, rec, _ := tracedServer(t,
		Tool{Name: "echo", Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
			_, sp := tracing.Start(ctx, "child", tracing.Client)
			sp.End()
			return "ok", nil
		}},
		Tool{Name: "slow", Call: func(ctx context.Context, _ json.RawMessage) (string, error) {
			return "", fmt.Errorf("wrapped: %w", context.DeadlineExceeded)
		}})
	const meta = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	const hdr = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":{"traceparent":"`+meta+`"}}}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("traceparent", hdr)
	req.Header.Set("Mcp-Session-Id", "sess-123")
	if _, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	sp := rec.Find("tools/call echo")
	if sp == nil {
		t.Fatalf("no tool span:\n%s", rec.Dump())
	}
	if sp.Kind != tracing.Server || !sp.Ended || sp.Traceparent != meta {
		t.Fatalf("span: %+v (params._meta.traceparent should win over the header)", sp)
	}
	for k, v := range map[string]any{"mcp.method.name": "tools/call", "gen_ai.tool.name": "echo",
		"gen_ai.operation.name": "execute_tool", "jsonrpc.request.id": "7", "mcp.session.id": "sess-123"} {
		if sp.Attrs[k] != v {
			t.Fatalf("%s = %v, want %v", k, sp.Attrs[k], v)
		}
	}
	if sp.ErrorType != "" {
		t.Fatalf("error.type on success: %q", sp.ErrorType)
	}
	if c := rec.Find("child"); c == nil || c.Parent != sp {
		t.Fatalf("tool's own spans should nest under the request span:\n%s", rec.Dump())
	}

	// Header only; a failing tool; JSON-RPC errors.
	req, _ = http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":"a-1","method":"tools/call","params":{"name":"slow"}}`))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("traceparent", hdr)
	http.DefaultClient.Do(req)
	sp = rec.Find("tools/call slow")
	if sp == nil || sp.Traceparent != hdr || sp.ErrorType != "timeout" || sp.Attrs["jsonrpc.request.id"] != "a-1" {
		t.Fatalf("slow span:\n%s", rec.Dump())
	}
	post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope"}}`)
	if sp = rec.Find("tools/call unknown"); sp == nil || sp.ErrorType != "-32602" {
		t.Fatalf("unknown tool:\n%s", rec.Dump())
	}
	post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":4,"method":"made/up"}`)
	if sp = rec.Find("_OTHER"); sp == nil || sp.ErrorType != "-32601" {
		t.Fatalf("unknown method:\n%s", rec.Dump())
	}
	post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	if sp = rec.Find("tools/list"); sp == nil || sp.Attrs["mcp.method.name"] != "tools/list" {
		t.Fatalf("tools/list:\n%s", rec.Dump())
	}
}

// TestNoContentInTelemetry sends a canary string as tool arguments, gets it
// back in a result and in an error, and checks it never reaches spans,
// metrics or logs.
func TestNoContentInTelemetry(t *testing.T) {
	const canary = "CANARY-7f3a9c-secret-query"
	srv, rec, logs := tracedServer(t,
		Tool{Name: "echo", Call: func(_ context.Context, a json.RawMessage) (string, error) {
			return "result " + string(a), nil
		}},
		Tool{Name: "fail", Call: func(_ context.Context, a json.RawMessage) (string, error) {
			return "", errors.New("upstream said no to " + string(a))
		}})
	ctx := context.Background()
	got, err := Call(ctx, http.DefaultClient, srv.URL, "tok", "echo", map[string]any{"query": canary})
	if err != nil || !strings.Contains(got, canary) {
		t.Fatalf("echo: %q %v", got, err)
	}
	if _, err := Call(ctx, http.DefaultClient, srv.URL, "tok", "fail", map[string]any{"text": canary}); err == nil || !strings.Contains(err.Error(), canary) {
		t.Fatalf("the model should still see the error text: %v", err)
	}
	Call(ctx, http.DefaultClient, srv.URL, "tok", canary, map[string]any{"q": canary})
	post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":"`+canary+` with spaces","method":"`+canary+`"}`)

	m := httptest.NewRecorder()
	srv.Config.Handler.(*Server).Metrics(m, nil)
	for what, out := range map[string]string{"spans": rec.Dump(), "metrics": m.Body.String(), "logs": logs.String()} {
		if strings.Contains(out, canary) || strings.Contains(out, "CANARY") {
			t.Fatalf("canary leaked into %s:\n%s", what, out)
		}
	}
	if len(rec.Spans) < 5 {
		t.Fatalf("expected spans for every request:\n%s", rec.Dump())
	}
}

func post(t *testing.T, url, token, body string) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestProtocol(t *testing.T) {
	srv := testServer(t)
	if code, _ := post(t, srv.URL, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	_, b := post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if !strings.Contains(b, `"protocolVersion":"2025-03-26"`) || !strings.Contains(b, `"tools"`) {
		t.Fatalf("initialize: %s", b)
	}
	if code, _ := post(t, srv.URL, "tok", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	_, b = post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if !strings.Contains(b, `"name":"echo"`) || !strings.Contains(b, `"inputSchema"`) {
		t.Fatalf("tools/list: %s", b)
	}
	_, b = post(t, srv.URL, "tok", `{"jsonrpc":"2.0","id":3,"method":"nope"}`)
	if !strings.Contains(b, `-32601`) {
		t.Fatalf("unknown method: %s", b)
	}
}

func TestCallHelper(t *testing.T) {
	srv := testServer(t)
	ctx := context.Background()
	got, err := Call(ctx, http.DefaultClient, srv.URL, "tok", "echo", map[string]any{"text": "hi"})
	if err != nil || got != "echo: hi" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Call(ctx, http.DefaultClient, srv.URL, "tok", "fail", nil); err == nil || err.Error() != "boom" {
		t.Fatalf("tool error should surface: %v", err)
	}
	if _, err := Call(ctx, http.DefaultClient, srv.URL, "bad", "echo", nil); err == nil {
		t.Fatal("bad token should fail")
	}
}
