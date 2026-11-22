package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zazulam/rdna-gateway/internal/hw"
)

func TestWriteReplacesTextfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hw.prom")
	d := hw.NewDevice("gpu0", "gpu")
	for _, w := range []float64{20, 180} {
		d.PowerW = w
		if err := write(path, []*hw.Device{d}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `hw_power_watts{device="gpu0",kind="gpu"} 180`) {
		t.Fatalf("%v %s", err, b)
	}
	// No temp files left for node_exporter to trip over.
	if left, _ := filepath.Glob(filepath.Join(dir, "sub", ".hw-*")); len(left) != 0 {
		t.Fatalf("leftover %v", left)
	}
}
