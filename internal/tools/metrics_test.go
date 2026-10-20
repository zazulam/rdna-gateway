package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeProm(t *testing.T) *Prom {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch r.URL.Path {
		case "/api/v1/query":
			if strings.Contains(q, "$__") {
				t.Errorf("grafana variable reached prometheus: %s", q)
			}
			if q == "bad(" {
				w.WriteHeader(400)
				w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
				return
			}
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"__name__":"lab_w","box":"sff"},"value":[1700000000,"95.5"]},
				{"metric":{"box":"desktop"},"value":[1700000000,"210"]}]}}`))
		case "/api/v1/query_range":
			w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"box":"sff"},"values":[[1700000000,"1"],[1700000060,"3"],[1700000120,"2"]]}]}}`))
		case "/api/v1/label/__name__/values":
			w.Write([]byte(`{"status":"success","data":["envoy_grid_watts","envoy_up","node_cpu_seconds_total"]}`))
		case "/api/v1/metadata":
			w.Write([]byte(`{"status":"success","data":{"envoy_grid_watts":[{"type":"gauge","help":"Grid power now"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return NewProm(srv.URL)
}

func TestInstantRangeAndNames(t *testing.T) {
	p, ctx := fakeProm(t), context.Background()
	ss, err := p.Instant(ctx, "sum(rate(x[$__rate_interval]))")
	if err != nil || len(ss) != 2 || ss[0].Samples[0].V != 95.5 {
		t.Fatalf("instant: %v %+v", err, ss)
	}
	if got := LabelString(ss[0].Labels); got != `lab_w{box="sff"}` {
		t.Fatalf("labels: %s", got)
	}
	if _, err := p.Instant(ctx, "bad("); err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("want prometheus error, got %v", err)
	}
	rs, step, err := p.Range(ctx, "x", 6*time.Hour, 240)
	if err != nil || len(rs[0].Samples) != 3 || step != 90*time.Second {
		t.Fatalf("range: %v step=%s %+v", err, step, rs)
	}
	ms, err := p.Names(ctx, "GRID")
	if err != nil || len(ms) != 1 || ms[0].Help != "Grid power now" {
		t.Fatalf("names: %v %+v", err, ms)
	}
}

func TestDashboardsReadTheRealDashboard(t *testing.T) {
	ps, err := Dashboards("../../deploy/sff/grafana/dashboards")
	if err != nil {
		t.Fatal(err)
	}
	var cost *Panel
	for i := range ps {
		if ps[i].Title == "Cost, last 24 h" {
			cost = &ps[i]
		}
	}
	if cost == nil || cost.Section != "Energy and cost" || cost.Unit != "currencyUSD" || !strings.Contains(cost.Queries[0].Expr, "lab:cost_usd_per_hour") {
		t.Fatalf("cost panel: %+v", cost)
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "6h": 6 * time.Hour, "7d": 168 * time.Hour, "2w": 336 * time.Hour} {
		if got, err := ParseWindow(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "6", "1y", "-1h", "1.5h"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
