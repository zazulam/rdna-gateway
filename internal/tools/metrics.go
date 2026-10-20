package tools

// Read-only access to the lab's Prometheus and the Grafana dashboard that
// charts it, so agents can answer "what's the lab costing today?" with the
// same queries the dashboard uses. Only the query APIs are called; nothing is
// written, and Grafana itself (and its login) is never involved.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prom queries a Prometheus server's HTTP API.
type Prom struct {
	Base string
	HTTP *http.Client
}

func NewProm(base string) *Prom {
	return &Prom{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Series is one result series: its labels and samples (one for an instant query).
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

type Sample struct {
	T time.Time
	V float64
}

func (p *Prom) get(ctx context.Context, path string, q url.Values, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Status    string          `json:"status"`
		Data      json.RawMessage `json:"data"`
		ErrorType string          `json:"errorType"`
		Error     string          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&env); err != nil {
		return fmt.Errorf("prometheus %s: %s", path, resp.Status)
	}
	if env.Status != "success" {
		return fmt.Errorf("prometheus: %s: %s", env.ErrorType, env.Error)
	}
	return json.Unmarshal(env.Data, into)
}

type promResult struct {
	ResultType string `json:"resultType"`
	Result     json.RawMessage
}

type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
	Values [][]any           `json:"values"`
}

func sample(v []any) (Sample, bool) {
	if len(v) != 2 {
		return Sample{}, false
	}
	ts, _ := v[0].(float64)
	s, _ := v[1].(string)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return Sample{}, false
	}
	return Sample{T: time.Unix(0, int64(ts*1e9)), V: f}, true
}

func decode(r promResult) ([]Series, error) {
	switch r.ResultType {
	case "scalar":
		var v []any
		json.Unmarshal(r.Result, &v)
		s, ok := sample(v)
		if !ok {
			return nil, errors.New("bad scalar")
		}
		return []Series{{Labels: map[string]string{}, Samples: []Sample{s}}}, nil
	case "vector", "matrix":
		var raw []promSeries
		if err := json.Unmarshal(r.Result, &raw); err != nil {
			return nil, err
		}
		out := make([]Series, 0, len(raw))
		for _, x := range raw {
			s := Series{Labels: x.Metric}
			if x.Value != nil {
				if v, ok := sample(x.Value); ok {
					s.Samples = append(s.Samples, v)
				}
			}
			for _, pt := range x.Values {
				if v, ok := sample(pt); ok {
					s.Samples = append(s.Samples, v)
				}
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported result type %q", r.ResultType)
}

// Grafana-only variables a dashboard query may use, replaced so the query
// runs as plain PromQL.
var grafanaVars = strings.NewReplacer("$__rate_interval", "5m", "${__rate_interval}", "5m",
	"$__interval", "1m", "${__interval}", "1m", "$__range", "24h", "${__range}", "24h")

// Instant evaluates query now.
func (p *Prom) Instant(ctx context.Context, query string) ([]Series, error) {
	var r promResult
	if err := p.get(ctx, "/api/v1/query", url.Values{"query": {grafanaVars.Replace(query)}}, &r); err != nil {
		return nil, err
	}
	return decode(r)
}

// Range evaluates query over the last `over`, with about `points` steps.
func (p *Prom) Range(ctx context.Context, query string, over time.Duration, points int) ([]Series, time.Duration, error) {
	end := time.Now()
	step := max(over/time.Duration(max(points, 1)), 15*time.Second).Round(time.Second)
	q := url.Values{
		"query": {grafanaVars.Replace(query)},
		"start": {strconv.FormatInt(end.Add(-over).Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds()))},
	}
	var r promResult
	if err := p.get(ctx, "/api/v1/query_range", q, &r); err != nil {
		return nil, 0, err
	}
	s, err := decode(r)
	return s, step, err
}

// MetricInfo is a metric name with its type and help text, if known.
type MetricInfo struct{ Name, Type, Help string }

// Names lists metric names containing every word of filter (case-insensitive).
func (p *Prom) Names(ctx context.Context, filter string) ([]MetricInfo, error) {
	var names []string
	if err := p.get(ctx, "/api/v1/label/__name__/values", url.Values{}, &names); err != nil {
		return nil, err
	}
	var meta map[string][]struct{ Type, Help string }
	_ = p.get(ctx, "/api/v1/metadata", url.Values{}, &meta) // best effort; recording rules have none
	words := strings.Fields(strings.ToLower(filter))
	var out []MetricInfo
	for _, n := range names {
		m := MetricInfo{Name: n}
		if md := meta[n]; len(md) > 0 {
			m.Type, m.Help = md[0].Type, md[0].Help
		}
		hay := strings.ToLower(n + " " + m.Help)
		ok := true
		for _, w := range words {
			ok = ok && strings.Contains(hay, w)
		}
		if ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// LabelString renders labels as {a="b", c="d"}, sorted, without __name__ first.
func LabelString(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	if n := l["__name__"]; n != "" {
		parts = append(parts, n)
	}
	var kv []string
	for _, k := range keys {
		if k != "__name__" {
			kv = append(kv, fmt.Sprintf("%s=%q", k, l[k]))
		}
	}
	if len(kv) > 0 {
		parts = append(parts, "{"+strings.Join(kv, ", ")+"}")
	}
	if len(parts) == 0 {
		return "{}"
	}
	return strings.Join(parts, "")
}

// Num formats a value compactly: integers plainly, others to 4 significant digits.
func Num(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 0):
		return fmt.Sprint(v)
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// Panel is one dashboard panel and the queries behind it.
type Panel struct {
	Dashboard, Section, Title, Type, Unit, Description string
	Queries                                            []PanelQuery
}

type PanelQuery struct{ Expr, Legend string }

type grafanaPanel struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	FieldConfig struct {
		Defaults struct {
			Unit string `json:"unit"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Targets []struct {
		Expr         string `json:"expr"`
		LegendFormat string `json:"legendFormat"`
	} `json:"targets"`
	Panels []grafanaPanel `json:"panels"` // collapsed rows nest their panels
}

// Dashboards reads every Grafana dashboard JSON file in dir.
func Dashboards(dir string) ([]Panel, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no dashboard JSON files in %s", dir)
	}
	var out []Panel
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var d struct {
			Title  string         `json:"title"`
			Panels []grafanaPanel `json:"panels"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		section := ""
		var walk func([]grafanaPanel)
		walk = func(ps []grafanaPanel) {
			for _, p := range ps {
				if p.Type == "row" {
					section = p.Title
					walk(p.Panels)
					continue
				}
				pn := Panel{Dashboard: d.Title, Section: section, Title: p.Title, Type: p.Type,
					Unit: p.FieldConfig.Defaults.Unit, Description: p.Description}
				for _, t := range p.Targets {
					if strings.TrimSpace(t.Expr) != "" {
						pn.Queries = append(pn.Queries, PanelQuery{Expr: t.Expr, Legend: t.LegendFormat})
					}
				}
				if len(pn.Queries) > 0 {
					out = append(out, pn)
				}
			}
		}
		walk(d.Panels)
	}
	return out, nil
}

var durRE = regexp.MustCompile(`^(\d+)([mhdw])$`)

// ParseWindow parses a lookback like "30m", "6h", "7d" or "2w".
func ParseWindow(s string) (time.Duration, error) {
	m := durRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("window %q: use a number and m, h, d or w, e.g. 6h or 7d", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}
