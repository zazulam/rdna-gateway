package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zazulam/rdna-gateway/internal/otlp"
)

// Outcomes of a proxied request, as span attributes and metric labels.
const (
	outcomeOK         = "ok"
	outcomeError      = "error"
	outcomeStalled    = "stalled"
	outcomeClientGone = "client_gone"
)

// call is one proxied request's telemetry: its span (nil when tracing is
// off), timings and what the worker reported. It never holds prompt or
// completion text.
type call struct {
	g      *Gateway
	span   *otlp.Span
	start  time.Time
	op     string
	model  string
	stream bool

	// Set per attempt; the last attempt is the one that answered.
	worker   *Worker
	route    string
	matched  int
	retries  int
	status   int
	firstTok time.Time
	stats    upstreamStats

	outcome   string
	errorType string
}

// operationName maps an endpoint to the GenAI operation it performs.
func operationName(path string) string {
	switch path {
	case "/v1/chat/completions":
		return "chat"
	case "/v1/completions":
		return "text_completion"
	case "/v1/embeddings":
		return "embeddings"
	case "/v1/rerank":
		return "rerank"
	}
	return strings.TrimPrefix(path, "/v1/")
}

// startCall opens the request's span, parented on the caller's traceparent
// when it sent one, and puts the span's traceparent on the response so the
// caller can log which trace the call belonged to.
func (g *Gateway) startCall(rw http.ResponseWriter, req *http.Request, parsed map[string]any, model string) *call {
	c := &call{g: g, start: time.Now(), op: operationName(req.URL.Path), model: model}
	c.stream, _ = parsed["stream"].(bool)
	if !g.otel.TracesEnabled() {
		return c
	}
	parent, _ := otlp.ParseTraceparent(req.Header.Get("traceparent"))
	if parent.IsValid() {
		parent.TraceState = req.Header.Get("tracestate")
	}
	name := c.op
	if model != "" {
		name += " " + model
	}
	attrs := []otlp.KeyValue{
		otlp.String("gen_ai.operation.name", c.op),
		otlp.String("gen_ai.provider.name", "llama.cpp"),
	}
	if model != "" {
		attrs = append(attrs, otlp.String("gen_ai.request.model", model))
	}
	// Request parameters only: numbers, never the messages themselves.
	for _, p := range []struct {
		key, attr string
		isInt     bool
	}{
		{"max_tokens", "gen_ai.request.max_tokens", true},
		{"max_completion_tokens", "gen_ai.request.max_tokens", true},
		{"temperature", "gen_ai.request.temperature", false},
		{"top_p", "gen_ai.request.top_p", false},
		{"seed", "gen_ai.request.seed", true},
	} {
		v, ok := parsed[p.key].(float64)
		switch {
		case !ok || (p.key == "max_completion_tokens" && parsed["max_tokens"] != nil):
		case p.isInt:
			attrs = append(attrs, otlp.Int64(p.attr, int64(v)))
		default:
			attrs = append(attrs, otlp.Float(p.attr, v))
		}
	}
	c.span = g.otel.Start(parent, name, otlp.KindClient, attrs...)
	if sc := c.span.Context(); sc.IsValid() {
		rw.Header().Set("traceparent", sc.Traceparent())
	}
	return c
}

// propagate copies W3C trace context to the upstream request. The gateway's
// own span becomes the worker's parent when there is one; otherwise the
// caller's traceparent passes through unchanged.
func (c *call) propagate(up, in http.Header) {
	for _, k := range []string{"tracestate", "baggage"} {
		for _, v := range in.Values(k) {
			up.Add(k, v)
		}
	}
	if sc := c.span.Context(); sc.IsValid() {
		up.Set("traceparent", sc.Traceparent())
	} else if tp := in.Get("traceparent"); tp != "" {
		up.Set("traceparent", tp)
	}
}

// attempt starts a new try on d.Worker, forgetting the previous try's results.
func (c *call) attempt(d Decision) {
	c.worker, c.route, c.matched = d.Worker, d.Reason, d.Matched
	c.status, c.firstTok, c.stats = 0, time.Time{}, upstreamStats{}
}

// retry records a failed attempt that will be tried again.
func (c *call) retry(worker, errorType string, same bool) {
	c.retries++
	c.span.AddEvent("rdnagw.retry",
		otlp.String("server.address", worker),
		otlp.String("error.type", errorType),
		otlp.Bool("rdnagw.retry.same_worker", same))
}

// fail marks the call failed with a low-cardinality error type.
func (c *call) fail(outcome, errorType string) {
	c.outcome, c.errorType = outcome, errorType
}

// observeLine looks at one line of the worker's response as it streams past:
// the first one carrying generated tokens sets time to first token, and the
// final chunk (or a whole JSON body) carries usage and llama-server timings.
// Content is inspected only to see whether it is empty, never kept.
func (c *call) observeLine(line []byte, now time.Time) (final upstreamStats, ok bool) {
	if c.stream && c.firstTok.IsZero() && hasToken(line) {
		c.firstTok = now
	}
	if bytes.Contains(line, []byte(`"usage"`)) || bytes.Contains(line, []byte(`"timings"`)) ||
		bytes.Contains(line, []byte(`"finish_reason":"`)) {
		if u, ok := parseStats(line); ok {
			c.stats.merge(u)
			return u, true
		}
	}
	return upstreamStats{}, false
}

// hasToken reports whether an SSE data line carries generated text, reasoning
// or a tool call.
func hasToken(line []byte) bool {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	line = bytes.TrimSpace(line[5:])
	if len(line) == 0 || line[0] != '{' {
		return false // [DONE], keep-alives
	}
	var ch struct {
		Choices []struct {
			Delta struct {
				Content   *string           `json:"content"`
				Reasoning *string           `json:"reasoning_content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			Text *string `json:"text"`
		} `json:"choices"`
		Content *string `json:"content"` // llama-server's native /completion stream
	}
	if json.Unmarshal(line, &ch) != nil {
		return false
	}
	nonEmpty := func(s *string) bool { return s != nil && *s != "" }
	for _, c := range ch.Choices {
		if nonEmpty(c.Delta.Content) || nonEmpty(c.Delta.Reasoning) || len(c.Delta.ToolCalls) > 0 || nonEmpty(c.Text) {
			return true
		}
	}
	return nonEmpty(ch.Content)
}

// upstreamStats is what a worker reports about a finished request.
type upstreamStats struct {
	ID, Model     string
	FinishReasons []string
	HasUsage      bool
	PromptTokens  int64
	OutputTokens  int64
	CachedTokens  int64
	HasTimings    bool
	PromptN       int64
	PromptMS      float64
	PromptPS      float64
	PredictedN    int64
	PredictedMS   float64
}

func (s *upstreamStats) merge(u upstreamStats) {
	if u.ID != "" {
		s.ID = u.ID
	}
	if u.Model != "" {
		s.Model = u.Model
	}
	s.FinishReasons = append(s.FinishReasons, u.FinishReasons...)
	if u.HasUsage {
		s.HasUsage, s.PromptTokens, s.OutputTokens = true, u.PromptTokens, u.OutputTokens
		if u.CachedTokens > s.CachedTokens {
			s.CachedTokens = u.CachedTokens
		}
	}
	if u.HasTimings {
		s.HasTimings, s.PromptN, s.PromptMS, s.PromptPS = true, u.PromptN, u.PromptMS, u.PromptPS
		s.PredictedN, s.PredictedMS = u.PredictedN, u.PredictedMS
		if u.CachedTokens > s.CachedTokens {
			s.CachedTokens = u.CachedTokens
		}
	}
}

// parseStats reads ids, finish reasons, usage and timings from one SSE chunk
// or a whole JSON response body.
func parseStats(line []byte) (upstreamStats, bool) {
	line = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
	var u struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Timings *struct {
			CacheN      int64   `json:"cache_n"`
			PromptN     int64   `json:"prompt_n"`
			PromptMS    float64 `json:"prompt_ms"`
			PromptPS    float64 `json:"prompt_per_second"`
			PredictedN  int64   `json:"predicted_n"`
			PredictedMS float64 `json:"predicted_ms"`
		} `json:"timings"`
	}
	if json.Unmarshal(line, &u) != nil {
		return upstreamStats{}, false
	}
	s := upstreamStats{ID: u.ID, Model: u.Model}
	for _, ch := range u.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.FinishReasons = append(s.FinishReasons, *ch.FinishReason)
		}
	}
	if u.Usage != nil {
		s.HasUsage = true
		s.PromptTokens, s.OutputTokens = u.Usage.PromptTokens, u.Usage.CompletionTokens
		s.CachedTokens = u.Usage.PromptTokensDetails.CachedTokens
	}
	if t := u.Timings; t != nil {
		s.HasTimings = true
		s.PromptN, s.PromptMS, s.PromptPS = t.PromptN, t.PromptMS, t.PromptPS
		s.PredictedN, s.PredictedMS = t.PredictedN, t.PredictedMS
		if t.CacheN > s.CachedTokens {
			s.CachedTokens = t.CacheN
		}
	}
	return s, true
}

// ttft is the time to first token: measured at the proxy for streams, and
// unknown (0) otherwise.
func (c *call) ttft() time.Duration {
	if c.firstTok.IsZero() {
		return 0
	}
	return c.firstTok.Sub(c.start)
}

// end closes the span with everything learned about the call.
func (c *call) end() {
	if c.outcome == "" {
		c.outcome = outcomeOK
		if c.status >= 400 {
			c.fail(outcomeError, strconv.Itoa(c.status))
		}
	}
	c.g.lat.record(c, time.Now())
	if c.span == nil {
		return
	}
	s := c.stats
	attrs := []otlp.KeyValue{
		otlp.Int("rdnagw.retries", c.retries),
		otlp.String("rdnagw.outcome", c.outcome),
	}
	if c.worker != nil {
		attrs = append(attrs,
			otlp.String("server.address", c.worker.Name),
			otlp.String("rdnagw.route", c.route),
			otlp.Int("rdnagw.matched_blocks", c.matched))
	}
	if c.status != 0 {
		attrs = append(attrs, otlp.Int("http.response.status_code", c.status))
	}
	if s.Model != "" {
		attrs = append(attrs, otlp.String("gen_ai.response.model", s.Model))
	}
	if s.ID != "" {
		attrs = append(attrs, otlp.String("gen_ai.response.id", s.ID))
	}
	if len(s.FinishReasons) > 0 {
		attrs = append(attrs, otlp.Strings("gen_ai.response.finish_reasons", s.FinishReasons))
	}
	if s.HasUsage || s.HasTimings {
		in, out := s.PromptTokens, s.OutputTokens
		if !s.HasUsage { // timings only: prompt_n counts just the uncached part
			in, out = s.PromptN+s.CachedTokens, s.PredictedN
		}
		attrs = append(attrs,
			otlp.Int64("gen_ai.usage.input_tokens", in),
			otlp.Int64("gen_ai.usage.output_tokens", out),
			otlp.Int64("gen_ai.usage.cache_read.input_tokens", s.CachedTokens))
	}
	if s.HasTimings {
		attrs = append(attrs,
			otlp.Float("rdnagw.worker.prompt_ms", s.PromptMS),
			otlp.Float("rdnagw.worker.predicted_ms", s.PredictedMS))
	}
	if t := c.ttft(); t > 0 {
		attrs = append(attrs, otlp.Float("gen_ai.server.time_to_first_token", t.Seconds()))
	}
	if c.errorType != "" {
		attrs = append(attrs, otlp.String("error.type", c.errorType))
		c.span.SetStatus(otlp.StatusError, c.errorType)
	}
	c.span.SetAttributes(attrs...)
	c.span.End()
}
