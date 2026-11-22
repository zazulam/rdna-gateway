// Package tracing is the small span API rdnamcp's packages instrument
// against. A Tracer travels in the context, so code deep in a call (the
// retrieval steps) starts child spans without knowing which exporter, if any,
// is behind it. With no tracer in the context every call is a no-op.
//
// Attributes are names, ids, counts and timings only: never tool arguments,
// results, queries, page text or memory contents.
package tracing

import "context"

// Kind is the OpenTelemetry span kind.
type Kind int

const (
	Internal Kind = iota
	Server
	Client
)

// Attr is one span attribute. Value is a string, bool, int, int64 or float64.
type Attr struct {
	Key   string
	Value any
}

func String(k, v string) Attr        { return Attr{k, v} }
func Int(k string, v int) Attr       { return Attr{k, int64(v)} }
func Bool(k string, v bool) Attr     { return Attr{k, v} }
func Float(k string, v float64) Attr { return Attr{k, v} }

// Span is a started span. End records its duration; calls after End are ignored.
type Span interface {
	SetAttributes(attrs ...Attr)
	// SetError marks the span failed with an OpenTelemetry error.type value
	// (a low-cardinality class such as "tool_error" or "timeout", never a message).
	SetError(errType string)
	End()
}

// Tracer starts spans. The parent is the span in ctx, if any; otherwise
// traceparent (a W3C trace context header, possibly empty or invalid, which
// the tracer then ignores) makes the span a child of a remote caller's span.
type Tracer interface {
	Start(ctx context.Context, name string, kind Kind, traceparent string, attrs ...Attr) (context.Context, Span)
}

type tracerKey struct{}

// WithTracer returns ctx carrying t, for Start calls further down.
func WithTracer(ctx context.Context, t Tracer) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, tracerKey{}, t)
}

// Start begins a span with the tracer carried by ctx, as a child of the span in ctx.
func Start(ctx context.Context, name string, kind Kind, attrs ...Attr) (context.Context, Span) {
	t, _ := ctx.Value(tracerKey{}).(Tracer)
	if t == nil {
		return ctx, noop{}
	}
	return t.Start(ctx, name, kind, "", attrs...)
}

type noop struct{}

func (noop) SetAttributes(...Attr) {}
func (noop) SetError(string)       {}
func (noop) End()                  {}

// Noop is a span that records nothing.
var Noop Span = noop{}
