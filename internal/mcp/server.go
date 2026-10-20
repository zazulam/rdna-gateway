// Package mcp is a minimal Model Context Protocol server over Streamable HTTP:
// stateless JSON-RPC on one POST endpoint, plain JSON replies, tools only.
package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
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
// as a bearer token.
type Server struct {
	Name, Version string
	Token         string
	Tools         []Tool
	Logf          func(string, ...any)

	mu    sync.Mutex
	stats map[[2]string]*callStats // by (tool, "ok"|"error")
}

type callStats struct {
	n       int64
	seconds float64
}

func (s *Server) record(tool string, ok bool, d time.Duration) {
	res := "ok"
	if !ok {
		res = "error"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stats == nil {
		s.stats = map[[2]string]*callStats{}
	}
	k := [2]string{tool, res}
	if s.stats[k] == nil {
		s.stats[k] = &callStats{}
	}
	s.stats[k].n++
	s.stats[k].seconds += d.Seconds()
}

// Metrics writes per-tool call counts and time in the Prometheus text format.
func (s *Server) Metrics(rw http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([][2]string, 0, len(s.stats))
	for k := range s.stats {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+keys[i][1] < keys[j][0]+keys[j][1] })
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
	for _, k := range keys {
		fmt.Fprintf(rw, "rdnamcp_tool_calls_total{tool=%q,result=%q} %d\n", k[0], k[1], s.stats[k].n)
		fmt.Fprintf(rw, "rdnamcp_tool_seconds_total{tool=%q,result=%q} %.3f\n", k[0], k[1], s.stats[k].seconds)
	}
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
	result, rerr := s.dispatch(req.Context(), r)
	writeRPC(rw, r.ID, result, rerr)
}

func (s *Server) dispatch(ctx context.Context, r rpcRequest) (any, *rpcError) {
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
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params: " + err.Error()}
		}
		for _, t := range s.Tools {
			if t.Name != p.Name {
				continue
			}
			if len(p.Arguments) == 0 {
				p.Arguments = json.RawMessage("{}")
			}
			start := time.Now()
			text, err := t.Call(ctx, p.Arguments)
			s.record(t.Name, err == nil, time.Since(start))
			s.logf("tool %s %s err=%v", t.Name, time.Since(start).Round(time.Millisecond), err)
			if err != nil {
				return toolResult(err.Error(), true), nil
			}
			return toolResult(text, false), nil
		}
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
