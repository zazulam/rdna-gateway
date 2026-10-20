package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the gateway's JSON config file.
type Config struct {
	// Listen address, e.g. ":8080" or "100.x.y.z:8080".
	Listen string `json:"listen"`
	// APIKey, if set, is required as "Authorization: Bearer <key>" on /v1/*.
	APIKey string `json:"api_key,omitempty"`
	// Policy is the default routing policy: least-loaded (default), cache-aware,
	// cost-aware or round-robin. A request can override it with the
	// X-Gateway-Policy header. least-loaded measured best on two unequal cards
	// (docs/BENCHMARKS.md): affinity overfills a warm card's slots.
	Policy string `json:"policy,omitempty"`
	// BlockChars is the prefix-index block size in bytes (~4 bytes per token).
	BlockChars int `json:"block_chars,omitempty"`
	// MaxNodes bounds the prefix index.
	MaxNodes int `json:"max_nodes,omitempty"`
	// MinMatchBlocks is the shortest prefix match that's worth routing for.
	MinMatchBlocks int `json:"min_match_blocks,omitempty"`
	// HealthInterval is how often workers are polled, e.g. "5s".
	HealthInterval Duration `json:"health_interval,omitempty"`
	// StallTimeout aborts a response stream that sends nothing for this long,
	// e.g. "2m". It doesn't bound prompt processing before the first byte.
	StallTimeout Duration `json:"stall_timeout,omitempty"`
	// HostMetrics is a local exporter to relay at /metrics/host, e.g.
	// LibreHardwareMonitor's "http://127.0.0.1:8085/metrics" for GPU temperatures
	// on Windows. Its output is regrouped into valid Prometheus text.
	HostMetrics string         `json:"host_metrics,omitempty"`
	Workers     []WorkerConfig `json:"workers"`
}

// WorkerConfig describes one llama-server (or any OpenAI-compatible) backend.
type WorkerConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Slots is the worker's parallel decode slots (llama-server --parallel).
	// Load is measured as in-flight requests / slots.
	Slots int `json:"slots,omitempty"`
	// Models this worker serves. Empty = discover from its /v1/models.
	Models []string `json:"models,omitempty"`
	// Weight scales the worker's capacity when comparing load across unequal
	// GPUs (default 1). A slower card can be given e.g. 0.7.
	Weight float64 `json:"weight,omitempty"`
	// Fallback workers only receive traffic when no primary worker is up,
	// e.g. a laptop that should cover outages but never slow the pool down.
	Fallback bool `json:"fallback,omitempty"`
}

// Duration is a time.Duration that unmarshals from "5s"-style strings.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// LoadConfig reads and validates a config file, filling defaults.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, c.normalize()
}

func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Policy == "" {
		c.Policy = PolicyLeastLoaded
	}
	if _, err := parsePolicy(c.Policy); err != nil {
		return err
	}
	if c.BlockChars <= 0 {
		c.BlockChars = 512
	}
	if c.MaxNodes <= 0 {
		c.MaxNodes = 200_000
	}
	if c.MinMatchBlocks <= 0 {
		c.MinMatchBlocks = 1
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = Duration(5 * time.Second)
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = Duration(2 * time.Minute)
	}
	if len(c.Workers) == 0 {
		return fmt.Errorf("config: no workers")
	}
	seen := map[string]bool{}
	for i := range c.Workers {
		w := &c.Workers[i]
		w.URL = strings.TrimRight(w.URL, "/")
		if w.URL == "" {
			return fmt.Errorf("config: worker %d has no url", i)
		}
		if w.Name == "" {
			w.Name = w.URL
		}
		if seen[w.Name] {
			return fmt.Errorf("config: duplicate worker name %q", w.Name)
		}
		seen[w.Name] = true
		if w.Slots <= 0 {
			w.Slots = 1
		}
		if w.Weight <= 0 {
			w.Weight = 1
		}
	}
	return nil
}
