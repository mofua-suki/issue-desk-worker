// Package tempdir removes the per-run temporary directories of fixed tests,
// AI sessions and native tests, including ones a crashed run left.
package tempdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Remove deletes root. Go module caches and similar trees have read-only
// directories, so owned directories are made writable first; one that
// cannot be does not stop the removal of the rest.
func Remove(root string) error {
	var failures []error
	_ = filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				failures = append(failures, err)
			}
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if err := os.Chmod(name, 0700); err != nil && !errors.Is(err, fs.ErrNotExist) {
				failures = append(failures, err)
			}
		}
		return nil
	})
	if err := os.RemoveAll(root); err != nil {
		return errors.Join(append(failures, err)...)
	}
	return nil
}

// Sweep removes the directories directly under each of dirs whose names match
// one of patterns. Call it only while nothing else uses those directories.
func Sweep(dirs, patterns []string) error {
	var failures []error
	for _, dir := range dirs {
		for _, pattern := range patterns {
			matches, err := filepath.Glob(filepath.Join(dir, pattern))
			if err != nil {
				failures = append(failures, err)
				continue
			}
			for _, match := range matches {
				if info, err := os.Lstat(match); err != nil || !info.IsDir() {
					continue
				}
				if err := Remove(match); err != nil {
					failures = append(failures, fmt.Errorf("%s: %w", filepath.Base(match), err))
				}
			}
		}
	}
	return errors.Join(failures...)
}
