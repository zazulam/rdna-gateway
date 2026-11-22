package tracing

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Recorder is an in-memory Tracer for tests: it keeps every span it starts.
type Recorder struct {
	mu    sync.Mutex
	Spans []*RecordedSpan
}

// RecordedSpan is one span a Recorder started.
type RecordedSpan struct {
	Name        string
	Kind        Kind
	Traceparent string        // the remote parent passed to Start
	Parent      *RecordedSpan // the span in ctx at Start, if any
	Attrs       map[string]any
	ErrorType   string
	Ended       bool
	mu          *sync.Mutex
}

type recKey struct{}

func (r *Recorder) Start(ctx context.Context, name string, kind Kind, traceparent string, attrs ...Attr) (context.Context, Span) {
	parent, _ := ctx.Value(recKey{}).(*RecordedSpan)
	s := &RecordedSpan{Name: name, Kind: kind, Traceparent: traceparent, Parent: parent, Attrs: map[string]any{}, mu: &r.mu}
	s.SetAttributes(attrs...)
	r.mu.Lock()
	r.Spans = append(r.Spans, s)
	r.mu.Unlock()
	return WithTracer(context.WithValue(ctx, recKey{}, s), r), s
}

func (s *RecordedSpan) SetAttributes(attrs ...Attr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range attrs {
		s.Attrs[a.Key] = a.Value
	}
}

func (s *RecordedSpan) SetError(t string) { s.mu.Lock(); s.ErrorType = t; s.mu.Unlock() }
func (s *RecordedSpan) End()              { s.mu.Lock(); s.Ended = true; s.mu.Unlock() }

// Find returns the first span whose name starts with prefix, or nil.
func (r *Recorder) Find(prefix string) *RecordedSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.Spans {
		if strings.HasPrefix(s.Name, prefix) {
			return s
		}
	}
	return nil
}

// Dump renders every span with its attributes, for searching in tests.
func (r *Recorder) Dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, s := range r.Spans {
		fmt.Fprintf(&b, "%s kind=%d parent=%q error.type=%q %v\n", s.Name, s.Kind, s.Traceparent, s.ErrorType, s.Attrs)
	}
	return b.String()
}
