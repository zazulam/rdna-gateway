package mcp

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// durationBuckets are the bucket boundaries the OpenTelemetry MCP semantic
// conventions recommend for mcp.server.operation.duration, in seconds.
var durationBuckets = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

type histogram struct {
	counts []uint64 // per bucket, not cumulative; the last is +Inf
	sum    float64
	n      uint64
}

func (h *histogram) observe(v float64) {
	if h.counts == nil {
		h.counts = make([]uint64, len(durationBuckets)+1)
	}
	i := sort.SearchFloat64s(durationBuckets, v) // first bound >= v
	h.counts[i]++
	h.sum += v
	h.n++
}

// toolLabel is the tool name used in metric labels: only names of tools the
// server has, so a client can't grow the label set by calling made-up tools.
func (s *Server) toolLabel(name string) string {
	for _, t := range s.Tools {
		if t.Name == name {
			return name
		}
	}
	return "unknown"
}

func (s *Server) record(tool, outcome string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[[2]string]uint64{}
		s.durations = map[string]*histogram{}
	}
	s.calls[[2]string{tool, outcome}]++
	h := s.durations[tool]
	if h == nil {
		h = &histogram{}
		s.durations[tool] = h
	}
	h.observe(d.Seconds())
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// Metrics writes tool call counts and durations in the Prometheus text format:
//
//	mcp_server_tool_calls_total{tool, outcome}        outcome is ok or error
//	mcp_server_operation_duration_seconds{tool}       histogram of tools/call time
//
// The names follow the OpenTelemetry MCP metric mcp.server.operation.duration.
func (s *Server) Metrics(rw http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString("# HELP mcp_server_tool_calls_total MCP tools/call requests by tool and outcome (ok or error).\n")
	b.WriteString("# TYPE mcp_server_tool_calls_total counter\n")
	keys := make([][2]string, 0, len(s.calls))
	for k := range s.calls {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "mcp_server_tool_calls_total{tool=%q,outcome=%q} %d\n", k[0], k[1], s.calls[k])
	}
	b.WriteString("# HELP mcp_server_operation_duration_seconds Time to answer an MCP tools/call request, by tool.\n")
	b.WriteString("# TYPE mcp_server_operation_duration_seconds histogram\n")
	tools := make([]string, 0, len(s.durations))
	for t := range s.durations {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	for _, t := range tools {
		h := s.durations[t]
		var cum uint64
		for i, le := range durationBuckets {
			cum += h.counts[i]
			fmt.Fprintf(&b, "mcp_server_operation_duration_seconds_bucket{tool=%q,le=%q} %d\n", t, fmtFloat(le), cum)
		}
		fmt.Fprintf(&b, "mcp_server_operation_duration_seconds_bucket{tool=%q,le=\"+Inf\"} %d\n", t, h.n)
		fmt.Fprintf(&b, "mcp_server_operation_duration_seconds_sum{tool=%q} %s\n", t, fmtFloat(h.sum))
		fmt.Fprintf(&b, "mcp_server_operation_duration_seconds_count{tool=%q} %d\n", t, h.n)
	}
	rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
	rw.Write([]byte(b.String()))
}
