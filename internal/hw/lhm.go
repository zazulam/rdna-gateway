package hw

import (
	"math"
	"strconv"
	"strings"
)

// LibreHardwareMonitor sensor names mapped to the shared readings. The first
// name present wins, so a board-power sensor beats a core-only one.
var (
	lhmGPUPower   = []string{"GPU Package", "GPU Power", "GPU Board Power"}
	lhmCPUPower   = []string{"Package", "CPU Package"}
	lhmGPUMemUsed = []string{"GPU Memory Used", "D3D Dedicated Memory Used"}
	lhmGPUMemSize = []string{"GPU Memory Total", "D3D Dedicated Memory Total"}
	lhmGPUTemps   = map[string]string{"GPU Core": "edge", "GPU Hot Spot": "junction", "GPU Memory": "mem", "GPU Memory Junction": "mem"}
	lhmCPUTemps   = map[string]string{"Core (Tctl/Tdie)": "tctl", "CPU Package": "package"}
)

type lhmSample struct {
	metric, hardwareID, sensor string
	v                          float64
}

// FromLHM derives hw devices from LibreHardwareMonitor's Prometheus output
// (lhm_<hardware>_<type>_<unit>{sensorName=..., hardwareId=...}). GPUs become
// gpu0, gpu1... and CPUs cpu0... in the order they first appear. LHM has no
// whole-system power sensor, so no kind="host" device comes from it.
func FromLHM(in []byte) []*Device {
	var samples []lhmSample
	for _, line := range strings.Split(string(in), "\n") {
		if s, ok := parseLHMLine(strings.TrimSpace(line)); ok {
			samples = append(samples, s)
		}
	}
	var devs []*Device
	byHW := map[string]*Device{}
	count := map[string]int{}
	device := func(s lhmSample) *Device {
		var kind string
		switch {
		case strings.HasPrefix(s.metric, "lhm_gpu"):
			kind = "gpu"
		case strings.HasPrefix(s.metric, "lhm_cpu_"):
			kind = "cpu"
		default:
			return nil
		}
		if d := byHW[s.hardwareID]; d != nil {
			return d
		}
		d := NewDevice(kind+strconv.Itoa(count[kind]), kind)
		count[kind]++
		byHW[s.hardwareID] = d
		devs = append(devs, d)
		return d
	}
	// pick returns the value of the first preferred sensor name present.
	pick := func(d *Device, suffix string, names []string) (float64, bool) {
		for _, n := range names {
			for _, s := range samples {
				if byHW[s.hardwareID] == d && s.sensor == n && strings.HasSuffix(s.metric, suffix) {
					return s.v, true
				}
			}
		}
		return 0, false
	}
	for _, s := range samples {
		device(s)
	}
	for _, d := range devs {
		if d.Kind == "gpu" {
			if v, ok := pick(d, "_power_watts", lhmGPUPower); ok {
				d.PowerW = v
			}
			if v, ok := pick(d, "_load_percent", []string{"GPU Core"}); ok {
				d.Utilization = v / 100
			}
			if v, ok := pick(d, "_bytes", lhmGPUMemUsed); ok {
				d.MemUsedBytes = v
			}
			if v, ok := pick(d, "_bytes", lhmGPUMemSize); ok {
				d.MemLimitBytes = v
			} else if free, ok := pick(d, "_bytes", []string{"GPU Memory Free"}); ok && !math.IsNaN(d.MemUsedBytes) {
				d.MemLimitBytes = d.MemUsedBytes + free
			}
		} else if v, ok := pick(d, "_power_watts", lhmCPUPower); ok {
			d.PowerW = v
		}
		names := lhmCPUTemps
		if d.Kind == "gpu" {
			names = lhmGPUTemps
		}
		seen := map[string]bool{}
		for _, s := range samples {
			if byHW[s.hardwareID] != d || !strings.HasSuffix(s.metric, "_temperature_celsius") {
				continue
			}
			if name, ok := names[s.sensor]; ok && !seen[name] {
				seen[name] = true
				d.Temps = append(d.Temps, Temp{name, s.v})
			}
		}
	}
	out := devs[:0]
	for _, d := range devs {
		if !d.empty() {
			out = append(out, d)
		}
	}
	return out
}

// parseLHMLine reads one sample line, keeping only the labels FromLHM needs.
// LHM quotes label names ({"sensorName"="GPU Core", ...}); both forms parse.
func parseLHMLine(line string) (lhmSample, bool) {
	if line == "" || line[0] == '#' || !strings.HasPrefix(line, "lhm_") {
		return lhmSample{}, false
	}
	open := strings.IndexByte(line, '{')
	end := strings.LastIndexByte(line, '}')
	if open < 0 || end < open {
		return lhmSample{}, false
	}
	s := lhmSample{metric: strings.TrimSpace(line[:open])}
	v, err := strconv.ParseFloat(strings.TrimSpace(line[end+1:]), 64)
	if err != nil {
		return lhmSample{}, false
	}
	s.v = v
	labels := line[open+1 : end]
	for len(labels) > 0 {
		labels = strings.TrimLeft(labels, " ,")
		eq := strings.IndexByte(labels, '=')
		if eq < 0 {
			break
		}
		key := strings.Trim(strings.TrimSpace(labels[:eq]), `"`)
		rest := strings.TrimLeft(labels[eq+1:], " ")
		if !strings.HasPrefix(rest, `"`) {
			break
		}
		// Find the closing quote, skipping escaped characters.
		i := 1
		var val strings.Builder
		for ; i < len(rest) && rest[i] != '"'; i++ {
			if rest[i] == '\\' && i+1 < len(rest) {
				i++
			}
			val.WriteByte(rest[i])
		}
		switch key {
		case "hardwareId":
			s.hardwareID = val.String()
		case "sensorName":
			s.sensor = val.String()
		}
		if i >= len(rest) {
			break
		}
		labels = rest[i+1:]
	}
	if s.hardwareID == "" {
		return lhmSample{}, false
	}
	return s, true
}
