package otlp

import (
	"encoding/json"
	"math"
	"strconv"
	"sync"
	"time"
)

// KeyValue is one attribute. Value is a string, bool, any integer type,
// float64/float32, or a []string / []int64 / []float64.
type KeyValue struct {
	Key   string
	Value any
}

func String(k, v string) KeyValue           { return KeyValue{k, v} }
func Int(k string, v int) KeyValue          { return KeyValue{k, int64(v)} }
func Int64(k string, v int64) KeyValue      { return KeyValue{k, v} }
func Float(k string, v float64) KeyValue    { return KeyValue{k, v} }
func Bool(k string, v bool) KeyValue        { return KeyValue{k, v} }
func Strings(k string, v []string) KeyValue { return KeyValue{k, v} }

// SpanKind values, as OTLP numbers them.
type SpanKind int

const (
	KindInternal SpanKind = 1
	KindServer   SpanKind = 2
	KindClient   SpanKind = 3
	KindProducer SpanKind = 4
	KindConsumer SpanKind = 5
)

// StatusCode values, as OTLP numbers them.
type StatusCode int

const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// Event is a timestamped annotation on a span.
type Event struct {
	Name  string
	Time  time.Time
	Attrs []KeyValue
}

// Span is one recorded operation. Create it with [Exporter.Start]; every
// method is safe on a nil *Span (tracing off or not sampled), so callers
// never need to check.
type Span struct {
	mu       sync.Mutex
	exp      *Exporter
	ended    bool
	sc       SpanContext
	parent   SpanID
	name     string
	kind     SpanKind
	start    time.Time
	end      time.Time
	attrs    []KeyValue
	events   []Event
	status   StatusCode
	statusMs string
}

// Context is the span's own context, to propagate to the next hop. It is the
// zero SpanContext for a nil span.
func (s *Span) Context() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return s.sc
}

// SetName renames the span (e.g. once the model is known).
func (s *Span) SetName(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
}

// SetAttributes adds or replaces attributes.
func (s *Span) SetAttributes(kv ...KeyValue) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
next:
	for _, a := range kv {
		for i := range s.attrs {
			if s.attrs[i].Key == a.Key {
				s.attrs[i] = a
				continue next
			}
		}
		s.attrs = append(s.attrs, a)
	}
}

// AddEvent records a named event now.
func (s *Span) AddEvent(name string, kv ...KeyValue) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if len(s.events) < 128 {
		s.events = append(s.events, Event{Name: name, Time: time.Now(), Attrs: kv})
	}
	s.mu.Unlock()
}

// SetStatus sets the span status. The message only counts for StatusError.
func (s *Span) SetStatus(code StatusCode, msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.status, s.statusMs = code, msg
	s.mu.Unlock()
}

// End finishes the span and queues it for export. Later calls do nothing.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.end = time.Now()
	s.mu.Unlock()
	s.exp.enqueueSpan(s)
}

// LogRecord is one OTLP log record (or event, when EventName is set).
type LogRecord struct {
	Time           time.Time
	SeverityNumber int // 9 = INFO, 13 = WARN, 17 = ERROR
	SeverityText   string
	EventName      string
	Body           string
	Attrs          []KeyValue
	TraceID        TraceID
	SpanID         SpanID
}

// Severity numbers for LogRecord.SeverityNumber.
const (
	SeverityDebug = 5
	SeverityInfo  = 9
	SeverityWarn  = 13
	SeverityError = 17
)

// Metric is one metric with its data points, for [Exporter.ExportMetrics].
// Exactly one of Sum, Gauge or Histogram is used, according to Kind.
type Metric struct {
	Name        string
	Description string
	Unit        string
	Kind        MetricKind
	Monotonic   bool // for MetricSum
	Points      []Point
}

// MetricKind selects the OTLP metric data type.
type MetricKind int

const (
	MetricGauge     MetricKind = iota
	MetricSum                  // cumulative
	MetricHistogram            // cumulative, explicit buckets
)

// Point is one data point. For gauges and sums set Value (or IntValue with
// IsInt); for histograms set Count, Sum, Bounds and BucketCounts (one more
// count than bounds, non-cumulative per bucket as OTLP expects).
type Point struct {
	Attrs        []KeyValue
	Start, Time  time.Time
	Value        float64
	IntValue     int64
	IsInt        bool
	Count        uint64
	Sum          float64
	Bounds       []float64
	BucketCounts []uint64
}

// ---- JSON encoding (OTLP/HTTP JSON: hex ids, 64-bit integers as strings,
// enums as numbers, lowerCamelCase field names). ----

type jsonAnyValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	IntValue    *string         `json:"intValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
	ArrayValue  *jsonArrayValue `json:"arrayValue,omitempty"`
}

type jsonArrayValue struct {
	Values []jsonAnyValue `json:"values"`
}

type jsonKV struct {
	Key   string       `json:"key"`
	Value jsonAnyValue `json:"value"`
}

func anyValue(v any) jsonAnyValue {
	str := func(s string) jsonAnyValue { return jsonAnyValue{StringValue: &s} }
	i64 := func(i int64) jsonAnyValue { s := strconv.FormatInt(i, 10); return jsonAnyValue{IntValue: &s} }
	f64 := func(f float64) jsonAnyValue {
		if math.IsNaN(f) || math.IsInf(f, 0) { // not representable in JSON
			return str(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return jsonAnyValue{DoubleValue: &f}
	}
	switch x := v.(type) {
	case string:
		return str(x)
	case bool:
		return jsonAnyValue{BoolValue: &x}
	case int:
		return i64(int64(x))
	case int32:
		return i64(int64(x))
	case int64:
		return i64(x)
	case uint32:
		return i64(int64(x))
	case uint64:
		return i64(int64(x))
	case float32:
		return f64(float64(x))
	case float64:
		return f64(x)
	case []string:
		arr := &jsonArrayValue{Values: make([]jsonAnyValue, 0, len(x))}
		for _, s := range x {
			arr.Values = append(arr.Values, str(s))
		}
		return jsonAnyValue{ArrayValue: arr}
	case []int64:
		arr := &jsonArrayValue{Values: make([]jsonAnyValue, 0, len(x))}
		for _, n := range x {
			arr.Values = append(arr.Values, i64(n))
		}
		return jsonAnyValue{ArrayValue: arr}
	case []float64:
		arr := &jsonArrayValue{Values: make([]jsonAnyValue, 0, len(x))}
		for _, f := range x {
			arr.Values = append(arr.Values, f64(f))
		}
		return jsonAnyValue{ArrayValue: arr}
	case nil:
		return str("")
	default:
		b, _ := json.Marshal(x)
		return str(string(b))
	}
}

func jsonAttrs(kv []KeyValue) []jsonKV {
	out := make([]jsonKV, 0, len(kv))
	for _, a := range kv {
		out = append(out, jsonKV{Key: a.Key, Value: anyValue(a.Value)})
	}
	return out
}

func nanos(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

type jsonResource struct {
	Attributes []jsonKV `json:"attributes"`
}

type jsonScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type jsonEvent struct {
	TimeUnixNano string   `json:"timeUnixNano"`
	Name         string   `json:"name"`
	Attributes   []jsonKV `json:"attributes,omitempty"`
}

type jsonStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type jsonSpan struct {
	TraceID           string      `json:"traceId"`
	SpanID            string      `json:"spanId"`
	TraceState        string      `json:"traceState,omitempty"`
	ParentSpanID      string      `json:"parentSpanId,omitempty"`
	Flags             uint32      `json:"flags,omitempty"`
	Name              string      `json:"name"`
	Kind              int         `json:"kind"`
	StartTimeUnixNano string      `json:"startTimeUnixNano"`
	EndTimeUnixNano   string      `json:"endTimeUnixNano"`
	Attributes        []jsonKV    `json:"attributes,omitempty"`
	Events            []jsonEvent `json:"events,omitempty"`
	Status            *jsonStatus `json:"status,omitempty"`
}

func (s *Span) toJSON() jsonSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	js := jsonSpan{
		TraceID:           s.sc.TraceID.String(),
		SpanID:            s.sc.SpanID.String(),
		TraceState:        s.sc.TraceState,
		Flags:             uint32(s.sc.Flags),
		Name:              s.name,
		Kind:              int(s.kind),
		StartTimeUnixNano: nanos(s.start),
		EndTimeUnixNano:   nanos(s.end),
		Attributes:        jsonAttrs(s.attrs),
	}
	if s.parent.IsValid() {
		js.ParentSpanID = s.parent.String()
		js.Flags |= 0x300 // bits 8-9: the parent is known to be remote (the caller's)
	}
	for _, e := range s.events {
		js.Events = append(js.Events, jsonEvent{TimeUnixNano: nanos(e.Time), Name: e.Name, Attributes: jsonAttrs(e.Attrs)})
	}
	if s.status != StatusUnset {
		js.Status = &jsonStatus{Code: int(s.status)}
		if s.status == StatusError {
			js.Status.Message = s.statusMs
		}
	}
	return js
}

type jsonLog struct {
	TimeUnixNano         string        `json:"timeUnixNano"`
	ObservedTimeUnixNano string        `json:"observedTimeUnixNano"`
	SeverityNumber       int           `json:"severityNumber,omitempty"`
	SeverityText         string        `json:"severityText,omitempty"`
	EventName            string        `json:"eventName,omitempty"`
	Body                 *jsonAnyValue `json:"body,omitempty"`
	Attributes           []jsonKV      `json:"attributes,omitempty"`
	TraceID              string        `json:"traceId,omitempty"`
	SpanID               string        `json:"spanId,omitempty"`
}

func (r LogRecord) toJSON() jsonLog {
	attrs := r.Attrs
	if r.EventName != "" {
		// Receivers that predate the eventName field still see the name.
		attrs = append([]KeyValue{String("event.name", r.EventName)}, attrs...)
	}
	jl := jsonLog{
		TimeUnixNano:         nanos(r.Time),
		ObservedTimeUnixNano: nanos(r.Time),
		SeverityNumber:       r.SeverityNumber,
		SeverityText:         r.SeverityText,
		EventName:            r.EventName,
		Attributes:           jsonAttrs(attrs),
	}
	if r.Body != "" {
		b := anyValue(r.Body)
		jl.Body = &b
	}
	if r.TraceID.IsValid() {
		jl.TraceID = r.TraceID.String()
	}
	if r.SpanID.IsValid() {
		jl.SpanID = r.SpanID.String()
	}
	return jl
}

type jsonPoint struct {
	Attributes        []jsonKV  `json:"attributes,omitempty"`
	StartTimeUnixNano string    `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string    `json:"timeUnixNano"`
	AsDouble          *float64  `json:"asDouble,omitempty"`
	AsInt             *string   `json:"asInt,omitempty"`
	Count             *string   `json:"count,omitempty"`
	Sum               *float64  `json:"sum,omitempty"`
	BucketCounts      []string  `json:"bucketCounts,omitempty"`
	ExplicitBounds    []float64 `json:"explicitBounds,omitempty"`
}

type jsonData struct {
	DataPoints             []jsonPoint `json:"dataPoints"`
	AggregationTemporality int         `json:"aggregationTemporality,omitempty"`
	IsMonotonic            bool        `json:"isMonotonic,omitempty"`
}

type jsonMetric struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	Gauge       *jsonData `json:"gauge,omitempty"`
	Sum         *jsonData `json:"sum,omitempty"`
	Histogram   *jsonData `json:"histogram,omitempty"`
}

const temporalityCumulative = 2

func (m Metric) toJSON() jsonMetric {
	jm := jsonMetric{Name: m.Name, Description: m.Description, Unit: m.Unit}
	d := &jsonData{}
	for _, p := range m.Points {
		jp := jsonPoint{Attributes: jsonAttrs(p.Attrs), TimeUnixNano: nanos(p.Time)}
		if !p.Start.IsZero() {
			jp.StartTimeUnixNano = nanos(p.Start)
		}
		switch m.Kind {
		case MetricHistogram:
			c := strconv.FormatUint(p.Count, 10)
			sum := p.Sum
			jp.Count, jp.Sum = &c, &sum
			jp.ExplicitBounds = p.Bounds
			for _, bc := range p.BucketCounts {
				jp.BucketCounts = append(jp.BucketCounts, strconv.FormatUint(bc, 10))
			}
		default:
			if p.IsInt {
				s := strconv.FormatInt(p.IntValue, 10)
				jp.AsInt = &s
			} else {
				v := p.Value
				jp.AsDouble = &v
			}
		}
		d.DataPoints = append(d.DataPoints, jp)
	}
	switch m.Kind {
	case MetricGauge:
		jm.Gauge = d
	case MetricSum:
		d.AggregationTemporality, d.IsMonotonic = temporalityCumulative, m.Monotonic
		jm.Sum = d
	case MetricHistogram:
		d.AggregationTemporality = temporalityCumulative
		jm.Histogram = d
	}
	return jm
}

// EncodeTraces renders spans as an OTLP ExportTraceServiceRequest JSON body.
func EncodeTraces(resource []KeyValue, scope string, spans []*Span) ([]byte, error) {
	js := make([]jsonSpan, 0, len(spans))
	for _, s := range spans {
		js = append(js, s.toJSON())
	}
	return json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   jsonResource{Attributes: jsonAttrs(resource)},
		"scopeSpans": []any{map[string]any{"scope": jsonScope{Name: scope}, "spans": js}},
	}}})
}

// EncodeLogs renders records as an OTLP ExportLogsServiceRequest JSON body.
func EncodeLogs(resource []KeyValue, scope string, recs []LogRecord) ([]byte, error) {
	jl := make([]jsonLog, 0, len(recs))
	for _, r := range recs {
		jl = append(jl, r.toJSON())
	}
	return json.Marshal(map[string]any{"resourceLogs": []any{map[string]any{
		"resource":  jsonResource{Attributes: jsonAttrs(resource)},
		"scopeLogs": []any{map[string]any{"scope": jsonScope{Name: scope}, "logRecords": jl}},
	}}})
}

// EncodeMetrics renders metrics as an OTLP ExportMetricsServiceRequest JSON body.
func EncodeMetrics(resource []KeyValue, scope string, ms []Metric) ([]byte, error) {
	jm := make([]jsonMetric, 0, len(ms))
	for _, m := range ms {
		jm = append(jm, m.toJSON())
	}
	return json.Marshal(map[string]any{"resourceMetrics": []any{map[string]any{
		"resource":     jsonResource{Attributes: jsonAttrs(resource)},
		"scopeMetrics": []any{map[string]any{"scope": jsonScope{Name: scope}, "metrics": jm}},
	}}})
}
