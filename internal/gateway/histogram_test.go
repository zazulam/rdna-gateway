package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHistogramExposition(t *testing.T) {
	h := newHistogram("x_seconds", "Test.", []float64{0.1, 1})
	k := histKey{"w", "m", "prefix", "ok"}
	for _, v := range []float64{0.05, 0.1, 0.5, 3, -1} { // a negative value is ignored
		h.observe(k, v)
	}
	var b strings.Builder
	h.write(&b)
	want := `# HELP x_seconds Test.
# TYPE x_seconds histogram
x_seconds_bucket{worker="w",model="m",route="prefix",outcome="ok",le="0.1"} 2
x_seconds_bucket{worker="w",model="m",route="prefix",outcome="ok",le="1"} 3
x_seconds_bucket{worker="w",model="m",route="prefix",outcome="ok",le="+Inf"} 4
x_seconds_sum{worker="w",model="m",route="prefix",outcome="ok"} 3.65
x_seconds_count{worker="w",model="m",route="prefix",outcome="ok"} 4
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}

func TestBucketsCoverTheRanges(t *testing.T) {
	if ttftBuckets[0] > 0.05 || ttftBuckets[len(ttftBuckets)-1] < 60 {
		t.Error("time to first token buckets should span 0.05-60 s")
	}
	if perTokenBuckets[0] > 0.001 || perTokenBuckets[len(perTokenBuckets)-1] < 1 {
		t.Error("per-token buckets should span 1 ms-1 s")
	}
	for _, bs := range [][]float64{ttftBuckets, perTokenBuckets, durationBuckets, queueBuckets} {
		for i := 1; i < len(bs); i++ {
			if bs[i] <= bs[i-1] {
				t.Fatalf("buckets not increasing: %v", bs)
			}
		}
	}
}

func scrape(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestLatencyHistogramsFromAStream(t *testing.T) {
	a := newLlamaStub(t, "a")
	a.firstTokenDelay = 150 * time.Millisecond
	_, srv, _ := tracedGateway(t, nil, nil, map[string]string{"a": a.srv.URL})
	post(t, srv.URL, "/v1/chat/completions", chatBody(true), nil)
	post(t, srv.URL, "/v1/chat/completions", chatBody(false), nil)

	m := scrape(t, srv.URL)
	const l = `worker="a",model="qwen",route="least-loaded",outcome="ok"`
	for _, want := range []string{
		"# TYPE gen_ai_server_time_to_first_token_seconds histogram",
		// The first token came after ~150 ms; only the streamed request has one.
		`gen_ai_server_time_to_first_token_seconds_bucket{` + l + `,le="0.1"} 0`,
		`gen_ai_server_time_to_first_token_seconds_count{` + l + `} 1`,
		// 50 ms over 5 tokens = 10 ms per token, from the worker's timings.
		`gen_ai_server_time_per_output_token_seconds_bucket{` + l + `,le="0.005"} 0`,
		`gen_ai_server_time_per_output_token_seconds_bucket{` + l + `,le="0.01"} 2`,
		`gen_ai_server_request_duration_seconds_count{` + l + `} 2`,
		`rdnagw_queue_wait_seconds_count{` + l + `} 2`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %s\n%s", want, m)
		}
	}
}

func TestLatencyOutcomesAndModelLabel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	a := newLlamaStub(t, "a")
	inner := a.srv.Config.Handler
	a.srv.Config.Handler = http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			inner.ServeHTTP(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		rw.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	g, srv, _ := tracedGateway(t, nil, func(c *Config) { c.StallTimeout = Duration(100 * time.Millisecond) },
		map[string]string{"a": a.srv.URL})
	post(t, srv.URL, "/v1/chat/completions", chatBody(true), nil)
	waitFor(t, func() bool {
		return strings.Contains(scrape(t, srv.URL), `gen_ai_server_request_duration_seconds_count{worker="a",model="qwen",route="least-loaded",outcome="stalled"} 1`)
	})

	c := &call{worker: g.router.workers[0], model: "some client string"}
	if c.metricModel() != "other" {
		t.Error("a model the worker doesn't list should be labelled other")
	}
}
