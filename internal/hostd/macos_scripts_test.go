package hostd

import (
	"os/exec"
	"testing"
)

// The macOS installers are shipped to Mac hosts. Their tests stub uname, file and
// launchctl, so they run the real scripts on any PC with bash and python3.
func TestMacOSScripts(t *testing.T) {
	for _, tool := range []string{"bash", "python3", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s がありません: %v", tool, err)
		}
	}
	for name, script := range map[string]string{
		"live-check": "../../deploy/client/macos-live-check/install.test.sh",
		"apple":      "../../deploy/client/macos-apple/prepare.test.sh",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command("bash", script).CombinedOutput()
			if err != nil {
				t.Fatalf("%s の検査に失敗しました: %v\n%s", script, err, out)
			}
		})
	}
}
