package hostload

import (
	"path"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Match the fixed Node/libuv Linux OS measurements, including their sysinfo fallback.
func linuxMemory(s Source) (float64, bool) {
	if raw, err := s.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[0] == "MemAvailable:" && f[2] == "kB" {
				if n, err := strconv.ParseUint(f[1], 10, 64); err == nil && n > 0 {
					return float64(n) / 1024, true
				}
			}
		}
	}
	var info syscall.Sysinfo_t
	if s.Sysinfo(&info) != nil {
		return 0, false
	}
	return float64(info.Freeram) * float64(info.Unit) / (1024 * 1024), true
}
func linuxLoad(s Source) (float64, bool) {
	if raw, err := s.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 3 {
			values := [3]float64{}
			valid := true
			for i := range values {
				v, e := strconv.ParseFloat(fields[i], 64)
				values[i] = v
				valid = valid && e == nil
			}
			if valid {
				return values[0], true
			}
		}
	}
	var info syscall.Sysinfo_t
	if s.Sysinfo(&info) != nil {
		return 0, false
	}
	return float64(info.Loads[0]) / 65536, true
}
func availableCores(read func(string) ([]byte, error)) (int, error) {
	count := runtime.NumCPU()
	var mask [128]byte
	if _, _, err := syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, uintptr(len(mask)), uintptr(unsafe.Pointer(&mask[0]))); err == 0 {
		n := 0
		for _, b := range mask {
			for b != 0 {
				n += int(b & 1)
				b >>= 1
			}
		}
		if n > 0 {
			count = n
		}
	}
	if limit := cgroupCores(read); limit > 0 && limit < count {
		count = limit
	}
	if count < 1 {
		count = 1
	}
	return count, nil
}
func cgroupCores(read func(string) ([]byte, error)) int {
	raw, err := read("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 || !strings.HasPrefix(f[2], "/") {
			continue
		}
		clean := path.Clean(f[2])
		if clean != f[2] && f[2] != "/" {
			continue
		}
		if f[0] == "0" && f[1] == "" {
			dir := path.Join("/sys/fs/cgroup", clean)
			if _, err := read(path.Join(dir, "cgroup.controllers")); err != nil {
				return 0
			}
			limit := 0
			for {
				if data, e := read(path.Join(dir, "cpu.max")); e == nil {
					f := strings.Fields(string(data))
					if len(f) == 2 {
						n := quotaCores(f[0], f[1])
						if n > 0 && (limit == 0 || n < limit) {
							limit = n
						}
					}
				}
				if dir == "/sys/fs/cgroup" {
					break
				}
				dir = path.Dir(dir)
			}
			return limit
		}
		// The fixed libuv v1 reader uses the substring after :cpu, verbatim.
		if index := strings.Index(line, ":cpu,"); index >= 0 {
			suffix := line[index+len(":cpu,"):]
			if strings.Contains(suffix, "..") {
				continue
			}
			dir := path.Join("/sys/fs/cgroup", suffix)
			q, qe := read(path.Join(dir, "cpu.cfs_quota_us"))
			p, pe := read(path.Join(dir, "cpu.cfs_period_us"))
			if qe == nil && pe == nil {
				quota, e := strconv.ParseInt(strings.TrimSpace(string(q)), 10, 64)
				period, e2 := strconv.ParseInt(strings.TrimSpace(string(p)), 10, 64)
				if e == nil && e2 == nil && period > 0 && quota > 0 {
					return int(quota / period)
				}
			}
		}
	}
	return 0
}
func quotaCores(quota, period string) int {
	q, e := strconv.ParseInt(quota, 10, 64)
	p, pe := strconv.ParseInt(period, 10, 64)
	if e != nil || pe != nil || q <= 0 || p <= 0 {
		return 0
	}
	n := q / p
	if n < 1 {
		n = 1
	}
	return int(n)
}
