package hostd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaddyBuildJSONCompatibility(t *testing.T) {
	installer, err := filepath.Abs("../../deploy/install-caddy.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, initial, expected string
	}{
		{"unset", "unset", "unset"},
		{"set", "jsonv2", "jsonv2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			fake := "#!/bin/sh\nprintf '%s|%s\\n' \"$1\" \"${GOEXPERIMENT-unset}\" >> \"$CALLS\"\n"
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fake), 0700); err != nil {
				t.Fatal(err)
			}
			versions := filepath.Join(dir, "versions.env")
			if err := os.WriteFile(versions, []byte("CADDY_VERSION=2.10.2\n"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			caller := filepath.Join(dir, "caller")
			cmd := exec.Command("sh", "-c", `set -e; sh "$1" "$2" "$3"; printf '%s\n' "${GOEXPERIMENT-unset}" > "$4"`, "sh", installer, versions, filepath.Join(dir, "out"), caller)
			for _, env := range os.Environ() {
				if !strings.HasPrefix(env, "GOEXPERIMENT=") && !strings.HasPrefix(env, "PATH=") && !strings.HasPrefix(env, "CALLS=") {
					cmd.Env = append(cmd.Env, env)
				}
			}
			cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CALLS="+calls)
			if tc.name == "set" {
				cmd.Env = append(cmd.Env, "GOEXPERIMENT="+tc.initial)
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("installer failed: %v\n%s", err, output)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			want := "mod|" + tc.expected + "\nget|" + tc.expected + "\nbuild|nojsonv2\n"
			if string(got) != want {
				t.Errorf("Go calls: got %q, want %q", got, want)
			}
			got, err = os.ReadFile(caller)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.expected+"\n" {
				t.Errorf("caller environment: got %q, want %q", got, tc.expected)
			}
		})
	}
}
