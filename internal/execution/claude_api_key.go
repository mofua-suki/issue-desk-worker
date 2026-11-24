package execution

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

var claudeAPIKeyPattern = regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{20,}$`)

// claudeAPIKey prefers the key file placed on this PC and otherwise uses the
// key distributed by the intake.
func (e *Executor) claudeAPIKey() string {
	if key := e.claudeAPIKeyFile(); key != "" {
		return key
	}
	return e.distributedClaudeAPIKey()
}

// claudeAPIKeyFile reads the protected file on every call; failures are indistinguishable from absence.
func (e *Executor) claudeAPIKeyFile() string {
	if e.Config.WorkerTokenFile == "" {
		return ""
	}
	path := filepath.Join(filepath.Dir(e.Config.WorkerTokenFile), "claude-api-key")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode() != 0600 || info.Size() > 4096 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return ""
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	if !claudeAPIKeyPattern.Match(data) {
		return ""
	}
	return string(data)
}
