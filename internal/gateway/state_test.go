package gateway

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zazulam/rdna-gateway/internal/otlp/otlptest"
)

// A worker that starts loading a model (503 on /health), then comes back:
// both changes are counted, timestamped and sent as OTLP log records.
func TestWorkerStateChanges(t *testing.T) {
	rcv := otlptest.New(t)
	a := newLlamaStub(t, "a")
	var loading atomic.Bool
	inner := a.srv.Config.Handler
	a.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && loading.Load() {
			http.Error(rw, `{"error":"Loading model"}`, http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(rw, r)
	})
	g, srv, exp := tracedGateway(t, rcv, nil, map[string]string{"a": a.srv.URL})
	w := g.router.workers[0]

	g.checkAllSync(context.Background()) // healthy at start: no change
	if m := scrape(t, srv.URL); !strings.Contains(m, `rdnagw_worker_state_changes_total{worker="a",to="down"} 0`) ||
		!strings.Contains(m, `rdnagw_worker_last_change_timestamp_seconds{worker="a"} 0.000`) {
		t.Fatalf("before any change:\n%s", m)
	}

	before := time.Now()
	loading.Store(true)
	g.checkAllSync(context.Background())
	loading.Store(false)
	g.checkAllSync(context.Background())
	g.checkAllSync(context.Background()) // still up: no change

	if w.downChanges.Load() != 1 || w.upChanges.Load() != 1 {
		t.Fatalf("changes down=%d up=%d", w.downChanges.Load(), w.upChanges.Load())
	}
	if ts := time.Unix(0, w.lastChange.Load()); ts.Before(before) {
		t.Errorf("last change %v", ts)
	}
	m := scrape(t, srv.URL)
	for _, want := range []string{
		`rdnagw_worker_state_changes_total{worker="a",to="up"} 1`,
		`rdnagw_worker_state_changes_total{worker="a",to="down"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}

	exp.ForceFlush(context.Background())
	logs := rcv.Logs()
	if len(logs) != 2 {
		t.Fatalf("want 2 log records, got %+v", logs)
	}
	down, up := logs[0], logs[1]
	if down.EventName != "rdnagw.worker.state_change" || down.Attrs["worker"] != "a" || down.Attrs["to"] != "down" ||
		down.Attrs["reason"] != "unhealthy_status" || down.SeverityNumber != 13 {
		t.Errorf("down record %+v", down)
	}
	if up.Attrs["to"] != "up" || up.Attrs["reason"] != "health_ok" {
		t.Errorf("up record %+v", up)
	}
	if strings.Contains(rcv.Raw("/v1/logs"), a.srv.URL) {
		t.Error("log records should name the worker, not its address")
	}
}

// A refused connection on the request path marks the worker down at once,
// and that counts as a state change too.
func TestRequestFailureIsAStateChange(t *testing.T) {
	b := newLlamaStub(t, "b")
	dead := newLlamaStub(t, "dead")
	dead.srv.Close()
	_, srv, _ := tracedGateway(t, nil, func(c *Config) { c.Policy = PolicyRoundRobin },
		map[string]string{"dead": dead.srv.URL, "b": b.srv.URL})
	for i := 0; i < 2; i++ { // round-robin: one of the two starts on the dead worker
		post(t, srv.URL, "/v1/chat/completions", chatBody(false), nil)
	}
	if !strings.Contains(scrape(t, srv.URL), `rdnagw_worker_state_changes_total{worker="dead",to="down"} 1`) {
		t.Error("the dead worker's change should be counted")
	}
}

func TestHealthFailureReasons(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := healthFailure(ctx.Err()); got != "timeout" {
		t.Errorf("deadline: %s", got)
	}
	if got := healthFailure(errHealthStatus); got != "unhealthy_status" {
		t.Errorf("status: %s", got)
	}
}
