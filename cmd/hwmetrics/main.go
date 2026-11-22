// hwmetrics writes a Linux host's GPU and host power, GPU utilization and
// memory, and temperatures for node_exporter's textfile collector, in the same
// hw_* form the gateway serves for Windows hosts at /metrics/host (see
// internal/hw and docs/HARDWARE-METRICS.md).
//
//	hwmetrics                                  # ~/.node-textfile/hw.prom every 15s
//	hwmetrics -textfile /path/hw.prom -interval 30s
//	hwmetrics -once -textfile -                # print once to stdout
//
// With -worker it also writes workers.prom next to it: each llama-server
// unit's restarts (systemd's NRestarts), when its process started, how long
// its model took to load and which model it serves (docs/HARDWARE-METRICS.md):
//
//	hwmetrics -worker server-6700xt=llama-worker.service,http://127.0.0.1:8081
//
// It reads amdgpu hwmon on a GPU box and the INA3221 rails on a Jetson. Using
// the textfile collector rather than its own listener means no new port or
// scrape job: node_exporter already runs on both kinds of box.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zazulam/rdna-gateway/internal/hw"
)

func main() {
	home, _ := os.UserHomeDir()
	textfile := flag.String("textfile", filepath.Join(home, ".node-textfile", "hw.prom"), `output file ("-" for stdout)`)
	interval := flag.Duration("interval", 15*time.Second, "how often to read the sensors")
	once := flag.Bool("once", false, "write once and exit")
	root := flag.String("root", "/", "filesystem root to read /sys from (for tests)")
	var workers workerFlags
	flag.Var(&workers, "worker", "a llama-server systemd user unit to follow: name=unit,url (repeatable)")
	flag.Parse()

	s := &hw.Sysfs{Root: *root}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var ww *workerWatcher
	if len(workers) > 0 && !*once {
		ww = newWorkerWatcher(workers, *interval)
		ww.run(ctx)
	}
	workersFile := filepath.Join(filepath.Dir(*textfile), "workers.prom")
	for {
		var b bytes.Buffer
		hw.Render(&b, s.Read())
		if err := write(*textfile, b.Bytes()); err != nil {
			log.Printf("hwmetrics: %v", err)
		}
		if *once {
			return
		}
		if ww != nil && *textfile != "-" {
			var b bytes.Buffer
			ww.render(&b)
			if err := write(workersFile, b.Bytes()); err != nil {
				log.Printf("hwmetrics: %v", err)
			}
		}
		var nudge chan struct{}
		if ww != nil {
			nudge = ww.changed
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(*interval):
		case <-nudge: // a model finished loading: publish it now
		}
	}
}

// write replaces path atomically (temp file + rename in the same directory),
// so node_exporter never reads a half-written file.
func write(path string, data []byte) error {
	b := bytes.NewBuffer(data)
	if path == "-" {
		_, err := os.Stdout.Write(b.Bytes())
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hw-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
