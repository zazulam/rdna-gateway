package gateway

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bucket bounds, in seconds. Time to first token spans a cached short prompt
// (tens of ms) to a cold 100k-token prefill (a minute); per-token decode time
// spans a fast small model (1 ms) to a big one spilling to system RAM (1 s).
var (
	ttftBuckets     = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 4, 8, 15, 30, 60}
	perTokenBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.02, 0.04, 0.08, 0.15, 0.3, 0.6, 1}
	durationBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 160, 320, 600}
	queueBuckets    = []float64{0.005, 0.025, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
)

// histKey is a histogram's label set. Kept small on purpose: no per-session
// or per-user labels.
type histKey struct{ worker, model, route, outcome string }

type histSeries struct {
	counts []uint64 // per bucket, plus +Inf last; not cumulative
	sum    float64
	count  uint64
}

// histogram is a Prometheus histogram vector over histKey.
type histogram struct {
	name, help string
	bounds     []float64

	mu     sync.Mutex
	series map[histKey]*histSeries
}

func newHistogram(name, help string, bounds []float64) *histogram {
	return &histogram{name: name, help: help, bounds: bounds, series: map[histKey]*histSeries{}}
}

func (h *histogram) observe(k histKey, v float64) {
	if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v: buckets are le
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[k]
	if s == nil {
		s = &histSeries{counts: make([]uint64, len(h.bounds)+1)}
		h.series[k] = s
	}
	s.counts[i]++
	s.sum += v
	s.count++
}

// write renders the histogram in Prometheus text exposition format.
func (h *histogram) write(b *strings.Builder) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", h.name, h.help, h.name)
	keys := make([]histKey, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, c := keys[i], keys[j]
		if a.worker != c.worker {
			return a.worker < c.worker
		}
		if a.model != c.model {
			return a.model < c.model
		}
		if a.route != c.route {
			return a.route < c.route
		}
		return a.outcome < c.outcome
	})
	for _, k := range keys {
		s := h.series[k]
		labels := fmt.Sprintf("worker=%q,model=%q,route=%q,outcome=%q", k.worker, k.model, k.route, k.outcome)
		var cum uint64
		for i, bound := range h.bounds {
			cum += s.counts[i]
			fmt.Fprintf(b, "%s_bucket{%s,le=%q} %d\n", h.name, labels, strconv.FormatFloat(bound, 'g', -1, 64), cum)
		}
		fmt.Fprintf(b, "%s_bucket{%s,le=\"+Inf\"} %d\n", h.name, labels, s.count)
		fmt.Fprintf(b, "%s_sum{%s} %s\n", h.name, labels, strconv.FormatFloat(s.sum, 'g', -1, 64))
		fmt.Fprintf(b, "%s_count{%s} %d\n", h.name, labels, s.count)
	}
}

// latency holds the request-latency histograms behind /metrics. They are
// named after the OpenTelemetry GenAI server metrics.
type latency struct {
	ttft, perToken, duration, queue *histogram
}

func newLatency() *latency {
	return &latency{
		ttft: newHistogram("gen_ai_server_time_to_first_token_seconds",
			"Streamed requests: request accepted to the first chunk carrying generated text, reasoning or a tool call.", ttftBuckets),
		perToken: newHistogram("gen_ai_server_time_per_output_token_seconds",
			"Decode time per output token, from the worker's timings (or the stream's arrival times).", perTokenBuckets),
		duration: newHistogram("gen_ai_server_request_duration_seconds",
			"Whole request at the gateway, including retries.", durationBuckets),
		queue: newHistogram("rdnagw_queue_wait_seconds",
			"Time not spent in the worker's prefill or decode: waiting for a slot, retries and network.", queueBuckets),
	}
}

func (l *latency) write(b *strings.Builder) {
	for _, h := range []*histogram{l.ttft, l.perToken, l.duration, l.queue} {
		h.write(b)
	}
}

// record files one finished call. Calls that never reached a worker (no
// worker serves the model) aren't recorded.
func (l *latency) record(c *call, end time.Time) {
	if c.worker == nil {
		return
	}
	k := histKey{worker: c.worker.Name, model: c.metricModel(), route: c.route, outcome: c.outcome}
	total := end.Sub(c.start).Seconds()
	l.duration.observe(k, total)
	ttft := c.ttft().Seconds()
	if ttft > 0 {
		l.ttft.observe(k, ttft)
	}
	s := c.stats
	switch {
	case s.HasTimings && s.PredictedN > 0:
		l.perToken.observe(k, s.PredictedMS/1000/float64(s.PredictedN))
	case ttft > 0 && s.OutputTokens > 1:
		l.perToken.observe(k, (total-ttft)/float64(s.OutputTokens-1))
	}
	if s.HasTimings {
		busy := s.PromptMS / 1000 // prefill happens before the first token
		upTo := ttft
		if upTo == 0 { // not streamed: the whole request, minus decoding too
			upTo = total
			busy += s.PredictedMS / 1000
		}
		l.queue.observe(k, math.Max(0, upTo-busy))
	}
}

// metricModel is the requested model as a label, or "other" when the worker
// doesn't list it, so arbitrary client strings can't grow the label set.
func (c *call) metricModel() string {
	for _, m := range c.worker.Models() {
		if m == c.model {
			return m
		}
	}
	return "other"
}
