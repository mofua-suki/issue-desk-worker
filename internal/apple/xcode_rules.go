package apple

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// darwinUserDirs returns the per-user temporary and cache directories that
// confstr reports (DARWIN_USER_TEMP_DIR and DARWIN_USER_CACHE_DIR, below
// /private/var/folders). xcrun and xcodebuild write their caches there instead
// of TMPDIR, so the sandbox must allow reading and writing them. Credentials and
// the runner's control state never live there. Missing values (non-Darwin
// hosts) are skipped.
func darwinUserDirs() []string {
	var dirs []string
	for _, name := range []string{"DARWIN_USER_TEMP_DIR", "DARWIN_USER_CACHE_DIR"} {
		out, err := exec.Command("/usr/bin/getconf", name).Output()
		if err != nil {
			continue
		}
		dir := strings.TrimSpace(string(out))
		if !filepath.IsAbs(dir) || filepath.Clean(dir) == "/" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
		dirs = append(dirs, filepath.Clean(dir))
	}
	return dirs
}

// xcodeRules lets Xcode's command line tools run inside the sandbox. Without
// user-preference-read, xcodebuild cannot read the accepted license
// (/Library/Preferences/com.apple.dt.Xcode.plist) and stops as if it had not been
// accepted; it also reads disk space (system-info) and writes /dev/dtracehelper.
// The user's text encoding file is read by Core Foundation at start-up.
// Confirmed on a Mac by worker apple probe.
func xcodeRules() string {
	q := strconv.Quote
	rules := "(allow user-preference-read) (allow system-info) (allow file-read* (subpath \"/Library/Preferences\"))\n" +
		"(allow file-write* (literal \"/dev/dtracehelper\") (literal \"/dev/tty\"))\n"
	for _, dir := range darwinUserDirs() {
		rules += "(allow file-read* (subpath " + q(dir) + "))\n"
	}
	// Simulator runtimes are mounted by cryptexd; the device set, Xcode's indexes,
	// DerivedData and xcodebuild's caches live below the real home (simctl and
	// xcodebuild do not follow HOME). Only these Developer/cache folders are
	// opened; the rest of the home, including ~/.issue-desk, stays denied.
	// The simulator and xcodebuild open GPU user clients, and arch/xcodebuild set
	// their own CPU affinity and grade; both are confirmed denials on a Mac.
	rules += "(allow file-ioctl (literal \"/dev/dtracehelper\")) (allow system-fsctl) (allow system-socket) (allow iokit-open-user-client)\n" +
		"(allow sysctl-write (sysctl-name \"kern.curproc_arch_affinity\") (sysctl-name \"kern.grade_cputype\"))\n" +
		"(allow file-read* (subpath \"/private/var/db/mds\"))\n" +
		"(allow file-read* (subpath \"/private/var/run/com.apple.security.cryptexd/mnt\") (subpath \"/private/etc\") (subpath \"/private/var/db/nsurlstoraged\"))\n"
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) && filepath.Clean(home) != "/" {
		rules += "(allow file-read* (literal " + q(filepath.Join(home, ".CFUserTextEncoding")) + "))\n"
		for _, dir := range []string{filepath.Dir(home), home, filepath.Join(home, "Library")} {
			rules += "(allow file-read* (literal " + q(dir) + "))\n"
		}
		rules += "(allow file-read* (literal " + q(filepath.Join(home, "Library/Caches")) + "))\n"
		// SwiftPM (used by xcodebuild for Flutter's generated plugin package) keeps its caches below the real home too.
		for _, dir := range []string{"Library/Developer", "Library/Caches/com.apple.dt.xcodebuild", "Library/HTTPStorages/com.apple.dt.xcodebuild",
			"Library/Caches/org.swift.swiftpm", "Library/org.swift.swiftpm", ".swiftpm", "Library/Logs/CoreSimulator", "Library/Caches/flutter_engine"} {
			rules += "(allow file-read* file-write* (subpath " + q(filepath.Join(home, dir)) + "))\n"
		}
	}
	return rules
}

// xcodeManifestSandboxOff are the xcodebuild user defaults (passed as
// -name=value arguments, so only that invocation sees them) that stop SwiftPM
// from wrapping package manifests and plugins in a sandbox of its own. A process
// inside sandbox-exec cannot apply another sandbox (sandbox_apply fails with
// "Operation not permitted"), and the runner's sandbox already confines them.
var xcodeManifestSandboxOff = []string{"-IDEPackageSupportDisableManifestSandbox=YES", "-IDEPackageSupportDisablePluginExecutionSandbox=YES"}

// writeXcodeShims writes xcrun and xcodebuild wrappers into the disposable HOME
// and returns their directory, to be put first in PATH. Flutter calls
// "xcrun xcodebuild" through PATH, so the wrappers add xcodeManifestSandboxOff
// (and a DerivedData location inside HOME) to every xcodebuild run without
// touching the user's Xcode preferences.
func writeXcodeShims(home string) (string, error) {
	dir := filepath.Join(home, "xcode-shims")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// DerivedData (and SwiftPM's checkouts in it) goes into the disposable HOME:
	// each run uses a new checkout path, so Xcode's default per-path folder below
	// ~/Library/Developer would pile up and never be cleaned.
	args := strings.Join(append(append([]string(nil), xcodeManifestSandboxOff...), "-IDECustomDerivedDataLocation="+filepath.Join(home, "DerivedData")), " ")
	scripts := map[string]string{
		"xcrun":      "#!/bin/sh\nif [ \"$1\" = xcodebuild ]; then shift; exec /usr/bin/xcrun xcodebuild " + args + " \"$@\"; fi\nexec /usr/bin/xcrun \"$@\"\n",
		"xcodebuild": "#!/bin/sh\nexec /usr/bin/xcrun xcodebuild " + args + " \"$@\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}
