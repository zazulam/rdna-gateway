package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zazulam/rdna-gateway/internal/mcp"
	"github.com/zazulam/rdna-gateway/internal/tools"
)

// fakeProm knows a few lab metrics; only envoy_grid_watts{} has a value.
func fakeProm(t *testing.T) *tools.Prom {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch r.URL.Path {
		case "/api/v1/query", "/api/v1/query_range":
			typ, key := "vector", "value"
			val := `[1700000000,"-850"]`
			if r.URL.Path == "/api/v1/query_range" {
				typ, key, val = "matrix", "values", `[[1700000000,"-850"]]`
			}
			res := ""
			if q == "envoy_grid_watts" {
				res = `{"metric":{"__name__":"envoy_grid_watts"},"` + key + `":` + val + `}`
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"` + typ + `","result":[` + res + `]}}`))
		case "/api/v1/label/__name__/values":
			w.Write([]byte(`{"status":"success","data":["envoy_consumption_watts","envoy_grid_watts",
				"envoy_production_watts","lab:power_watts:sum","lab:solar_watts","rdnagw_worker_up"]}`))
		case "/api/v1/metadata":
			w.Write([]byte(`{"status":"success","data":{"envoy_production_watts":[{"type":"gauge","help":"Solar production now"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return tools.NewProm(srv.URL)
}

func call(t *testing.T, ts []mcp.Tool, name string, args any) string {
	t.Helper()
	for _, tl := range ts {
		if tl.Name == name {
			a, _ := json.Marshal(args)
			out, err := tl.Call(context.Background(), a)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return out
		}
	}
	t.Fatalf("no tool %s", name)
	return ""
}

func TestEmptyResultSuggestsRealNames(t *testing.T) {
	ts := metricsTools(fakeProm(t), "")
	for _, tool := range []string{"metrics_query", "metrics_range"} {
		out := call(t, ts, tool, map[string]any{"query": "solar_power_watts"})
		for _, want := range []string{"No data", "probably wrong", "envoy_production_watts", "lab:solar_watts", "lab_dashboard"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q in:\n%s", tool, want, out)
			}
		}
	}
	out := call(t, ts, "metrics_query", map[string]any{"query": `envoy_grid_watts{phase="b"}`})
	if !strings.Contains(out, "envoy_grid_watts exists") {
		t.Errorf("existing metric not called out:\n%s", out)
	}
	if out := call(t, ts, "metrics_query", map[string]any{"query": "envoy_grid_watts"}); out != "envoy_grid_watts = -850" {
		t.Errorf("value: %q", out)
	}
	out = call(t, ts, "metrics_names", map[string]any{"filter": "house power"})
	if !strings.Contains(out, "closest matches") || !strings.Contains(out, "envoy_consumption_watts") {
		t.Errorf("metrics_names fallback:\n%s", out)
	}
}

func TestDescriptionsPointAtTheDashboard(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "d.json"), []byte(`{"title":"D","panels":[{"type":"row","title":"Energy"},
		{"type":"stat","title":"Grid now","targets":[{"expr":"envoy_grid_watts"}]}]}`), 0o644)
	ts := metricsTools(fakeProm(t), dir)
	for _, tl := range ts {
		if tl.Name != "metrics_names" && !strings.HasPrefix(tl.Description, "Start with lab_dashboard (run=true, filter=<topic>)") {
			t.Errorf("%s description: %s", tl.Name, tl.Description)
		}
	}
	if out := call(t, ts, "lab_dashboard", map[string]any{"filter": "grid", "run": true}); !strings.Contains(out, "= -850") {
		t.Errorf("dashboard run:\n%s", out)
	}
	if out := call(t, ts, "lab_dashboard", map[string]any{"filter": "nope"}); !strings.Contains(out, "Sections: Energy") {
		t.Errorf("no-match hint:\n%s", out)
	}
}
