package hostload

import (
	"errors"
	"syscall"
	"testing"
)

func TestLinuxMemoryFallbackAndZero(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		failed bool
		want   float64
		ok     bool
	}{
		{"MemFree: 1 kB\nMemAvailable: 2048 kB", false, 2, true},
		{"MemAvailable: 0 kB", false, 3, true},
		{"invalid", false, 3, true},
		{"", true, 0, false},
	} {
		s := Source{ReadFile: func(string) ([]byte, error) { return []byte(tc.raw), nil }, Sysinfo: func(p *syscall.Sysinfo_t) error {
			if tc.failed {
				return errors.New("unavailable")
			}
			p.Freeram = 3 * 1024
			p.Unit = 1024
			return nil
		}}
		got, ok := linuxMemory(s)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%+v: %v %v", tc, got, ok)
		}
	}
	s := Source{ReadFile: func(string) ([]byte, error) { return nil, errors.New("proc unavailable") }, Sysinfo: func(*syscall.Sysinfo_t) error { return nil }}
	if got, ok := linuxMemory(s); !ok || got != 0 {
		t.Fatalf("実測0: %v %v", got, ok)
	}
	if got, ok := linuxLoad(s); !ok || got != 0 {
		t.Fatalf("実測0: %v %v", got, ok)
	}
	s.Sysinfo = func(p *syscall.Sysinfo_t) error { p.Loads[0] = 98304; return nil }
	if got, ok := linuxLoad(s); !ok || got != 1.5 {
		t.Fatalf("sysinfo load: %v %v", got, ok)
	}
}
func TestCgroupCPUHierarchyAndMissing(t *testing.T) {
	for _, tc := range []struct {
		name, group string
		files       map[string]string
		want        int
	}{
		{"親の制約", "0::/parent/child", map[string]string{"/sys/fs/cgroup/parent/child/cgroup.controllers": "cpu", "/sys/fs/cgroup/parent/child/cpu.max": "max 100000", "/sys/fs/cgroup/parent/cpu.max": "250000 100000", "/sys/fs/cgroup/cpu.max": "400000 100000"}, 2},
		{"小数は最低1", "0::/x", map[string]string{"/sys/fs/cgroup/x/cgroup.controllers": "cpu", "/sys/fs/cgroup/x/cpu.max": "50000 100000"}, 1},
		{"無制限", "0::/", map[string]string{"/sys/fs/cgroup/cgroup.controllers": "cpu", "/sys/fs/cgroup/cpu.max": "max 100000"}, 0},
		{"v1", "2:cpu,cpuacct:/x", map[string]string{"/sys/fs/cgroup/cpuacct:/x/cpu.cfs_quota_us": "300000", "/sys/fs/cgroup/cpuacct:/x/cpu.cfs_period_us": "100000"}, 3},
		{"不正な親参照", "0::/../outside", map[string]string{}, 0},
		{"controller欠測", "0::/x", map[string]string{"/sys/fs/cgroup/x/cpu.max": "100000 100000"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(p string) ([]byte, error) {
				if p == "/proc/self/cgroup" {
					return []byte(tc.group), nil
				}
				if s, ok := tc.files[p]; ok {
					return []byte(s), nil
				}
				return nil, errors.New("missing")
			}
			if got := cgroupCores(read); got != tc.want {
				t.Fatalf("%d != %d", got, tc.want)
			}
		})
	}
}
