// Package mcp is a minimal Model Context Protocol server over Streamable HTTP:
// stateless JSON-RPC on one POST endpoint, plain JSON replies, tools only.
package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zazulam/rdna-gateway/internal/tracing"
)

// Tool is one callable tool. Call returns the text shown to the model; an
// error becomes an isError result, which the model can read and recover from.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Call        func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server dispatches JSON-RPC requests to its tools. Token, if set, is required
// as a bearer token. Tracer, if set, gets a server span per request following
// the OpenTelemetry MCP semantic conventions; spans and metrics carry names,
// ids, outcomes and timings, never tool arguments or results.
type Server struct {
	Name, Version string
	Token         string
	Tools         []Tool
	Logf          func(string, ...any)
	Tracer        tracing.Tracer

	mu        sync.Mutex
	calls     map[[2]string]uint64 // by (tool, "ok"|"error")
	durations map[string]*histogram
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const latestProtocol = "2025-06-18"

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	} else {
		log.Printf(f, a...)
	}
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		rw.Header().Set("Allow", "POST")
		http.Error(rw, "POST JSON-RPC here", http.StatusMethodNotAllowed)
		return
	}
	if s.Token != "" {
		got := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			http.Error(rw, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 8<<20))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var r rpcRequest
	if err := json.Unmarshal(body, &r); err != nil {
		writeRPC(rw, nil, nil, &rpcError{-32700, "parse error: " + err.Error()})
		return
	}
	if len(r.ID) == 0 { // a notification, e.g. notifications/initialized
		rw.WriteHeader(http.StatusAccepted)
		return
	}
	ctx, span := s.startSpan(req, r)
	result, rerr := s.dispatch(ctx, span, r)
	if rerr != nil {
		span.SetError(strconv.Itoa(rerr.Code))
	}
	span.End()
	writeRPC(rw, r.ID, result, rerr)
}

// startSpan opens the request's server span, parented by the caller's trace
// context: params._meta.traceparent (MCP's own propagation) or else the
// traceparent HTTP header.
func (s *Server) startSpan(req *http.Request, r rpcRequest) (context.Context, tracing.Span) {
	ctx := req.Context()
	if s.Tracer == nil {
		return ctx, tracing.Noop
	}
	ctx = tracing.WithTracer(ctx, s.Tracer)
	var p struct {
		Name string `json:"name"`
		Meta struct {
			Traceparent string `json:"traceparent"`
		} `json:"_meta"`
	}
	json.Unmarshal(r.Params, &p)
	parent := p.Meta.Traceparent
	if parent == "" {
		parent = req.Header.Get("traceparent")
	}
	method := methodName(r.Method)
	name := method
	attrs := []tracing.Attr{
		tracing.String("mcp.method.name", method),
		tracing.String("network.transport", "tcp"),
		tracing.String("network.protocol.name", "http"),
	}
	if id := idValue(r.ID); id != "" {
		attrs = append(attrs, tracing.String("jsonrpc.request.id", id))
	}
	if sid := token(req.Header.Get("Mcp-Session-Id")); sid != "" {
		attrs = append(attrs, tracing.String("mcp.session.id", sid))
	}
	if v := token(req.Header.Get("Mcp-Protocol-Version")); v != "" {
		attrs = append(attrs, tracing.String("mcp.protocol.version", v))
	}
	if r.Method == "tools/call" {
		tool := s.toolLabel(p.Name)
		name += " " + tool
		attrs = append(attrs, tracing.String("gen_ai.tool.name", tool), tracing.String("gen_ai.operation.name", "execute_tool"))
	}
	return s.Tracer.Start(ctx, name, tracing.Server, parent, attrs...)
}

var knownMethods = map[string]bool{"initialize": true, "ping": true, "tools/list": true, "tools/call": true}

// methodName keeps span names and attributes to methods MCP defines.
func methodName(m string) string {
	if knownMethods[m] || strings.HasPrefix(m, "notifications/") && len(m) < 64 {
		return m
	}
	return "_OTHER"
}

// idValue is a JSON-RPC id as a string: a number, or a short string.
func idValue(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch x := v.(type) {
	case float64:
		return strings.TrimSpace(string(raw))
	case string:
		return token(x)
	}
	return ""
}

// token returns s if it looks like an identifier (short, visible ASCII), else "".
func token(s string) string {
	if len(s) == 0 || len(s) > 128 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}

// errorType is the OpenTelemetry error.type for a failed tool call: a class,
// never the message, which can quote the query or URL.
func errorType(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return "tool_error"
}

func (s *Server) dispatch(ctx context.Context, span tracing.Span, r rpcRequest) (any, *rpcError) {
	switch r.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(r.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = latestProtocol
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := make([]map[string]any, len(s.Tools))
		for i, t := range s.Tools {
			tools[i] = map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		start := time.Now()
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			s.record("unknown", "error", time.Since(start))
			return nil, &rpcError{-32602, "invalid params: " + err.Error()}
		}
		for _, t := range s.Tools {
			if t.Name != p.Name {
				continue
			}
			if len(p.Arguments) == 0 {
				p.Arguments = json.RawMessage("{}")
			}
			text, err := t.Call(ctx, p.Arguments)
			d := time.Since(start)
			if err != nil {
				et := errorType(err)
				span.SetError(et)
				s.record(t.Name, "error", d)
				s.logf("tool %s %s error.type=%s", t.Name, d.Round(time.Millisecond), et)
				return toolResult(err.Error(), true), nil
			}
			s.record(t.Name, "ok", d)
			s.logf("tool %s %s ok", t.Name, d.Round(time.Millisecond))
			return toolResult(text, false), nil
		}
		s.record("unknown", "error", time.Since(start))
		return nil, &rpcError{-32602, "unknown tool " + p.Name}
	}
	return nil, &rpcError{-32601, "method not found: " + r.Method}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": isError}
}

func writeRPC(rw http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		out["id"] = nil
	}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(out)
}

// Call is a client helper: initialize, then call one tool, over HTTP.
func Call(ctx context.Context, c *http.Client, url, token, tool string, args any) (string, error) {
	send := func(id int, method string, params any) (map[string]any, error) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(b)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, &httpError{resp.Status}
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, err
		}
		if e, ok := out["error"].(map[string]any); ok {
			return nil, &httpError{e["message"].(string)}
		}
		return out["result"].(map[string]any), nil
	}
	if _, err := send(1, "initialize", map[string]any{"protocolVersion": latestProtocol, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "rdnamcp", "version": "1"}}); err != nil {
		return "", err
	}
	res, err := send(2, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var text []string
	for _, c := range res["content"].([]any) {
		if t, ok := c.(map[string]any)["text"].(string); ok {
			text = append(text, t)
		}
	}
	joined := strings.Join(text, "\n")
	if isErr, _ := res["isError"].(bool); isErr {
		return "", &httpError{joined}
	}
	return joined, nil
}

type httpError struct{ msg string }

func (e *httpError) Error() string { return e.msg }
