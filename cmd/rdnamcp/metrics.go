package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zazulam/rdna-gateway/internal/mcp"
	"github.com/zazulam/rdna-gateway/internal/tools"
)

const maxSeries = 40

// metricsTools are the read-only lab metrics tools: the dashboard's panels and
// their queries, PromQL now and over a window, and metric-name search.
func metricsTools(p *tools.Prom, dashboards string) []mcp.Tool {
	ts := []mcp.Tool{
		{
			Name: "metrics_query",
			Description: "Run a PromQL query against the lab's Prometheus and return the current value of each series. " +
				"Read-only. Use lab_dashboard to find the queries the Grafana dashboard uses (cost, power, solar, " +
				"temperatures, tokens/s), or metrics_names to find metrics.",
			Schema: schema(map[string]any{"query": str("PromQL expression, e.g. lab:power_watts:sum")}, "query"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var q struct{ Query string }
				json.Unmarshal(a, &q)
				if strings.TrimSpace(q.Query) == "" {
					return "", errors.New("query is required")
				}
				ss, err := p.Instant(ctx, q.Query)
				if err != nil {
					return "", err
				}
				return formatInstant(ss), nil
			},
		},
		{
			Name: "metrics_range",
			Description: "Run a PromQL query over a recent window (e.g. 6h, 7d) and summarize each series: " +
				"min, mean, max, last, plus a short downsampled series. Read-only.",
			Schema: schema(map[string]any{"query": str("PromQL expression"),
				"window": str("lookback: number + m/h/d/w, default 6h, max 30d"),
				"points": num("samples in the summary series, 2-48, default 12")}, "query"),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var q struct {
					Query, Window string
					Points        int
				}
				json.Unmarshal(a, &q)
				if strings.TrimSpace(q.Query) == "" {
					return "", errors.New("query is required")
				}
				if q.Window == "" {
					q.Window = "6h"
				}
				over, err := tools.ParseWindow(q.Window)
				if err != nil {
					return "", err
				}
				if over > 30*24*time.Hour {
					return "", errors.New("window is at most 30d")
				}
				// Fetch at a finer step than shown so min/max are meaningful.
				ss, step, err := p.Range(ctx, q.Query, over, 240)
				if err != nil {
					return "", err
				}
				return formatRange(ss, q.Window, step, clamp(q.Points, 2, 48, 12)), nil
			},
		},
		{
			Name:        "metrics_names",
			Description: "Find lab metric names (and their help text) containing all the given words, e.g. \"gpu temp\" or \"envoy\".",
			Schema:      schema(map[string]any{"filter": str("words to match; empty lists everything")}),
			Call: func(ctx context.Context, a json.RawMessage) (string, error) {
				var q struct{ Filter string }
				json.Unmarshal(a, &q)
				ms, err := p.Names(ctx, q.Filter)
				if err != nil {
					return "", err
				}
				if len(ms) == 0 {
					return "No matching metrics.", nil
				}
				var b strings.Builder
				for i, m := range ms {
					if i == 150 {
						fmt.Fprintf(&b, "… and %d more; narrow the filter.\n", len(ms)-i)
						break
					}
					b.WriteString(m.Name)
					if m.Type != "" {
						fmt.Fprintf(&b, " (%s)", m.Type)
					}
					if m.Help != "" {
						fmt.Fprintf(&b, ": %s", m.Help)
					}
					b.WriteByte('\n')
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
	}
	if dashboards == "" {
		return ts
	}
	return append(ts, mcp.Tool{
		Name: "lab_dashboard",
		Description: "List the Grafana dashboard's panels (section, title, unit) with the PromQL behind each, so you can " +
			"answer with the same numbers the dashboard shows. Pass run=true to also evaluate each listed panel now.",
		Schema: schema(map[string]any{"filter": str("words to match in section or title, e.g. \"cost\", \"solar\", \"temperature\""),
			"run": map[string]any{"type": "boolean", "description": "evaluate the matching panels' queries now (at most 12 panels)"}}),
		Call: func(ctx context.Context, a json.RawMessage) (string, error) {
			var q struct {
				Filter string
				Run    bool
			}
			json.Unmarshal(a, &q)
			panels, err := tools.Dashboards(dashboards)
			if err != nil {
				return "", err
			}
			words := strings.Fields(strings.ToLower(q.Filter))
			var b strings.Builder
			n := 0
			for _, pn := range panels {
				hay := strings.ToLower(pn.Section + " " + pn.Title + " " + pn.Description)
				ok := true
				for _, w := range words {
					ok = ok && strings.Contains(hay, w)
				}
				if !ok {
					continue
				}
				n++
				fmt.Fprintf(&b, "## %s / %s", pn.Section, pn.Title)
				if pn.Unit != "" && pn.Unit != "none" {
					fmt.Fprintf(&b, " [unit: %s]", pn.Unit)
				}
				b.WriteByte('\n')
				if pn.Description != "" {
					b.WriteString(pn.Description + "\n")
				}
				for _, pq := range pn.Queries {
					fmt.Fprintf(&b, "query: %s\n", pq.Expr)
					if q.Run && n <= 12 {
						ss, err := p.Instant(ctx, pq.Expr)
						if err != nil {
							fmt.Fprintf(&b, "  error: %v\n", err)
						} else {
							for _, line := range strings.Split(formatInstant(ss), "\n") {
								b.WriteString("  " + line + "\n")
							}
						}
					}
				}
				b.WriteByte('\n')
			}
			if n == 0 {
				return "No panels match; try a broader filter or none.", nil
			}
			out := strings.TrimSpace(b.String())
			if q.Run && n > 12 {
				out += fmt.Sprintf("\n\n(Evaluated the first 12 of %d panels; narrow the filter to run the rest.)", n)
			}
			out += "\n\nGrafana-only variables ($__rate_interval etc.) are replaced with 5m/1m/24h when run here."
			return out, nil
		},
	})
}

func formatInstant(ss []tools.Series) string {
	if len(ss) == 0 {
		return "No data (empty result)."
	}
	var b strings.Builder
	for i, s := range ss {
		if i == maxSeries {
			fmt.Fprintf(&b, "… and %d more series; aggregate or filter the query.\n", len(ss)-i)
			break
		}
		if len(s.Samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s = %s\n", tools.LabelString(s.Labels), tools.Num(s.Samples[len(s.Samples)-1].V))
	}
	return strings.TrimSpace(b.String())
}

func formatRange(ss []tools.Series, window string, step time.Duration, points int) string {
	if len(ss) == 0 {
		return "No data in the last " + window + "."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Last %s (step %s):\n", window, step)
	for i, s := range ss {
		if i == maxSeries {
			fmt.Fprintf(&b, "… and %d more series; aggregate or filter the query.\n", len(ss)-i)
			break
		}
		if len(s.Samples) == 0 {
			continue
		}
		lo, hi, sum := math.Inf(1), math.Inf(-1), 0.0
		for _, x := range s.Samples {
			lo, hi, sum = math.Min(lo, x.V), math.Max(hi, x.V), sum+x.V
		}
		last := s.Samples[len(s.Samples)-1]
		fmt.Fprintf(&b, "\n%s\n  min %s, mean %s, max %s, last %s (%s)\n  series:", tools.LabelString(s.Labels),
			tools.Num(lo), tools.Num(sum/float64(len(s.Samples))), tools.Num(hi), tools.Num(last.V), last.T.Local().Format("Jan 2 15:04"))
		stride := max(1, (len(s.Samples)+points-1)/points)
		for j := 0; j < len(s.Samples); j += stride {
			x := s.Samples[j]
			fmt.Fprintf(&b, " %s=%s", x.T.Local().Format("15:04"), tools.Num(x.V))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}
