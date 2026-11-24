package hostload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
)

type loadFixture struct {
	at         time.Time
	user, idle uint64
	fail       string
	cores      int
	meter      *Meter
}

func newFixture() *loadFixture {
	f := &loadFixture{at: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), cores: 2}
	f.meter = New("/runtime", Source{GPU: func(context.Context) ([]byte, error) { return nil, errors.New("GPU unavailable") }, ReadFile: func(name string) ([]byte, error) {
		if f.fail == name {
			return nil, errors.New("read failure")
		}
		switch name {
		case "/proc/stat":
			return []byte(fmt.Sprintf("cpu 0 0 0 0 0 0\ncpu0 %d 0 0 %d 999999 0 999999\ncpu1 %d 0 0 %d 999999 0 999999\n", f.user, f.idle, f.user, f.idle)), nil
		case "/proc/loadavg":
			return []byte("1.5 1 1 1/2 3"), nil
		case "/proc/meminfo":
			return []byte("MemFree: 1048576 kB\nMemAvailable: 2097152 kB\n"), nil
		}
		return nil, errors.New("unexpected path")
	}, Sysinfo: func(*syscall.Sysinfo_t) error { return errors.New("sysinfo unavailable") }, Disk: func(p string) (uint64, uint64, error) {
		if p != "/runtime" {
			panic(p)
		}
		if f.fail == "disk" {
			return 0, 0, errors.New("disk failure")
		}
		return 1024, 4096, nil
	}, Cores: func() (int, error) {
		if f.fail == "cores" {
			return 0, errors.New("affinity failure")
		}
		return f.cores, nil
	}})
	f.meter.Sample(f.at)
	return f
}
func (f *loadFixture) tick(active uint64, delay time.Duration) {
	f.at = f.at.Add(delay)
	f.user += active
	f.idle += 100 - active
	f.meter.Sample(f.at)
}
func TestMinuteCPUWindowAndUnits(t *testing.T) {
	f := newFixture()
	m := f.meter.Measure(f.at)
	if _, ok := m["cpuPercent"]; ok {
		t.Fatal("未測定CPUを0にしました")
	}
	if m["loadAverage1m"] != 1.5 || m["logicalCpuCount"] != 2 || m["freeMemoryMB"] != 2048 || m["freeDiskMB"] != 4 {
		t.Fatal(m)
	}
	for n := 0; n < 11; n++ {
		f.tick(25, Interval)
	}
	if _, ok := f.meter.Measure(f.at)["cpuPercent"]; ok {
		t.Fatal("60秒未満です")
	}
	f.tick(25, Interval)
	if f.meter.Measure(f.at)["cpuPercent"] != 25 {
		t.Fatal(f.meter.Measure(f.at))
	}
	for n := 0; n < 36; n++ {
		f.tick(25, Interval)
	}
	for n := 0; n < 12; n++ {
		f.tick(75, Interval)
	}
	if f.meter.Measure(f.at)["cpuPercent"] != 75 {
		t.Fatal("5分全体でなく直近1分を測る必要があります")
	}
	if len(f.meter.samples) > 14 {
		t.Fatal("65秒を超えて保持しました")
	}
}
func TestJitterStaleReversalAndCounterReset(t *testing.T) {
	f := newFixture()
	for n := 0; n < 12; n++ {
		f.tick(50, 5001*time.Millisecond)
	}
	if f.meter.Measure(f.at)["cpuPercent"] != 50 {
		t.Fatal(f.meter.Measure(f.at))
	}
	if _, ok := f.meter.Measure(f.at.Add(10 * time.Second))["cpuPercent"]; ok {
		t.Fatal("古いCPUを出しました")
	}
	f.at = f.at.Add(-time.Hour)
	f.meter.Sample(f.at)
	if _, ok := f.meter.Measure(f.at)["cpuPercent"]; ok {
		t.Fatal("時刻逆行を出しました")
	}
	f = newFixture()
	for n := 0; n < 12; n++ {
		f.tick(25, Interval)
	}
	f.user = 0
	f.tick(0, Interval)
	if _, ok := f.meter.Measure(f.at)["cpuPercent"]; ok {
		t.Fatal("カウンタ逆行を出しました")
	}
}
func TestIndependentMissingMetricsAndMeasuredZero(t *testing.T) {
	mapping := map[string]string{"/proc/stat": "cpuPercent", "/proc/loadavg": "loadAverage1m", "/proc/meminfo": "freeMemoryMB", "disk": "freeDiskMB", "cores": "logicalCpuCount"}
	for failed, key := range mapping {
		t.Run(key, func(t *testing.T) {
			f := newFixture()
			for n := 0; n < 12; n++ {
				f.tick(0, Interval)
			}
			f.fail = failed
			if failed == "/proc/stat" {
				f.meter.Sample(f.at.Add(time.Second))
			}
			m := f.meter.Measure(f.at)
			if _, ok := m[key]; ok {
				t.Fatalf("欠測を出しました: %v", m)
			}
			if len(m) != 4 {
				t.Fatalf("無関係な欄を失いました: %v", m)
			}
		})
	}
	f := newFixture()
	for n := 0; n < 12; n++ {
		f.tick(0, Interval)
	}
	if v, ok := f.meter.Measure(f.at)["cpuPercent"]; !ok || v != 0 {
		t.Fatal("実測0を欠測にしました")
	}
	_, err := cpuCounters([]byte("cpu0 invalid 0 0 0 0 0"))
	if err == nil {
		t.Fatal("不正カウンタを受け入れました")
	}
}
func TestLinuxActualReadings(t *testing.T) {
	m := New(t.TempDir(), Source{})
	m.Sample(time.Now())
	v := m.Measure(time.Now())
	for _, k := range []string{"loadAverage1m", "logicalCpuCount", "freeMemoryMB", "freeDiskMB"} {
		x, ok := v[k]
		if !ok || math.IsNaN(x) || x < 0 {
			t.Fatalf("Linux実測 %s: %v", k, v)
		}
	}
	if _, ok := v["cpuPercent"]; ok {
		t.Fatal("実測1分前にCPUを出しました")
	}
}

func TestGPUHeartbeatMeasurements(t *testing.T) {
	cases := []struct {
		name, raw string
		fail      bool
		want      map[string]float64
	}{
		{"supported", "37, 1536\n", false, map[string]float64{"gpuPercent": 37, "freeVramMB": 1536}},
		{"partial utilization", "12, N/A\n", false, map[string]float64{"gpuPercent": 12}},
		{"partial memory", "N/A, 256\n", false, map[string]float64{"freeVramMB": 256}},
		{"zero", "0, 0\n", false, map[string]float64{"gpuPercent": 0, "freeVramMB": 0}},
		{"no GPU", "", false, nil},
		{"unsupported", "N/A, N/A\n", false, nil},
		{"read failure", "42, 100\n", true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			for i := 0; i < 12; i++ {
				f.tick(25, Interval)
			}
			f.meter.gpu.read = func(context.Context) ([]byte, error) {
				if tc.fail {
					return nil, errors.New("unavailable")
				}
				return []byte(tc.raw), nil
			}
			load := f.meter.Measure(f.at)
			data, err := json.Marshal(intake.HostStatus{Load: load})
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Load map[string]float64 `json:"load"`
			}
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]float64{"cpuPercent": 25, "loadAverage1m": 1.5, "logicalCpuCount": 2, "freeMemoryMB": 2048, "freeDiskMB": 4} {
				if got, ok := payload.Load[key]; !ok || got != want {
					t.Fatalf("%s: %v", key, payload.Load)
				}
			}
			for _, key := range []string{"gpuPercent", "freeVramMB"} {
				got, ok := payload.Load[key]
				want, exists := tc.want[key]
				if ok != exists || (ok && got != want) {
					t.Fatalf("%s: %v", key, payload.Load)
				}
			}
		})
	}
}

func TestGPUDelayDoesNotBlockExistingLoad(t *testing.T) {
	f := newFixture()
	started, release := make(chan struct{}), make(chan struct{})
	f.meter.gpu.read = func(context.Context) ([]byte, error) { close(started); <-release; return []byte("10, 20"), nil }
	defer close(release)
	begin := time.Now()
	load := f.meter.Measure(f.at)
	if time.Since(begin) > time.Second {
		t.Fatal("GPU read blocked heartbeat")
	}
	<-started
	if load["loadAverage1m"] != 1.5 || load["freeMemoryMB"] != 2048 {
		t.Fatal(load)
	}
	if _, ok := load["gpuPercent"]; ok {
		t.Fatal(load)
	}
}

func TestGPUDelayedResultAvailableOnNextMeasure(t *testing.T) {
	f := newFixture()
	finished := make(chan struct{})
	f.meter.gpu.read = func(ctx context.Context) ([]byte, error) {
		defer close(finished)
		select {
		case <-time.After(150 * time.Millisecond):
			return []byte("37, 1536\n"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	first := f.meter.Measure(f.at)
	for _, key := range []string{"gpuPercent", "freeVramMB"} {
		if _, ok := first[key]; ok {
			t.Fatalf("delayed GPU result appeared in first measurement: %v", first)
		}
	}
	<-finished
	second := f.meter.Measure(f.at)
	if second["gpuPercent"] != 37 || second["freeVramMB"] != 1536 {
		t.Fatalf("delayed GPU result missing: %v", second)
	}
}

func TestAMDGPUSysfsHeartbeat(t *testing.T) {
	cases := []struct {
		name, busy, total, used string
		want                    map[string]float64
	}{
		{"dedicated", "37", "2147483648", "536870912", map[string]float64{"gpuPercent": 37, "freeVramMB": 1536}},
		{"zero", "0", "1048576", "1048576", map[string]float64{"gpuPercent": 0, "freeVramMB": 0}},
		{"missing memory", "20", "", "", map[string]float64{"gpuPercent": 20}},
		{"invalid busy", "101", "1048576", "0", nil},
		{"invalid memory", "12", "1048576", "2097152", map[string]float64{"gpuPercent": 12}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			device := filepath.Join(root, "card0", "device")
			if err := os.MkdirAll(device, 0755); err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"vendor": "0x1002", "gpu_busy_percent": tc.busy, "mem_info_vram_total": tc.total, "mem_info_vram_used": tc.used} {
				if value != "" {
					if err := os.WriteFile(filepath.Join(device, name), []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			f := newFixture()
			f.meter.gpu.read = func(context.Context) ([]byte, error) { return readDRMGPU(root) }
			load := f.meter.Measure(f.at)
			data, err := json.Marshal(intake.HostStatus{Load: load})
			if err != nil {
				t.Fatal(err)
			}
			var status intake.HostStatus
			if err := json.Unmarshal(data, &status); err != nil {
				t.Fatal(err)
			}
			if status.Load["loadAverage1m"] != 1.5 || status.Load["freeMemoryMB"] != 2048 {
				t.Fatal(status.Load)
			}
			for _, key := range []string{"gpuPercent", "freeVramMB"} {
				got, ok := status.Load[key]
				want, exists := tc.want[key]
				if ok != exists || (ok && got != want) {
					t.Fatalf("%s: %v", key, status.Load)
				}
			}
		})
	}
}
