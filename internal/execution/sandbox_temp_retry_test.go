package execution

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSandboxRetriesOnlyTerminalCapacityDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic, stream string
		code, attempts           int
	}{
		{"stderr errno", "mkdir: cannot create directory: No space left on device", "2", 1, 2},
		{"stderr quota", "write: Disk quota exceeded", "2", 1, 2},
		{"stdout text", "no space left on device", "1", 1, 1},
		{"prose mention", "test expected no space left on device but received another error", "2", 1, 1},
		{"successful output", "no space left on device", "2", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, r, root := sandboxPlacementFixture(t)
			tmpfs, err := os.MkdirTemp("/dev/shm", "worker-retry-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpfs)
			t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", tmpfs)
			calls := filepath.Join(root, "calls")
			script := "#!/bin/sh\nprintf 'attempt\\n' >> " + strconv.Quote(calls) + " || exit 1\ncase \"$TMPDIR\" in " + tmpfs + "/*) printf '%s\\n' " + strconv.Quote(tc.diagnostic) + " >&" + tc.stream + "; exit " + strconv.Itoa(tc.code) + ";; esac\n"
			if err := os.WriteFile(e.Config.Codex, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			out, err := e.sandbox(context.Background(), r, []string{"/bin/true"}, 10)
			data, readErr := os.ReadFile(calls)
			if err != nil || readErr != nil || strings.Count(string(data), "attempt\n") != tc.attempts {
				t.Fatalf("attempts=%q output=%+v err=%v read=%v", data, out, err, readErr)
			}
			wantCode := tc.code
			if tc.attempts == 2 {
				wantCode = 0
			}
			if out.Code != wantCode {
				t.Fatalf("code=%d want %d", out.Code, wantCode)
			}
		})
	}
}
