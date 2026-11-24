package apple

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// SwiftPMの入れ子の隔離を止める指定と使い捨てのDerivedDataの場所は、xcodebuildを直接呼んでもxcrun経由で呼んでも付き、xcrunのほかの道具には付かない。
func TestXcodeShimsTurnOffManifestSandboxForXcodebuildOnly(t *testing.T) {
	home := t.TempDir()
	dir, err := writeXcodeShims(home)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]string{
		"xcrun":      {`if [ "$1" = xcodebuild ]; then shift; exec /usr/bin/xcrun xcodebuild -IDEPackageSupportDisableManifestSandbox=YES -IDEPackageSupportDisablePluginExecutionSandbox=YES -IDECustomDerivedDataLocation=` + home + `/DerivedData "$@"; fi`, `exec /usr/bin/xcrun "$@"`},
		"xcodebuild": {`exec /usr/bin/xcrun xcodebuild -IDEPackageSupportDisableManifestSandbox=YES -IDEPackageSupportDisablePluginExecutionSandbox=YES -IDECustomDerivedDataLocation=` + home + `/DerivedData "$@"`},
	} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
		body, _ := os.ReadFile(path)
		for _, line := range want {
			if !strings.Contains(string(body), line) {
				t.Fatalf("%s に %q がありません:\n%s", name, line, body)
			}
		}
		if out, err := exec.Command("/bin/sh", "-n", path).CombinedOutput(); err != nil {
			t.Fatalf("%s の構文: %v %s", name, err, out)
		}
	}
}
