package hw

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Shaped like LibreHardwareMonitor's /metrics on a 5950X + 6900 XT box.
const lhm = `# HELP lhm_cpu_power_watts x
# TYPE lhm_cpu_power_watts gauge
lhm_cpu_power_watts{"sensorName"="Package", "hardwareName"="AMD Ryzen 9 5950X", "sensorId"="/power/0", "hardwareId"="/amdcpu/0", "host"="desktop"} 65.6438369750977
lhm_cpu_power_watts{"sensorName"="Core 1 (SMU)", "hardwareId"="/amdcpu/0", "host"="desktop"} 2.28
lhm_cpu_temperature_celsius{"sensorName"="Core (Tctl/Tdie)", "hardwareId"="/amdcpu/0", "host"="desktop"} 48.5
lhm_memory_load_percent{"sensorName"="Memory", "hardwareId"="/ram", "host"="desktop"} 33.4
# TYPE lhm_gpuamd_power_watts gauge
lhm_gpuamd_power_watts{"sensorName"="GPU Core", "hardwareId"="/gpu-amd/0", "host"="desktop"} 9
lhm_gpuamd_power_watts{"sensorName"="GPU Package", "hardwareName"="AMD Radeon RX 6900 XT", "sensorId"="/power/3", "hardwareId"="/gpu-amd/0", "host"="desktop"} 17
lhm_gpuamd_temperature_celsius{"sensorName"="GPU Core", "hardwareId"="/gpu-amd/0", "host"="desktop"} 56.25
lhm_gpuamd_temperature_celsius{"sensorName"="GPU Hot Spot", "hardwareId"="/gpu-amd/0", "host"="desktop"} 60
lhm_gpuamd_load_percent{"sensorName"="GPU Core", "hardwareId"="/gpu-amd/0", "host"="desktop"} 37
lhm_gpuamd_load_percent{"sensorName"="D3D 3D", "hardwareId"="/gpu-amd/0", "host"="desktop"} 0
lhm_gpuamd_smalldata_bytes{"sensorName"="GPU Memory Used", "hardwareId"="/gpu-amd/0", "host"="desktop"} 10530848768
lhm_gpuamd_smalldata_bytes{"sensorName"="GPU Memory Free", "hardwareId"="/gpu-amd/0", "host"="desktop"} 6632243200
lhm_gpuamd_smalldata_bytes{"sensorName"="D3D Dedicated Memory Total", "hardwareId"="/gpu-amd/0", "host"="desktop"} 17131864064
lhm_gpuamd_fan_rpm{"sensorName"="GPU Fan", "hardwareId"="/gpu-amd/0", "host"="desktop"} 0
`

func render(devs []*Device) string {
	var b strings.Builder
	Render(&b, devs)
	return b.String()
}

func wantLines(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
}

func TestFromLHM(t *testing.T) {
	got := render(FromLHM([]byte(lhm)))
	wantLines(t, got,
		`hw_power_watts{device="cpu0",kind="cpu"} 65.6438369750977`,
		`hw_power_watts{device="gpu0",kind="gpu"} 17`, // package, not core
		`hw_gpu_utilization_ratio{device="gpu0"} 0.37`,
		`hw_gpu_memory_used_bytes{device="gpu0"} 10530848768`,
		`hw_gpu_memory_limit_bytes{device="gpu0"} 17131864064`,
		`hw_temperature_celsius{device="gpu0",kind="gpu",sensor="edge"} 56.25`,
		`hw_temperature_celsius{device="gpu0",kind="gpu",sensor="junction"} 60`,
		`hw_temperature_celsius{device="cpu0",kind="cpu",sensor="tctl"} 48.5`,
	)
	if strings.Count(got, "# TYPE hw_power_watts gauge") != 1 || strings.Contains(got, "host=") {
		t.Fatalf("want one TYPE line per family and no host label:\n%s", got)
	}
}

func TestFromLHMMemoryLimitFromFree(t *testing.T) {
	in := `lhm_gpuamd_smalldata_bytes{"sensorName"="GPU Memory Used", "hardwareId"="/gpu-amd/0"} 1000
lhm_gpuamd_smalldata_bytes{"sensorName"="GPU Memory Free", "hardwareId"="/gpu-amd/0"} 3000
lhm_gpunvidia_power_watts{"sensorName"="GPU Package", "hardwareId"="/gpu-nvidia/0"} 120
`
	got := render(FromLHM([]byte(in)))
	wantLines(t, got, `hw_gpu_memory_limit_bytes{device="gpu0"} 4000`, `hw_power_watts{device="gpu1",kind="gpu"} 120`)
}

func TestFromLHMIgnoresOtherExporters(t *testing.T) {
	if got := render(FromLHM([]byte("# TYPE up gauge\nup 1\nnode_hwmon_temp_celsius{chip=\"x\"} 40\n"))); got != "" {
		t.Fatalf("%s", got)
	}
}

func put(t *testing.T, root, path, v string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSysfsAMDGPU(t *testing.T) {
	root := t.TempDir()
	// card0: an APU's iGPU (no junction sensor), card1: the discrete card.
	put(t, root, "sys/class/drm/card0/device/hwmon/hwmon3/name", "amdgpu")
	put(t, root, "sys/class/drm/card0/device/hwmon/hwmon3/temp1_label", "edge")
	put(t, root, "sys/class/drm/card0/device/hwmon/hwmon3/temp1_input", "41000")
	put(t, root, "sys/class/drm/card0/device/hwmon/hwmon3/power1_input", "30000000")
	put(t, root, "sys/class/drm/card0-DP-1/status", "connected")
	g := "sys/class/drm/card1/device/"
	put(t, root, g+"hwmon/hwmon5/name", "amdgpu")
	put(t, root, g+"hwmon/hwmon5/power1_average", "186000000")
	put(t, root, g+"hwmon/hwmon5/temp1_label", "edge")
	put(t, root, g+"hwmon/hwmon5/temp1_input", "62000")
	put(t, root, g+"hwmon/hwmon5/temp2_label", "junction")
	put(t, root, g+"hwmon/hwmon5/temp2_input", "78000")
	put(t, root, g+"hwmon/hwmon5/temp3_label", "mem")
	put(t, root, g+"hwmon/hwmon5/temp3_input", "70000")
	put(t, root, g+"gpu_busy_percent", "99")
	put(t, root, g+"mem_info_vram_used", "11000000000")
	put(t, root, g+"mem_info_vram_total", "12868124672")
	put(t, root, "sys/class/hwmon/hwmon1/name", "k10temp")
	put(t, root, "sys/class/hwmon/hwmon1/temp1_label", "Tctl")
	put(t, root, "sys/class/hwmon/hwmon1/temp1_input", "55250")

	got := render((&Sysfs{Root: root}).Read())
	wantLines(t, got,
		`hw_power_watts{device="gpu0",kind="gpu"} 186`,
		`hw_gpu_utilization_ratio{device="gpu0"} 0.99`,
		`hw_gpu_memory_used_bytes{device="gpu0"} 11000000000`,
		`hw_gpu_memory_limit_bytes{device="gpu0"} 12868124672`,
		`hw_temperature_celsius{device="gpu0",kind="gpu",sensor="junction"} 78`,
		`hw_temperature_celsius{device="gpu0",kind="gpu",sensor="mem"} 70`,
		`hw_temperature_celsius{device="cpu0",kind="cpu",sensor="tctl"} 55.25`,
	)
	if strings.Contains(got, " 30\n") || strings.Contains(got, "gpu1") {
		t.Fatalf("the iGPU's package power leaked in:\n%s", got)
	}
}

func TestSysfsPowerInputFallback(t *testing.T) {
	root := t.TempDir()
	g := "sys/class/drm/card0/device/"
	put(t, root, g+"hwmon/hwmon2/name", "amdgpu")
	put(t, root, g+"hwmon/hwmon2/power1_input", "15500000")
	put(t, root, g+"hwmon/hwmon2/temp2_label", "junction")
	put(t, root, g+"hwmon/hwmon2/temp2_input", "40000")
	wantLines(t, render((&Sysfs{Root: root}).Read()), `hw_power_watts{device="gpu0",kind="gpu"} 15.5`)
}

func TestSysfsJetson(t *testing.T) {
	root := t.TempDir()
	m := "sys/class/hwmon/hwmon1/"
	put(t, root, m+"name", "ina3221")
	for ch, rail := range map[string][3]string{"1": {"VDD_IN", "5072", "1416"}, "2": {"VDD_CPU_GPU_CV", "5072", "400"}, "3": {"VDD_SOC", "5072", "600"}} {
		put(t, root, m+"in"+ch+"_label", rail[0])
		put(t, root, m+"in"+ch+"_input", rail[1])
		put(t, root, m+"curr"+ch+"_input", rail[2])
	}
	put(t, root, "sys/devices/platform/bus@0/17000000.gpu/load", "250")
	put(t, root, "sys/class/thermal/thermal_zone1/type", "gpu-thermal")
	put(t, root, "sys/class/thermal/thermal_zone1/temp", "47500")
	put(t, root, "sys/class/thermal/thermal_zone0/type", "cpu-thermal")
	put(t, root, "sys/class/thermal/thermal_zone0/temp", "49000")

	got := render((&Sysfs{Root: root}).Read())
	wantLines(t, got,
		`hw_power_watts{device="board",kind="host"} 7.181952`,
		`hw_power_watts{device="vdd_cpu_gpu_cv",kind="rail"} 2.0288`,
		`hw_gpu_utilization_ratio{device="gpu0"} 0.25`,
		`hw_temperature_celsius{device="gpu0",kind="gpu",sensor="gpu"} 47.5`,
		`hw_temperature_celsius{device="cpu0",kind="cpu",sensor="cpu"} 49`,
	)
	if strings.Contains(got, `kind="gpu"} `) && strings.Contains(got, `hw_power_watts{device="gpu0"`) {
		t.Fatalf("a Jetson has no GPU power rail:\n%s", got)
	}
}

// A sysfs file that never answers (the Jetson's powered-off cv zones) must not
// stall the collector, and is skipped from then on.
func TestSysfsReadTimeout(t *testing.T) {
	root := t.TempDir()
	z := filepath.Join(root, "sys/class/thermal/thermal_zone0")
	os.MkdirAll(z, 0o755)
	if err := syscall.Mkfifo(filepath.Join(z, "type"), 0o644); err != nil {
		t.Skip("no fifo:", err)
	}
	s := &Sysfs{Root: root, Timeout: 50 * time.Millisecond}
	start := time.Now()
	s.Read()
	s.Read()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("reads took %v", took)
	}
	if !s.hung[filepath.Join(z, "type")] {
		t.Fatal("hung path not remembered")
	}
}

func TestRenderSkipsAbsent(t *testing.T) {
	d := NewDevice("gpu0", "gpu")
	d.PowerW = 20
	got := render([]*Device{d})
	if strings.Contains(got, "utilization") || strings.Contains(got, "memory") || strings.Contains(got, "NaN") {
		t.Fatalf("%s", got)
	}
}
