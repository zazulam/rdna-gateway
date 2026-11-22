package otlp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zazulam/rdna-gateway/internal/otlp"
	"github.com/zazulam/rdna-gateway/internal/otlp/otlptest"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnv(t *testing.T) {
	c := otlp.Config{Endpoint: "http://file:4318", ServiceName: "from-file",
		Headers: map[string]string{"X-File": "1"}}.WithEnv(env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":        "http://collector:4318/",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://traces:9999/custom",
		"OTEL_EXPORTER_OTLP_HEADERS":         "Authorization=Bearer%20abc, x-tenant = lab",
		"OTEL_SERVICE_NAME":                  "gw",
		"OTEL_RESOURCE_ATTRIBUTES":           "deployment.environment.name=lab,bad,service.version=1",
		"OTEL_EXPORTER_OTLP_TIMEOUT":         "1500",
	}))
	if !c.Enabled() {
		t.Fatal("should be enabled")
	}
	if got := c.TracesURL(); got != "http://traces:9999/custom" {
		t.Errorf("traces url %q", got)
	}
	if got := c.LogsURL(); got != "http://collector:4318/v1/logs" {
		t.Errorf("logs url %q", got)
	}
	if c.Headers["Authorization"] != "Bearer abc" || c.Headers["x-tenant"] != "lab" || c.Headers["X-File"] != "1" {
		t.Errorf("headers %v", c.Headers)
	}
	if c.ServiceName != "gw" || c.ResourceAttributes["deployment.environment.name"] != "lab" || c.Timeout != 1500*time.Millisecond {
		t.Errorf("config %+v", c)
	}

	if (otlp.Config{}).WithEnv(env(nil)).Enabled() {
		t.Error("no endpoint should mean off")
	}
	off := otlp.Config{Endpoint: "http://x"}.WithEnv(env(map[string]string{"OTEL_SDK_DISABLED": "true"}))
	if off.Enabled() || otlp.NewExporter(off, "s", "svc", otlp.Options{}) != nil {
		t.Error("OTEL_SDK_DISABLED should turn export off")
	}
	nt := otlp.Config{Endpoint: "http://x"}.WithEnv(env(map[string]string{"OTEL_TRACES_EXPORTER": "none"}))
	if nt.TracesURL() != "" || nt.LogsURL() == "" {
		t.Error("OTEL_TRACES_EXPORTER=none should only drop traces")
	}
	if w := (otlp.Config{Protocol: "http/protobuf"}).Warnings(); len(w) != 1 {
		t.Errorf("want a protocol warning, got %v", w)
	}
}

func TestTraceparent(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := otlp.ParseTraceparent(tp)
	if !ok || !sc.Sampled() || sc.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID.String() != "00f067aa0ba902b7" {
		t.Fatalf("parse: %+v %v", sc, ok)
	}
	if sc.Traceparent() != tp {
		t.Errorf("round trip %q", sc.Traceparent())
	}
	for _, bad := range []string{
		"",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01", // zero trace id
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", // zero span id
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // forbidden version
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", // upper case
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"00-4bf92f3577b34da6a3ce929d0e0e473-600f067aa0ba902b7-01",
	} {
		if _, ok := otlp.ParseTraceparent(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	// A future version may carry more fields.
	if _, ok := otlp.ParseTraceparent("01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-abcd"); !ok {
		t.Error("future version with extra field should parse")
	}
	a, b := otlp.NewTraceID(), otlp.NewTraceID()
	if a == b || !a.IsValid() || !otlp.NewSpanID().IsValid() {
		t.Error("ids should be random and non-zero")
	}
}

// The encoded bodies follow the OTLP/HTTP JSON mapping: hex ids, int64 as
// strings, enums as numbers, lowerCamelCase keys.
func TestEncodeShape(t *testing.T) {
	rcv := otlptest.New(t)
	exp := otlp.NewExporter(otlp.Config{Endpoint: rcv.URL, Headers: map[string]string{"X-Token": "t"}},
		"test/scope", "svc", otlp.Options{})
	parent, _ := otlp.ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	s := exp.Start(parent, "chat m", otlp.KindClient, otlp.String("gen_ai.operation.name", "chat"))
	s.SetAttributes(otlp.Int64("gen_ai.usage.input_tokens", 1234567890123), otlp.Float("x.f", 0.5),
		otlp.Strings("gen_ai.response.finish_reasons", []string{"stop"}), otlp.Bool("x.b", true))
	s.AddEvent("rdnagw.retry", otlp.String("error.type", "dial"))
	s.SetStatus(otlp.StatusError, "boom")
	s.End()
	s.End() // idempotent
	exp.Emit(otlp.LogRecord{EventName: "rdnagw.worker.state_change", SeverityNumber: otlp.SeverityInfo,
		Attrs: []otlp.KeyValue{otlp.String("worker", "w1")}})
	if err := exp.ExportMetrics(context.Background(), []otlp.Metric{
		{Name: "c", Kind: otlp.MetricSum, Monotonic: true, Points: []otlp.Point{{IsInt: true, IntValue: 7, Time: time.Now()}}},
		{Name: "h", Kind: otlp.MetricHistogram, Unit: "s", Points: []otlp.Point{{Count: 3, Sum: 1.5, Bounds: []float64{0.1, 1},
			BucketCounts: []uint64{1, 1, 1}, Time: time.Now()}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := exp.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	raw := rcv.Raw("/v1/traces")
	var generic map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &generic); err != nil {
		t.Fatalf("traces body: %v\n%s", err, raw)
	}
	for _, want := range []string{`"resourceSpans"`, `"scopeSpans"`, `"traceId":"4bf92f3577b34da6a3ce929d0e0e4736"`,
		`"parentSpanId":"00f067aa0ba902b7"`, `"kind":3`, `"intValue":"1234567890123"`, `"startTimeUnixNano":"`,
		`"code":2`, `"name":"test/scope"`, `"arrayValue":{"values":[{"stringValue":"stop"}]}`} {
		if !strings.Contains(raw, want) {
			t.Errorf("traces body lacks %s:\n%s", want, raw)
		}
	}
	spans := rcv.Spans()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	sp := spans[0]
	if sp.Resource["service.name"] != "svc" || sp.Events[0].Name != "rdnagw.retry" || sp.StatusMessage != "boom" {
		t.Errorf("span %+v", sp)
	}
	logs := rcv.Logs()
	if len(logs) != 1 || logs[0].EventName != "rdnagw.worker.state_change" || logs[0].Attrs["event.name"] != "rdnagw.worker.state_change" {
		t.Errorf("logs %+v", logs)
	}
	m := rcv.Raw("/v1/metrics")
	for _, want := range []string{`"resourceMetrics"`, `"asInt":"7"`, `"isMonotonic":true`, `"aggregationTemporality":2`,
		`"bucketCounts":["1","1","1"]`, `"explicitBounds":[0.1,1]`, `"count":"3"`} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics body lacks %s:\n%s", want, m)
		}
	}
	for _, h := range rcv.Headers() {
		if h.Get("X-Token") != "t" {
			t.Error("configured header not sent")
		}
	}
}

func TestSamplingAndNil(t *testing.T) {
	var nilExp *otlp.Exporter
	s := nilExp.Start(otlp.SpanContext{}, "x", otlp.KindClient)
	s.SetAttributes(otlp.String("a", "b")) // nil-safe
	s.AddEvent("e")
	s.End()
	nilExp.Emit(otlp.LogRecord{})
	if s.Context().IsValid() || nilExp.Shutdown(context.Background()) != nil {
		t.Error("nil exporter should be inert")
	}

	rcv := otlptest.New(t)
	exp := otlp.NewExporter(otlp.Config{Endpoint: rcv.URL}, "s", "svc", otlp.Options{})
	unsampled, _ := otlp.ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	if exp.Start(unsampled, "x", otlp.KindClient) != nil {
		t.Error("an unsampled parent should not be recorded")
	}
	root := exp.Start(otlp.SpanContext{}, "x", otlp.KindClient)
	if !root.Context().IsValid() || !root.Context().Sampled() {
		t.Error("a root span should get fresh sampled ids")
	}
	exp.Shutdown(context.Background())
}

// Enqueueing never blocks: with an unreachable collector and a tiny queue,
// ending many spans returns at once and the overflow is counted.
func TestQueueOverflowDrops(t *testing.T) {
	exp := otlp.NewExporter(otlp.Config{Endpoint: "http://127.0.0.1:1", Timeout: 50 * time.Millisecond},
		"s", "svc", otlp.Options{QueueSize: 4, BatchSize: 1000, BatchTimeout: time.Hour})
	start := time.Now()
	for i := 0; i < 100; i++ {
		exp.Start(otlp.SpanContext{}, "x", otlp.KindClient).End()
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("ending spans took %v; must not block", d)
	}
	if st := exp.Stats(); st.DroppedSpans < 96 {
		t.Errorf("want >=96 dropped, got %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if st := exp.Stats(); st.ExportErrors == 0 || st.DroppedSpans != 100 {
		t.Errorf("failed export should be counted and dropped: %+v", st)
	}
}

// A full batch is exported without waiting for the batch timer.
func TestFullBatchExportsEarly(t *testing.T) {
	rcv := otlptest.New(t)
	exp := otlp.NewExporter(otlp.Config{Endpoint: rcv.URL}, "s", "svc",
		otlp.Options{BatchSize: 3, BatchTimeout: time.Hour})
	defer exp.Shutdown(context.Background())
	for i := 0; i < 3; i++ {
		exp.Start(otlp.SpanContext{}, "x", otlp.KindClient).End()
	}
	if !otlptest.WaitFor(2*time.Second, func() bool { return len(rcv.Spans()) == 3 }) {
		t.Fatalf("got %d spans", len(rcv.Spans()))
	}
}
