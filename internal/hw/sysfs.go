package hw

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sysfs reads Linux hardware sensors under Root ("/" on a real host; a fake
// tree in tests):
//
//   - discrete amdgpu cards: board power (hwmon power1_average, else
//     power1_input, in microwatts), busy percent, VRAM used and total, and the
//     edge/junction/mem temperatures. A card without a "junction" sensor is an
//     APU's iGPU, whose power reading is the whole package's; it is skipped.
//   - AMD/Intel CPU temperature (k10temp Tctl, coretemp package).
//   - Jetson: the INA3221 rails (VDD_IN is the whole board, kind="host"; the
//     other rails kind="rail"), GPU load and the gpu/cpu thermal zones. The
//     Jetson's GPU has no power rail of its own and shares system memory, so
//     it reports utilization and temperature only.
//
// Some Jetson thermal zones (the powered-off cv*) block forever on read, so
// every read has a timeout and a path that timed out once is skipped after.
type Sysfs struct {
	Root    string
	Timeout time.Duration

	mu   sync.Mutex
	hung map[string]bool
}

var cardRE = regexp.MustCompile(`^card[0-9]+$`)

// Read returns the host's devices in a stable order.
func (s *Sysfs) Read() []*Device {
	var devs []*Device
	devs = append(devs, s.amdgpu()...)
	devs = append(devs, s.jetson()...)
	devs = append(devs, s.cpu()...)
	out := devs[:0]
	for _, d := range devs {
		if !d.empty() {
			out = append(out, d)
		}
	}
	return out
}

func (s *Sysfs) path(p ...string) string {
	return filepath.Join(append([]string{s.Root}, p...)...)
}

// read returns a sysfs file's trimmed contents, or "" if it is missing,
// unreadable or doesn't answer within the timeout.
func (s *Sysfs) read(path string) string {
	s.mu.Lock()
	if s.hung == nil {
		s.hung = map[string]bool{}
	}
	if s.hung[path] {
		s.mu.Unlock()
		return ""
	}
	s.mu.Unlock()
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ch := make(chan string, 1)
	go func() {
		b, err := os.ReadFile(path)
		if err != nil {
			ch <- ""
			return
		}
		ch <- strings.TrimSpace(string(b))
	}()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		s.mu.Lock()
		s.hung[path] = true
		s.mu.Unlock()
		return ""
	}
}

func (s *Sysfs) num(path string) (float64, bool) {
	v := s.read(path)
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	return f, err == nil
}

// hwmons returns the hwmon directories under dir whose name file is one of names.
func (s *Sysfs) hwmons(dir string, names ...string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, "hwmon*"))
	sort.Strings(paths)
	var out []string
	for _, p := range paths {
		n := s.read(filepath.Join(p, "name"))
		for _, want := range names {
			if n == want {
				out = append(out, p)
			}
		}
	}
	return out
}

// labelledTemps reads tempN_label / tempN_input pairs (millidegrees).
func (s *Sysfs) labelledTemps(hwmon string) map[string]float64 {
	out := map[string]float64{}
	inputs, _ := filepath.Glob(filepath.Join(hwmon, "temp*_input"))
	for _, in := range inputs {
		label := s.read(strings.TrimSuffix(in, "_input") + "_label")
		if v, ok := s.num(in); ok && label != "" {
			out[label] = v / 1000
		}
	}
	return out
}

func (s *Sysfs) amdgpu() []*Device {
	entries, _ := os.ReadDir(s.path("sys/class/drm"))
	var cards []string
	for _, e := range entries {
		if cardRE.MatchString(e.Name()) {
			cards = append(cards, e.Name())
		}
	}
	// card10 after card9.
	sort.Slice(cards, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(cards[i], "card"))
		b, _ := strconv.Atoi(strings.TrimPrefix(cards[j], "card"))
		return a < b
	})
	var devs []*Device
	for _, c := range cards {
		dev := s.path("sys/class/drm", c, "device")
		mons := s.hwmons(filepath.Join(dev, "hwmon"), "amdgpu")
		if len(mons) == 0 {
			continue
		}
		temps := s.labelledTemps(mons[0])
		if _, discrete := temps["junction"]; !discrete {
			continue
		}
		d := NewDevice("gpu"+strconv.Itoa(len(devs)), "gpu")
		for _, f := range []string{"power1_average", "power1_input"} {
			if v, ok := s.num(filepath.Join(mons[0], f)); ok {
				d.PowerW = v / 1e6
				break
			}
		}
		if v, ok := s.num(filepath.Join(dev, "gpu_busy_percent")); ok {
			d.Utilization = v / 100
		}
		if v, ok := s.num(filepath.Join(dev, "mem_info_vram_used")); ok {
			d.MemUsedBytes = v
		}
		if v, ok := s.num(filepath.Join(dev, "mem_info_vram_total")); ok {
			d.MemLimitBytes = v
		}
		for _, l := range []string{"edge", "junction", "mem"} {
			if v, ok := temps[l]; ok {
				d.Temps = append(d.Temps, Temp{l, v})
			}
		}
		devs = append(devs, d)
	}
	return devs
}

func (s *Sysfs) cpu() []*Device {
	d := NewDevice("cpu0", "cpu")
	for _, m := range s.hwmons(s.path("sys/class/hwmon"), "k10temp", "coretemp") {
		t := s.labelledTemps(m)
		if v, ok := t["Tctl"]; ok {
			d.Temps = append(d.Temps, Temp{"tctl", v})
		}
		if v, ok := t["Package id 0"]; ok {
			d.Temps = append(d.Temps, Temp{"package", v})
		}
		break // first socket only
	}
	// Jetson: the CPU cluster's thermal zone.
	if v, ok := s.zone("cpu-thermal"); ok {
		d.Temps = append(d.Temps, Temp{"cpu", v})
	}
	return []*Device{d}
}

// zone returns the temperature of the thermal zone whose type is name,
// skipping the cv* zones that hang on read.
func (s *Sysfs) zone(name string) (float64, bool) {
	zones, _ := filepath.Glob(s.path("sys/class/thermal/thermal_zone*"))
	sort.Strings(zones)
	for _, z := range zones {
		t := s.read(filepath.Join(z, "type"))
		if t != name || strings.HasPrefix(t, "cv") {
			continue
		}
		if v, ok := s.num(filepath.Join(z, "temp")); ok {
			return v / 1000, true
		}
	}
	return 0, false
}

// jetsonGPULoad lists where L4T exposes GPU load in per-mille.
var jetsonGPULoad = []string{
	"sys/devices/platform/bus@0/17000000.gpu/load", // Orin, JetPack 6
	"sys/devices/platform/17000000.gpu/load",       // Orin, JetPack 5
	"sys/devices/gpu.0/load",                       // Xavier and older
}

func (s *Sysfs) jetson() []*Device {
	var devs []*Device
	for _, m := range s.hwmons(s.path("sys/class/hwmon"), "ina3221") {
		for ch := 1; ch <= 3; ch++ {
			label := s.read(filepath.Join(m, "in"+strconv.Itoa(ch)+"_label"))
			mv, ok1 := s.num(filepath.Join(m, "in"+strconv.Itoa(ch)+"_input"))
			ma, ok2 := s.num(filepath.Join(m, "curr"+strconv.Itoa(ch)+"_input"))
			if label == "" || !ok1 || !ok2 {
				continue
			}
			d := NewDevice(strings.ToLower(label), "rail")
			if strings.EqualFold(label, "VDD_IN") {
				d.ID, d.Kind = "board", "host"
			}
			d.PowerW = mv * ma / 1e6
			devs = append(devs, d)
		}
	}
	gpu := NewDevice("gpu0", "gpu")
	for _, p := range jetsonGPULoad {
		if v, ok := s.num(s.path(p)); ok {
			gpu.Utilization = v / 1000
			break
		}
	}
	if !gpu.empty() { // only a Jetson has the load file; the zone alone isn't enough
		if v, ok := s.zone("gpu-thermal"); ok {
			gpu.Temps = append(gpu.Temps, Temp{"gpu", v})
		}
		devs = append(devs, gpu)
	}
	return devs
}
