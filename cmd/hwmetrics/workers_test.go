package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerFlag(t *testing.T) {
	var w workerFlags
	if err := w.Set("server-6700xt=llama-worker.service,http://127.0.0.1:8081/"); err != nil {
		t.Fatal(err)
	}
	if w[0] != (workerSpec{"server-6700xt", "llama-worker.service", "http://127.0.0.1:8081"}) {
		t.Fatalf("%+v", w[0])
	}
	if err := w.Set("bad"); err == nil {
		t.Fatal("want an error")
	}
}

// A worker that loads, serves, crashes and is restarted by systemd.
func TestWorkerWatcher(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !ready.Load() {
				http.Error(rw, `{"error":"Loading model"}`, http.StatusServiceUnavailable)
				return
			}
			io.WriteString(rw, `{"status":"ok"}`)
		case "/props":
			io.WriteString(rw, `{"model_alias":"gpt-oss:20b","model_path":"/home/lab/models/gpt-oss-20b.gguf",
				"default_generation_settings":{"n_ctx":32768},"chat_template":"{{ secret template }}"}`)
		}
	}))
	defer srv.Close()

	props := map[string]string{"NRestarts": "0", "ExecMainStartTimestamp": "@1000", "ExecMainPID": "100", "Result": "success"}
	unitShow = func(ctx context.Context, unit string) (map[string]string, error) {
		if unit != "llama-worker.service" {
			t.Errorf("unit %q", unit)
		}
		return props, nil
	}
	w := newWorkerWatcher([]workerSpec{{"server-6700xt", "llama-worker.service", srv.URL}}, time.Minute)
	now := time.Unix(1000, 0)
	w.now = func() time.Time { return now }
	ctx := context.Background()
	spec := w.specs[0]
	render := func() string {
		var b strings.Builder
		w.render(&b)
		return b.String()
	}

	if !w.poll(ctx, spec) {
		t.Fatal("want loading while /health is 503")
	}
	now = time.Unix(1042, 500_000_000)
	ready.Store(true)
	if w.poll(ctx, spec) {
		t.Fatal("still loading after a healthy /health")
	}
	got := render()
	for _, want := range []string{
		`worker_restarts_total{worker="server-6700xt",reason="exit"} 0`,
		`worker_last_restart_timestamp_seconds{worker="server-6700xt"} 1000`,
		`worker_model_load_seconds{worker="server-6700xt"} 42.500`,
		`worker_model_info{worker="server-6700xt",model="gpt-oss:20b",ctx="32768"} 1`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "template") || strings.Contains(got, "/home/") {
		t.Fatalf("props content leaked:\n%s", got)
	}

	// systemd restarts it after a crash.
	ready.Store(false)
	props = map[string]string{"NRestarts": "1", "ExecMainStartTimestamp": "@2000", "ExecMainPID": "200", "Result": "exit-code"}
	now = time.Unix(2000, 0)
	if !w.poll(ctx, spec) {
		t.Fatal("want loading after the restart")
	}
	ready.Store(true)
	now = time.Unix(2010, 0)
	w.poll(ctx, spec)
	got = render()
	for _, want := range []string{
		`worker_restarts_total{worker="server-6700xt",reason="exit"} 1`,
		`worker_last_restart_timestamp_seconds{worker="server-6700xt"} 2000`,
		`worker_model_load_seconds{worker="server-6700xt"} 10.000`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

// A worker already serving when hwmetrics starts has no known load time.
func TestWorkerWatcherAlreadyUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			io.WriteString(rw, `{"model_path":"C:\\models\\gemma.gguf","default_generation_settings":{"n_ctx":8192}}`)
		}
	}))
	defer srv.Close()
	unitShow = func(ctx context.Context, unit string) (map[string]string, error) {
		return map[string]string{"NRestarts": "3", "ExecMainStartTimestamp": "@1000", "ExecMainPID": "7"}, nil
	}
	w := newWorkerWatcher([]workerSpec{{"embed", "llama-embed.service", srv.URL}}, time.Minute)
	w.poll(context.Background(), w.specs[0])
	var b strings.Builder
	w.render(&b)
	if strings.Contains(b.String(), "worker_model_load_seconds{") ||
		!strings.Contains(b.String(), `worker_model_info{worker="embed",model="gemma.gguf",ctx="8192"} 1`) {
		t.Fatalf("%s", b.String())
	}
}

// systemd before 251 has no --timestamp=unix, so its default format is converted.
func TestUnixTimestamp(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	for in, want := range map[string]string{
		"Wed 2026-11-04 20:15:11 EST": "@1793841311",
		"Sat 2026-07-04 12:00:00 EDT": "@1783180800",
		"":                            "",
		"n/a":                         "n/a",
	} {
		if got := unixTimestamp(in, ny); got != want {
			t.Errorf("unixTimestamp(%q) = %q, want %q", in, got, want)
		}
	}
	utc := unixTimestamp("Wed 2026-11-04 20:15:11 UTC", time.UTC)
	if utc != "@1793823311" {
		t.Errorf("UTC: got %q", utc)
	}
}
