package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	Call(ctx, http.DefaultClient, srv.URL, "tok", "fail", nil)
	rec := httptest.NewRecorder()
	s.Metrics(rec, nil)
	m := rec.Body.String()
	if !strings.Contains(m, `rdnamcp_tool_calls_total{tool="echo",result="ok"} 1`) ||
		!strings.Contains(m, `rdnamcp_tool_calls_total{tool="fail",result="error"} 1`) {
		t.Fatalf("metrics:\n%s", m)
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
