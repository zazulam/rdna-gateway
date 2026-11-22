// Package otlptest is an in-process OTLP/HTTP JSON receiver for tests.
package otlptest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Receiver records every OTLP/HTTP JSON export it is sent.
type Receiver struct {
	*httptest.Server

	mu      sync.Mutex
	bodies  map[string][][]byte // by path
	headers []http.Header
}

// New starts a receiver; it is closed when the test ends.
func New(t testing.TB) *Receiver {
	r := &Receiver{bodies: map[string][][]byte{}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		if req.Header.Get("Content-Type") != "application/json" || !json.Valid(b) {
			http.Error(rw, "want a JSON body", http.StatusUnsupportedMediaType)
			return
		}
		r.mu.Lock()
		r.bodies[req.URL.Path] = append(r.bodies[req.URL.Path], b)
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		io.WriteString(rw, "{}")
	}))
	t.Cleanup(r.Server.Close)
	return r
}

// Raw returns the bodies posted to path (e.g. "/v1/traces"), concatenated.
func (r *Receiver) Raw(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var s string
	for _, b := range r.bodies[path] {
		s += string(b) + "\n"
	}
	return s
}

// Headers returns the request headers of every export so far.
func (r *Receiver) Headers() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers...)
}

// Span is a decoded span, with attributes flattened to Go values (strings,
// bools, float64, int64-as-string for intValue, []any for arrays).
type Span struct {
	TraceID, SpanID, ParentSpanID string
	Name                          string
	Kind                          int
	Attrs                         map[string]any
	Events                        []Event
	StatusCode                    int
	StatusMessage                 string
	Resource                      map[string]any
}

// Event is a decoded span event.
type Event struct {
	Name  string
	Attrs map[string]any
}

// LogRecord is a decoded log record.
type LogRecord struct {
	EventName      string
	SeverityNumber int
	Body           any
	Attrs          map[string]any
	TraceID        string
	SpanID         string
}

type kv struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

func flat(kvs []kv) map[string]any {
	out := map[string]any{}
	for _, a := range kvs {
		out[a.Key] = Value(a.Value)
	}
	return out
}

// Value decodes one OTLP JSON AnyValue.
func Value(raw json.RawMessage) any {
	var v struct {
		StringValue *string  `json:"stringValue"`
		BoolValue   *bool    `json:"boolValue"`
		IntValue    *string  `json:"intValue"`
		DoubleValue *float64 `json:"doubleValue"`
		ArrayValue  *struct {
			Values []json.RawMessage `json:"values"`
		} `json:"arrayValue"`
	}
	json.Unmarshal(raw, &v)
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != nil:
		return *v.IntValue
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.ArrayValue != nil:
		var out []any
		for _, e := range v.ArrayValue.Values {
			out = append(out, Value(e))
		}
		return out
	}
	return nil
}

// Spans decodes every span received so far.
func (r *Receiver) Spans() []Span {
	r.mu.Lock()
	bodies := append([][]byte(nil), r.bodies["/v1/traces"]...)
	r.mu.Unlock()
	var out []Span
	for _, b := range bodies {
		var req struct {
			ResourceSpans []struct {
				Resource struct {
					Attributes []kv `json:"attributes"`
				} `json:"resource"`
				ScopeSpans []struct {
					Spans []struct {
						TraceID      string `json:"traceId"`
						SpanID       string `json:"spanId"`
						ParentSpanID string `json:"parentSpanId"`
						Name         string `json:"name"`
						Kind         int    `json:"kind"`
						Attributes   []kv   `json:"attributes"`
						Events       []struct {
							Name       string `json:"name"`
							Attributes []kv   `json:"attributes"`
						} `json:"events"`
						Status struct {
							Code    int    `json:"code"`
							Message string `json:"message"`
						} `json:"status"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		json.Unmarshal(b, &req)
		for _, rs := range req.ResourceSpans {
			res := flat(rs.Resource.Attributes)
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					sp := Span{TraceID: s.TraceID, SpanID: s.SpanID, ParentSpanID: s.ParentSpanID, Name: s.Name,
						Kind: s.Kind, Attrs: flat(s.Attributes), StatusCode: s.Status.Code,
						StatusMessage: s.Status.Message, Resource: res}
					for _, e := range s.Events {
						sp.Events = append(sp.Events, Event{Name: e.Name, Attrs: flat(e.Attributes)})
					}
					out = append(out, sp)
				}
			}
		}
	}
	return out
}

// Logs decodes every log record received so far.
func (r *Receiver) Logs() []LogRecord {
	r.mu.Lock()
	bodies := append([][]byte(nil), r.bodies["/v1/logs"]...)
	r.mu.Unlock()
	var out []LogRecord
	for _, b := range bodies {
		var req struct {
			ResourceLogs []struct {
				ScopeLogs []struct {
					LogRecords []struct {
						EventName      string          `json:"eventName"`
						SeverityNumber int             `json:"severityNumber"`
						Body           json.RawMessage `json:"body"`
						Attributes     []kv            `json:"attributes"`
						TraceID        string          `json:"traceId"`
						SpanID         string          `json:"spanId"`
					} `json:"logRecords"`
				} `json:"scopeLogs"`
			} `json:"resourceLogs"`
		}
		json.Unmarshal(b, &req)
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, l := range sl.LogRecords {
					rec := LogRecord{EventName: l.EventName, SeverityNumber: l.SeverityNumber,
						Attrs: flat(l.Attributes), TraceID: l.TraceID, SpanID: l.SpanID}
					if len(l.Body) > 0 {
						rec.Body = Value(l.Body)
					}
					out = append(out, rec)
				}
			}
		}
	}
	return out
}

// WaitFor polls until cond holds or the timeout passes, and reports which.
func WaitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
