// Package atomicfile persists a file through a synced temporary file and rename.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Stages identify the operation that failed during a write.
const (
	StageCreate  = "create"
	StageWrite   = "write"
	StageRename  = "rename"
	StageOpenDir = "open-dir"
	StageSyncDir = "sync-dir"
)

// Error identifies the stage that failed while preserving the underlying error.
type Error struct {
	Stage string
	Err   error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }
func failure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{stage, err}
}

// Write persists data, syncing the file before rename and its parent afterwards.
// prepare can configure ownership on the open temporary file.
func Write(target, pattern string, mode os.FileMode, applyMode bool, data []byte, prepare func(*os.File) error) error {
	return write(target, pattern, mode, applyMode, data, prepare, os.Rename)
}

func write(target, pattern string, mode os.FileMode, applyMode bool, data []byte, prepare func(*os.File) error, rename func(string, string) error) error {
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return failure(StageCreate, err)
	}
	name := f.Name()
	defer os.Remove(name)
	if applyMode {
		err = f.Chmod(mode)
	}
	if err == nil && prepare != nil {
		err = prepare(f)
	}
	if err == nil {
		var n int
		n, err = f.Write(data)
		if err == nil && n != len(data) {
			err = fmt.Errorf("short write")
		}
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return failure(StageWrite, err)
	}
	if closeErr != nil {
		return failure(StageWrite, closeErr)
	}
	if err = rename(name, target); err != nil {
		return failure(StageRename, err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return failure(StageOpenDir, err)
	}
	defer d.Close()
	return failure(StageSyncDir, d.Sync())
}
