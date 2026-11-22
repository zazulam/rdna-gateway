package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// workerSpec is one -worker flag: a llama-server run by a systemd user unit.
type workerSpec struct{ Name, Unit, URL string }

type workerFlags []workerSpec

func (w *workerFlags) String() string { return fmt.Sprint(*w) }

// Set parses "name=unit,url", e.g. "server-6700xt=llama-worker.service,http://127.0.0.1:8081".
func (w *workerFlags) Set(v string) error {
	name, rest, ok := strings.Cut(v, "=")
	unit, url, ok2 := strings.Cut(rest, ",")
	if !ok || !ok2 || name == "" || unit == "" || url == "" {
		return fmt.Errorf("want name=unit,url, got %q", v)
	}
	*w = append(*w, workerSpec{name, unit, strings.TrimRight(url, "/")})
	return nil
}

// unitShow returns a unit's properties (systemctl --user show). A variable so
// tests can fake systemd.
var unitShow = func(ctx context.Context, unit string) (map[string]string, error) {
	props, err := systemctlShow(ctx, unit, !noUnixTimestamps.Load())
	if err != nil && !noUnixTimestamps.Load() {
		// systemd before 251 rejects --timestamp=unix; ask for the default
		// format instead, and don't try the flag again.
		if p, err2 := systemctlShow(ctx, unit, false); err2 == nil {
			noUnixTimestamps.Store(true)
			props, err = p, nil
		}
	}
	if err != nil {
		return nil, err
	}
	if t, ok := props["ExecMainStartTimestamp"]; ok && !strings.HasPrefix(t, "@") {
		props["ExecMainStartTimestamp"] = unixTimestamp(t, time.Local)
	}
	return props, nil
}

// noUnixTimestamps is set once systemctl has refused --timestamp=unix.
var noUnixTimestamps atomic.Bool

func systemctlShow(ctx context.Context, unit string, unix bool) (map[string]string, error) {
	args := []string{"--user", "show"}
	if unix {
		args = append(args, "--timestamp=unix")
	}
	args = append(args, "-p", "NRestarts", "-p", "ExecMainStartTimestamp", "-p", "ExecMainPID", "-p", "Result", unit)
	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	return props, nil
}

// unixTimestamp converts systemd's default timestamp format ("Wed 2026-11-04
// 20:15:11 EST", in the local zone) to the "@seconds" form --timestamp=unix
// prints. Anything else (empty, "n/a") is returned unchanged.
func unixTimestamp(t string, loc *time.Location) string {
	tm, err := time.ParseInLocation("Mon 2006-01-02 15:04:05 MST", t, loc)
	if err != nil {
		return t
	}
	return "@" + strconv.FormatInt(tm.Unix(), 10)
}

// workerState is what the textfile reports for one worker.
type workerState struct {
	restarts    float64 // systemd NRestarts: automatic restarts of the unit
	startedUnix float64 // when the running process started
	pid         string
	loadSeconds float64 // NaN until a load has been timed
	model       string
	ctx         int
	loading     bool // started, not yet healthy
}

// workerWatcher follows each unit's restarts and times model loads: from the
// process start (systemd's ExecMainStartTimestamp) to the first healthy
// /health, polling every second while a model loads.
type workerWatcher struct {
	specs    []workerSpec
	interval time.Duration
	client   *http.Client
	now      func() time.Time
	changed  chan struct{} // nudges the writer when a load finishes

	mu    sync.Mutex
	state map[string]*workerState
}

func newWorkerWatcher(specs []workerSpec, interval time.Duration) *workerWatcher {
	w := &workerWatcher{specs: specs, interval: interval, client: &http.Client{Timeout: 2 * time.Second},
		now: time.Now, changed: make(chan struct{}, 1), state: map[string]*workerState{}}
	for _, s := range specs {
		w.state[s.Name] = &workerState{loadSeconds: math.NaN()}
	}
	return w
}

func (w *workerWatcher) run(ctx context.Context) {
	for _, s := range w.specs {
		go func(s workerSpec) {
			for {
				loading := w.poll(ctx, s)
				wait := w.interval
				if loading {
					wait = time.Second
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
		}(s)
	}
}

// poll checks one worker once and reports whether it is still loading.
func (w *workerWatcher) poll(ctx context.Context, s workerSpec) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	props, err := unitShow(cctx, s.Unit)
	cancel()
	if err != nil {
		log.Printf("hwmetrics: %s: systemctl: %v", s.Name, err)
		return false
	}
	w.mu.Lock()
	st := w.state[s.Name]
	first := st.pid == ""
	pid := props["ExecMainPID"]
	if n, err := strconv.ParseFloat(props["NRestarts"], 64); err == nil {
		st.restarts = n
	}
	if t, err := strconv.ParseFloat(strings.TrimPrefix(props["ExecMainStartTimestamp"], "@"), 64); err == nil {
		st.startedUnix = t
	}
	if pid != st.pid {
		if !first && pid != "0" {
			// Only names and counts go to the log; the crash output stays in the journal.
			log.Printf("hwmetrics: %s restarted (unit %s, restarts %v, last result %s)", s.Name, s.Unit, st.restarts, props["Result"])
		}
		st.pid = pid
		st.loading = pid != "0"
		st.model, st.ctx = "", 0
	}
	loading, needInfo, started := st.loading, st.model == "", st.startedUnix
	w.mu.Unlock()

	if pid == "0" || (!loading && !needInfo) {
		return false
	}
	if !w.healthy(ctx, s.URL) {
		return loading
	}
	model, nctx := w.props(ctx, s.URL)
	w.mu.Lock()
	defer w.mu.Unlock()
	// A worker already healthy when first seen loaded before we were watching,
	// so its load time is unknown.
	if loading && !first && started > 0 {
		st.loadSeconds = float64(w.now().UnixMilli())/1000 - started
		log.Printf("hwmetrics: %s loaded %s in %.1fs", s.Name, model, st.loadSeconds)
	}
	st.loading = false
	st.model, st.ctx = model, nctx
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return false
}

func (w *workerWatcher) healthy(ctx context.Context, url string) bool {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	resp, err := w.client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// props reads the loaded model's alias and per-slot context from /props.
func (w *workerWatcher) props(ctx context.Context, url string) (string, int) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/props", nil)
	resp, err := w.client.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	var p struct {
		ModelAlias string `json:"model_alias"`
		ModelPath  string `json:"model_path"`
		Settings   struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&p) != nil {
		return "", 0
	}
	model := p.ModelAlias
	if model == "" { // the file name only, never the directory
		model = p.ModelPath[strings.LastIndexAny(p.ModelPath, `/\`)+1:]
	}
	return model, p.Settings.NCtx
}

// render writes the worker lifecycle metrics, the same names supervise.ps1
// writes on Windows.
func (w *workerWatcher) render(out io.Writer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(w.state))
	for n := range w.state {
		names = append(names, n)
	}
	sort.Strings(names)
	q := promLabel
	fmt.Fprintln(out, "# HELP worker_restarts_total Worker restarts by its supervisor (systemd NRestarts on Linux), by reason.")
	fmt.Fprintln(out, "# TYPE worker_restarts_total counter")
	for _, n := range names {
		fmt.Fprintf(out, "worker_restarts_total{worker=%s,reason=\"exit\"} %g\n", q(n), w.state[n].restarts)
	}
	fmt.Fprintln(out, "# HELP worker_last_restart_timestamp_seconds When the worker's running process was (re)started.")
	fmt.Fprintln(out, "# TYPE worker_last_restart_timestamp_seconds gauge")
	for _, n := range names {
		if st := w.state[n]; st.startedUnix > 0 && st.pid != "0" {
			fmt.Fprintf(out, "worker_last_restart_timestamp_seconds{worker=%s} %d\n", q(n), int64(st.startedUnix))
		}
	}
	fmt.Fprintln(out, "# HELP worker_model_load_seconds Time from process start to the first healthy /health, for the last load.")
	fmt.Fprintln(out, "# TYPE worker_model_load_seconds gauge")
	for _, n := range names {
		if v := w.state[n].loadSeconds; !math.IsNaN(v) {
			fmt.Fprintf(out, "worker_model_load_seconds{worker=%s} %.3f\n", q(n), v)
		}
	}
	fmt.Fprintln(out, "# HELP worker_model_info The loaded model's name and per-slot context size.")
	fmt.Fprintln(out, "# TYPE worker_model_info gauge")
	for _, n := range names {
		if st := w.state[n]; st.model != "" {
			fmt.Fprintf(out, "worker_model_info{worker=%s,model=%s,ctx=\"%d\"} 1\n", q(n), q(st.model), st.ctx)
		}
	}
}

// promLabel quotes a label value the way the Prometheus text format wants.
func promLabel(v string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v) + `"`
}
