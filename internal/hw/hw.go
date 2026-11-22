// Package hw renders one host's hardware readings (GPU and host power, GPU
// utilization and memory, temperatures) as Prometheus gauges named after the
// OpenTelemetry hw.* semantic conventions, with dots as underscores:
//
//	hw_power_watts{device="gpu0",kind="gpu"}          hw.power
//	hw_gpu_utilization_ratio{device="gpu0"}           hw.gpu.utilization
//	hw_gpu_memory_used_bytes{device="gpu0"}           hw.gpu.memory.usage
//	hw_gpu_memory_limit_bytes{device="gpu0"}          hw.gpu.memory.limit
//	hw_temperature_celsius{device,kind,sensor}        hw.temperature
//
// Every host produces the same series whatever its sensors are: the Windows
// gateway translates LibreHardwareMonitor (FromLHM), Linux hosts and Jetsons
// read sysfs (ReadSysfs). The exposition carries no host label: Prometheus adds
// it from the scrape target, set to the same name as the host.name resource
// attribute in traces, so power lines up with requests by host.
package hw

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Device is one measured component. Fields left NaN were not reported.
type Device struct {
	ID   string // gpu0, cpu0, board, vdd_soc, ...
	Kind string // gpu, cpu, host (whole-system input), rail (one supply rail)

	PowerW, Utilization, MemUsedBytes, MemLimitBytes float64
	Temps                                            []Temp
}

// Temp is one temperature sensor on a device. Sensor names follow the amdgpu
// hwmon labels where there is one (edge, junction, mem), so Windows and Linux
// GPUs line up.
type Temp struct {
	Sensor  string
	Celsius float64
}

// NewDevice returns a device with every reading absent.
func NewDevice(id, kind string) *Device {
	n := math.NaN()
	return &Device{ID: id, Kind: kind, PowerW: n, Utilization: n, MemUsedBytes: n, MemLimitBytes: n}
}

func (d *Device) empty() bool {
	return math.IsNaN(d.PowerW) && math.IsNaN(d.Utilization) && math.IsNaN(d.MemUsedBytes) &&
		math.IsNaN(d.MemLimitBytes) && len(d.Temps) == 0
}

// Render writes the devices' readings as Prometheus text, one HELP and TYPE
// per metric. Nothing is written for absent readings.
func Render(w io.Writer, devs []*Device) {
	type sample struct {
		labels string
		v      float64
	}
	fams := []struct {
		name, help string
		get        func(d *Device) []sample
	}{
		{"hw_power_watts", "Instantaneous power drawn by the device (OpenTelemetry hw.power).", func(d *Device) []sample {
			return []sample{{fmt.Sprintf(`device=%q,kind=%q`, d.ID, d.Kind), d.PowerW}}
		}},
		{"hw_gpu_utilization_ratio", "Fraction of time the GPU was busy (hw.gpu.utilization).", func(d *Device) []sample {
			return []sample{{fmt.Sprintf(`device=%q`, d.ID), d.Utilization}}
		}},
		{"hw_gpu_memory_used_bytes", "GPU memory in use (hw.gpu.memory.usage).", func(d *Device) []sample {
			return []sample{{fmt.Sprintf(`device=%q`, d.ID), d.MemUsedBytes}}
		}},
		{"hw_gpu_memory_limit_bytes", "GPU memory size (hw.gpu.memory.limit).", func(d *Device) []sample {
			return []sample{{fmt.Sprintf(`device=%q`, d.ID), d.MemLimitBytes}}
		}},
		{"hw_temperature_celsius", "Temperature of a sensor on the device (hw.temperature).", func(d *Device) []sample {
			var s []sample
			for _, t := range d.Temps {
				s = append(s, sample{fmt.Sprintf(`device=%q,kind=%q,sensor=%q`, d.ID, d.Kind, t.Sensor), t.Celsius})
			}
			return s
		}},
	}
	for _, f := range fams {
		var lines []string
		for _, d := range devs {
			for _, s := range f.get(d) {
				if !math.IsNaN(s.v) && !math.IsInf(s.v, 0) {
					lines = append(lines, fmt.Sprintf("%s{%s} %s\n", f.name, s.labels, strconv.FormatFloat(s.v, 'f', -1, 64)))
				}
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", f.name, f.help, f.name)
		io.WriteString(w, strings.Join(lines, ""))
	}
}
