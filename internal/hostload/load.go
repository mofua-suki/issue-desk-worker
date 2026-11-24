// Package hostload preserves the Linux host heartbeat measurements and missing-data semantics.
package hostload

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const Interval = 5 * time.Second
const Window = time.Minute

type Source struct {
	ReadFile func(string) ([]byte, error)
	Disk     func(string) (uint64, uint64, error)
	Cores    func() (int, error)
	Sysinfo  func(*syscall.Sysinfo_t) error
	GPU      func(context.Context) ([]byte, error)
}
type cpuSample struct {
	at   time.Time
	cpus [][5]uint64
}
type Meter struct {
	mu      sync.Mutex
	source  Source
	root    string
	samples []cpuSample
	gpu     gpuSampler
}

func New(root string, source Source) *Meter {
	if source.ReadFile == nil {
		source.ReadFile = os.ReadFile
	}
	if source.Disk == nil {
		source.Disk = func(p string) (uint64, uint64, error) {
			var s syscall.Statfs_t
			err := syscall.Statfs(p, &s)
			return s.Bavail, uint64(s.Bsize), err
		}
	}
	if source.Sysinfo == nil {
		source.Sysinfo = syscall.Sysinfo
	}
	if source.Cores == nil {
		source.Cores = func() (int, error) { return availableCores(source.ReadFile) }
	}
	if source.GPU == nil {
		source.GPU = readGPU
	}
	return &Meter{root: root, source: source, gpu: gpuSampler{read: source.GPU}}
}
func cpuCounters(raw []byte) ([][5]uint64, error) {
	var out [][5]uint64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || len(fields[0]) < 4 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if _, err := strconv.Atoi(fields[0][3:]); err != nil {
			continue
		}
		if len(fields) < 7 {
			return nil, errors.New("CPU counters incomplete")
		}
		var one [5]uint64
		// Node os.cpus().times uses user,nice,sys,idle,irq; iowait/softirq/guest are not part of that contract.
		for n, index := range []int{1, 2, 3, 4, 6} {
			v, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil {
				return nil, err
			}
			one[n] = v
		}
		out = append(out, one)
	}
	if len(out) == 0 {
		return nil, errors.New("CPU counters unavailable")
	}
	return out, nil
}
func (m *Meter) Sample(at time.Time) {
	at = time.UnixMilli(at.UnixMilli())
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := m.source.ReadFile("/proc/stat")
	if err != nil {
		m.samples = nil
		return
	}
	cpus, err := cpuCounters(raw)
	if err != nil {
		m.samples = nil
		return
	}
	if len(m.samples) > 0 && !at.After(m.samples[len(m.samples)-1].at) {
		m.samples = nil
	}
	m.samples = append(m.samples, cpuSample{at, cpus})
	keep := 0
	for keep < len(m.samples) && m.samples[keep].at.Before(at.Add(-65*time.Second)) {
		keep++
	}
	m.samples = append([]cpuSample(nil), m.samples[keep:]...)
}
func (m *Meter) Run(ctx context.Context) {
	m.Sample(time.Now())
	timer := time.NewTicker(Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			m.Sample(time.Now())
		}
	}
}
func (m *Meter) Measure(at time.Time) map[string]float64 {
	at = time.UnixMilli(at.UnixMilli())
	m.mu.Lock()
	out := map[string]float64{}
	put := func(k string, v float64) {
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 {
			out[k] = v
		}
	}
	if v, ok := linuxLoad(m.source); ok {
		put("loadAverage1m", v)
	}
	if n, err := m.source.Cores(); err == nil && n >= 1 {
		put("logicalCpuCount", float64(n))
	}
	if v, ok := linuxMemory(m.source); ok {
		put("freeMemoryMB", v)
	}
	if blocks, size, err := m.source.Disk(m.root); err == nil {
		put("freeDiskMB", float64(blocks)*float64(size)/(1024*1024))
	}
	if len(m.samples) > 0 {
		end := m.samples[len(m.samples)-1]
		if !at.Before(end.at) && at.Sub(end.at) <= Interval {
			begin := -1
			for n := range m.samples {
				if !m.samples[n].at.After(end.at.Add(-Window)) {
					begin = n
				}
			}
			if begin >= 0 && len(end.cpus) == len(m.samples[begin].cpus) {
				var total, idle float64
				valid := true
				for n, cpu := range end.cpus {
					for k, v := range cpu {
						old := m.samples[begin].cpus[n][k]
						if v < old {
							valid = false
							continue
						}
						delta := float64(v - old)
						total += delta
						if k == 3 {
							idle += delta
						}
					}
				}
				if valid && total > 0 {
					put("cpuPercent", (total-idle)/total*100)
				}
			}
		}
	}
	m.mu.Unlock()
	for k, v := range m.gpu.measure() {
		put(k, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
