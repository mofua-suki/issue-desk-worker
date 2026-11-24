package journal

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func watchJournalOpens(t *testing.T, root string) func() int {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if _, err = syscall.InotifyAddWatch(fd, filepath.Join(root, "state"), syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	return func() int {
		count := 0
		buf := make([]byte, 65536)
		for {
			n, err := syscall.Read(fd, buf)
			if err == syscall.EAGAIN {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off < n; {
				size := int(binary.LittleEndian.Uint32(buf[off+12:]))
				name := strings.TrimRight(string(buf[off+16:off+16+size]), "\x00")
				if strings.HasPrefix(name, "job-") && strings.HasSuffix(name, ".json") {
					count++
				}
				off += 16 + size
			}
		}
		return count
	}
}

func TestSourceLookupRetainsOnlyMatchingPayloads(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := int64(1); i <= 32; i++ {
		if err := s.Save(&Record{ID: i, Repo: "other/repo", Issue: i, Head: strings.Repeat("x", 1<<20)}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := s.ListIssue("owner/repo", 1)
	if err != nil || len(got) != 0 {
		t.Fatalf("lookup: %d records, %v", len(got), err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)
	if after.HeapAlloc > before.HeapAlloc+12<<20 {
		t.Fatalf("unrelated payloads retained: heap grew by %d bytes", after.HeapAlloc-before.HeapAlloc)
	}
}

func TestSourceLookupSwitchesIssue(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, r := range []Record{{ID: 1, Repo: "a", Issue: 1, Head: "first"}, {ID: 2, Repo: "b", Issue: 1, Head: "second"}} {
		if err := s.Save(&r); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ repo, head string }{{"a", "first"}, {"b", "second"}, {"a", "first"}} {
		got, err := s.ListIssue(tc.repo, 1)
		if err != nil || len(got) != 1 || got[0].Head != tc.head {
			t.Fatalf("lookup %s: %+v, %v", tc.repo, got, err)
		}
		got[0].Head = "mutated"
	}
}

func TestSourceLookupNoFullReread(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := int64(1); i <= 500; i++ {
		if err := s.Save(&Record{ID: i, Repo: "owner/repo", Issue: i % 3}); err != nil {
			t.Fatal(err)
		}
	}
	opens := watchJournalOpens(t, s.Root)
	first, err := s.ListIssue("owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if opens() != 500 {
		t.Fatal("initial lookup did not read all records")
	}
	second, err := s.ListIssue("owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := opens(); n != 0 {
		t.Fatalf("second lookup opened %d records", n)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("lookup results differ")
	}
}

func TestSourceLookupSeesUpdates(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := &Record{ID: 1, Repo: "owner/repo", Issue: 1, Head: "before"}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListIssue(r.Repo, r.Issue); err != nil {
		t.Fatal(err)
	}
	r.Head = "saved"
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListIssue(r.Repo, r.Issue)
	if err != nil || len(got) != 1 || got[0].Head != "saved" {
		t.Fatalf("save: %+v %v", got, err)
	}
	if err := os.WriteFile(s.file(1), []byte(`{"id":1,"repo":"owner/repo","issue":1,"head":"external"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListIssue(r.Repo, r.Issue)
	if err != nil || len(got) != 1 || got[0].Head != "external" {
		t.Fatalf("external: %+v %v", got, err)
	}
}
