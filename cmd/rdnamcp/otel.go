package main

import (
	"context"

	"github.com/zazulam/rdna-gateway/internal/otlp"
	"github.com/zazulam/rdna-gateway/internal/tracing"
)

// otelTracer sends rdnamcp's spans through an OTLP exporter.
type otelTracer struct{ e *otlp.Exporter }

type spanCtxKey struct{}

var kinds = map[tracing.Kind]otlp.SpanKind{
	tracing.Internal: otlp.KindInternal, tracing.Server: otlp.KindServer, tracing.Client: otlp.KindClient,
}

func (t otelTracer) Start(ctx context.Context, name string, kind tracing.Kind, traceparent string,
	attrs ...tracing.Attr) (context.Context, tracing.Span) {
	parent, inCtx := ctx.Value(spanCtxKey{}).(otlp.SpanContext)
	if !inCtx && traceparent != "" {
		parent, _ = otlp.ParseTraceparent(traceparent)
	}
	kv := make([]otlp.KeyValue, len(attrs))
	for i, a := range attrs {
		kv[i] = otlp.KeyValue{Key: a.Key, Value: a.Value}
	}
	s := t.e.Start(parent, name, kinds[kind], kv...)
	sc := s.Context()
	if s == nil {
		// Not sampled: keep the caller's context so child spans aren't
		// sampled either, instead of each starting a trace of its own.
		sc = parent
	}
	return tracing.WithTracer(context.WithValue(ctx, spanCtxKey{}, sc), t), otelSpan{s}
}

type otelSpan struct{ s *otlp.Span } // a nil s records nothing

func (o otelSpan) SetAttributes(attrs ...tracing.Attr) {
	for _, a := range attrs {
		o.s.SetAttributes(otlp.KeyValue{Key: a.Key, Value: a.Value})
	}
}

// SetError sets error.type and an error status with no message: messages can
// quote tool input.
func (o otelSpan) SetError(errType string) {
	o.s.SetAttributes(otlp.String("error.type", errType))
	o.s.SetStatus(otlp.StatusError, "")
}

func (o otelSpan) End() { o.s.End() }
