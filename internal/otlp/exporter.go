package otlp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for the batching exporter.
const (
	DefaultQueueSize    = 2048
	DefaultBatchSize    = 512
	DefaultBatchTimeout = 2 * time.Second
	DefaultTimeout      = 3 * time.Second
)

// Exporter batches spans and log records and posts them in the background.
// A nil *Exporter is valid and does nothing, so callers can hold one
// unconditionally. Nothing on the caller's path blocks: when the queue is
// full, items are dropped and counted.
type Exporter struct {
	cfg      Config
	scope    string
	resource []KeyValue
	client   *http.Client
	logf     func(string, ...any)

	tracesURL, logsURL, metricsURL string

	spans chan *Span
	logs  chan LogRecord
	kick  chan struct{}
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once

	batchSize    int
	batchTimeout time.Duration

	droppedSpans, droppedLogs  atomic.Int64
	exportedSpans, exportedLog atomic.Int64
	exportErrors               atomic.Int64
	lastErrLog                 atomic.Int64 // unix seconds, rate-limits error logging
}

// Options tune an exporter; the zero value uses the defaults.
type Options struct {
	QueueSize    int
	BatchSize    int
	BatchTimeout time.Duration
	Logf         func(string, ...any) // export failures, at most once a minute
	Client       *http.Client
}

// NewExporter starts an exporter for cfg, or returns nil when cfg exports
// nothing. scope names the instrumentation (e.g. the Go package path) and
// defaultService is used when cfg has no service name. Call Shutdown to flush.
func NewExporter(cfg Config, scope, defaultService string, o Options) *Exporter {
	if !cfg.Enabled() {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.BatchTimeout <= 0 {
		o.BatchTimeout = DefaultBatchTimeout
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: cfg.Timeout}
	}
	svc := cfg.ServiceName
	if svc == "" {
		svc = defaultService
	}
	res := []KeyValue{String("service.name", svc), String("telemetry.sdk.language", "go")}
	keys := make([]string, 0, len(cfg.ResourceAttributes))
	for k := range cfg.ResourceAttributes {
		if k != "service.name" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		res = append(res, String(k, cfg.ResourceAttributes[k]))
	}
	for _, w := range cfg.Warnings() {
		o.Logf("otlp: %s", w)
	}
	e := &Exporter{
		cfg: cfg, scope: scope, resource: res, client: o.Client, logf: o.Logf,
		tracesURL: cfg.TracesURL(), logsURL: cfg.LogsURL(), metricsURL: cfg.MetricsURL(),
		spans: make(chan *Span, o.QueueSize), logs: make(chan LogRecord, o.QueueSize),
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		batchSize: o.BatchSize, batchTimeout: o.BatchTimeout,
	}
	go e.run()
	return e
}

// TracesEnabled reports whether spans go anywhere.
func (e *Exporter) TracesEnabled() bool { return e != nil && e.tracesURL != "" }

// LogsEnabled reports whether log records go anywhere.
func (e *Exporter) LogsEnabled() bool { return e != nil && e.logsURL != "" }

// MetricsEnabled reports whether ExportMetrics has somewhere to post.
func (e *Exporter) MetricsEnabled() bool { return e != nil && e.metricsURL != "" }

// Start begins a span. parent is the caller's context (from a traceparent
// header) or the zero value for a new trace. It returns nil — a no-op span —
// when traces are off or the caller's context says it isn't sampling.
func (e *Exporter) Start(parent SpanContext, name string, kind SpanKind, attrs ...KeyValue) *Span {
	if !e.TracesEnabled() {
		return nil
	}
	if parent.IsValid() && !parent.Sampled() {
		return nil // parent-based sampling: respect the caller's decision
	}
	s := &Span{exp: e, name: name, kind: kind, start: time.Now(), attrs: attrs}
	if parent.IsValid() {
		s.sc = SpanContext{TraceID: parent.TraceID, SpanID: NewSpanID(), Flags: parent.Flags, TraceState: parent.TraceState}
		s.parent = parent.SpanID
	} else {
		s.sc = SpanContext{TraceID: NewTraceID(), SpanID: NewSpanID(), Flags: FlagSampled}
	}
	return s
}

func (e *Exporter) enqueueSpan(s *Span) {
	select {
	case <-e.stop:
		e.droppedSpans.Add(1)
		return
	default:
	}
	select {
	case e.spans <- s:
		e.kickIfFull(len(e.spans))
	default:
		e.droppedSpans.Add(1)
	}
}

// Emit queues a log record. It never blocks; a full queue drops the record.
func (e *Exporter) Emit(r LogRecord) {
	if !e.LogsEnabled() {
		return
	}
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	select {
	case <-e.stop:
		e.droppedLogs.Add(1)
		return
	default:
	}
	select {
	case e.logs <- r:
		e.kickIfFull(len(e.logs))
	default:
		e.droppedLogs.Add(1)
	}
}

// ExportMetrics posts metrics now, synchronously, bounded by the export
// timeout. Call it from your own ticker, never from a request path.
func (e *Exporter) ExportMetrics(ctx context.Context, ms []Metric) error {
	if !e.MetricsEnabled() || len(ms) == 0 {
		return nil
	}
	body, err := EncodeMetrics(e.resource, e.scope, ms)
	if err != nil {
		return err
	}
	return e.post(ctx, e.metricsURL, body)
}

// Stats are the exporter's own counters.
type Stats struct {
	DroppedSpans, DroppedLogs   int64
	ExportedSpans, ExportedLogs int64
	ExportErrors                int64
}

// Stats returns the exporter's counters (zero for a nil exporter).
func (e *Exporter) Stats() Stats {
	if e == nil {
		return Stats{}
	}
	return Stats{
		DroppedSpans: e.droppedSpans.Load(), DroppedLogs: e.droppedLogs.Load(),
		ExportedSpans: e.exportedSpans.Load(), ExportedLogs: e.exportedLog.Load(),
		ExportErrors: e.exportErrors.Load(),
	}
}

// Shutdown flushes what's queued and stops the exporter. Spans ended after
// it are dropped. It returns when the flush is done or ctx expires.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.once.Do(func() { close(e.stop) })
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ForceFlush exports everything queued so far, then returns.
func (e *Exporter) ForceFlush(ctx context.Context) {
	if e == nil {
		return
	}
	e.flushSpans(ctx)
	e.flushLogs(ctx)
}

func (e *Exporter) run() {
	defer close(e.done)
	t := time.NewTicker(e.batchTimeout)
	defer t.Stop()
	for {
		select {
		case <-e.stop:
			ctx, cancel := context.WithTimeout(context.Background(), 2*e.cfg.Timeout)
			e.ForceFlush(ctx)
			cancel()
			return
		case <-t.C:
		case <-e.kick: // a full batch is waiting
		}
		e.ForceFlush(context.Background())
	}
}

func (e *Exporter) kickIfFull(n int) {
	if n >= e.batchSize {
		select {
		case e.kick <- struct{}{}:
		default:
		}
	}
}

// flushSpans exports what is queued, one batch per request.
func (e *Exporter) flushSpans(ctx context.Context) {
	for {
		var batch []*Span
	fill:
		for len(batch) < e.batchSize {
			select {
			case s := <-e.spans:
				batch = append(batch, s)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			return
		}
		body, err := EncodeTraces(e.resource, e.scope, batch)
		if err == nil {
			err = e.post(ctx, e.tracesURL, body)
		}
		if err != nil {
			e.droppedSpans.Add(int64(len(batch)))
		} else {
			e.exportedSpans.Add(int64(len(batch)))
		}
		if len(batch) < e.batchSize {
			return
		}
	}
}

func (e *Exporter) flushLogs(ctx context.Context) {
	for {
		var batch []LogRecord
	fill:
		for len(batch) < e.batchSize {
			select {
			case r := <-e.logs:
				batch = append(batch, r)
			default:
				break fill
			}
		}
		if len(batch) == 0 {
			return
		}
		body, err := EncodeLogs(e.resource, e.scope, batch)
		if err == nil {
			err = e.post(ctx, e.logsURL, body)
		}
		if err != nil {
			e.droppedLogs.Add(int64(len(batch)))
		} else {
			e.exportedLog.Add(int64(len(batch)))
		}
		if len(batch) < e.batchSize {
			return
		}
	}
}

func (e *Exporter) post(ctx context.Context, url string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return e.fail(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return e.fail(err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return e.fail(fmt.Errorf("%s: %s", url, resp.Status))
	}
	return nil
}

func (e *Exporter) fail(err error) error {
	e.exportErrors.Add(1)
	now := time.Now().Unix()
	if last := e.lastErrLog.Load(); now-last >= 60 && e.lastErrLog.CompareAndSwap(last, now) {
		e.logf("otlp export failed (further errors for a minute are only counted): %v", err)
	}
	return err
}
