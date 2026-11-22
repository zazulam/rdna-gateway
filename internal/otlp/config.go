// Package otlp is a small, dependency-free OpenTelemetry exporter: spans, log
// records and metrics encoded as OTLP/HTTP JSON, W3C trace context, and a
// batching exporter that never blocks its caller.
//
// It speaks only OTLP/HTTP with JSON bodies (Content-Type: application/json),
// which every OTLP/HTTP receiver accepts on /v1/traces, /v1/logs and
// /v1/metrics. Configure it with the standard OTEL_* environment variables or
// the equivalent config-file block; with no endpoint it stays off.
package otlp

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config says where and how to export. The JSON tags are the config-file form;
// [Config.WithEnv] layers the standard environment variables on top.
type Config struct {
	// Endpoint is the OTLP/HTTP base URL, e.g. "http://collector:4318". Signal
	// paths (/v1/traces, /v1/logs, /v1/metrics) are appended to it.
	Endpoint string `json:"endpoint,omitempty"`
	// Per-signal URLs, used as-is. They override Endpoint for that signal.
	TracesEndpoint  string `json:"traces_endpoint,omitempty"`
	LogsEndpoint    string `json:"logs_endpoint,omitempty"`
	MetricsEndpoint string `json:"metrics_endpoint,omitempty"`
	// Headers are sent with every export, e.g. an authorization header.
	Headers map[string]string `json:"headers,omitempty"`
	// ServiceName is the resource's service.name.
	ServiceName string `json:"service_name,omitempty"`
	// ResourceAttributes are extra resource attributes, e.g.
	// {"deployment.environment.name": "lab"}.
	ResourceAttributes map[string]string `json:"resource_attributes,omitempty"`
	// Disabled turns export off even when an endpoint is set.
	Disabled bool `json:"disabled,omitempty"`
	// NoTraces / NoLogs turn one signal off (OTEL_TRACES_EXPORTER=none,
	// OTEL_LOGS_EXPORTER=none).
	NoTraces bool `json:"no_traces,omitempty"`
	NoLogs   bool `json:"no_logs,omitempty"`

	// Timeout bounds one export request (OTEL_EXPORTER_OTLP_TIMEOUT, ms).
	// Default 3s.
	Timeout time.Duration `json:"-"`
	// Protocol is OTEL_EXPORTER_OTLP_PROTOCOL as given; only "http/json" is
	// spoken, and anything else is reported by [Config.Warnings].
	Protocol string `json:"-"`
}

// WithEnv returns c with any OTEL_* variables that getenv reports layered on
// top: a set variable overrides the config-file value.
func (c Config) WithEnv(getenv func(string) string) Config {
	if v := getenv("OTEL_SDK_DISABLED"); strings.EqualFold(strings.TrimSpace(v), "true") {
		c.Disabled = true
	}
	if v := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	if v := getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); v != "" {
		c.TracesEndpoint = v
	}
	if v := getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); v != "" {
		c.LogsEndpoint = v
	}
	if v := getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"); v != "" {
		c.MetricsEndpoint = v
	}
	if v := getenv("OTEL_EXPORTER_OTLP_HEADERS"); v != "" {
		h := map[string]string{}
		for k, val := range c.Headers {
			h[k] = val
		}
		for k, val := range parseKV(v) {
			h[k] = val
		}
		c.Headers = h
	}
	if v := getenv("OTEL_SERVICE_NAME"); v != "" {
		c.ServiceName = v
	}
	if v := getenv("OTEL_RESOURCE_ATTRIBUTES"); v != "" {
		ra := map[string]string{}
		for k, val := range parseKV(v) {
			ra[k] = val
		}
		for k, val := range c.ResourceAttributes { // the config file wins over the generic list
			ra[k] = val
		}
		c.ResourceAttributes = ra
	}
	if v := getenv("OTEL_EXPORTER_OTLP_TIMEOUT"); v != "" {
		if ms, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && ms > 0 {
			c.Timeout = time.Duration(ms) * time.Millisecond
		}
	}
	if v := getenv("OTEL_EXPORTER_OTLP_PROTOCOL"); v != "" {
		c.Protocol = v
	}
	if strings.EqualFold(getenv("OTEL_TRACES_EXPORTER"), "none") {
		c.NoTraces = true
	}
	if strings.EqualFold(getenv("OTEL_LOGS_EXPORTER"), "none") {
		c.NoLogs = true
	}
	return c
}

// Warnings lists settings that were understood but can't be honoured.
func (c Config) Warnings() []string {
	var w []string
	if p := strings.ToLower(c.Protocol); p != "" && p != "http/json" {
		w = append(w, "OTEL_EXPORTER_OTLP_PROTOCOL="+c.Protocol+" is not supported; exporting OTLP/HTTP JSON instead")
	}
	return w
}

// Enabled reports whether any signal has somewhere to go.
func (c Config) Enabled() bool {
	return !c.Disabled && (c.TracesURL() != "" || c.LogsURL() != "" || c.MetricsURL() != "")
}

// TracesURL is the URL spans are posted to, or "" when traces are off.
func (c Config) TracesURL() string {
	if c.NoTraces {
		return ""
	}
	return c.signalURL(c.TracesEndpoint, "v1/traces")
}

// LogsURL is the URL log records are posted to, or "" when logs are off.
func (c Config) LogsURL() string {
	if c.NoLogs {
		return ""
	}
	return c.signalURL(c.LogsEndpoint, "v1/logs")
}

// MetricsURL is the URL metrics are posted to, or "".
func (c Config) MetricsURL() string { return c.signalURL(c.MetricsEndpoint, "v1/metrics") }

func (c Config) signalURL(specific, path string) string {
	if c.Disabled {
		return ""
	}
	if specific != "" {
		return specific
	}
	if c.Endpoint == "" {
		return ""
	}
	return strings.TrimRight(c.Endpoint, "/") + "/" + path
}

// parseKV parses the W3C-baggage-like "k1=v1,k2=v2" form the OTEL_* variables
// use, percent-decoding keys and values and skipping malformed entries.
func parseKV(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if dk, err := url.PathUnescape(k); err == nil {
			k = dk
		}
		if dv, err := url.PathUnescape(v); err == nil {
			v = dv
		}
		if k != "" {
			out[k] = v
		}
	}
	return out
}
