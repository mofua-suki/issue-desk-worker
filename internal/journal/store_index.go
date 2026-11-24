package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type indexEntry struct {
	info   os.FileInfo
	change syscall.Timespec
	data   []byte
	repo   string
	issue  int64
}

// IDs returns journal IDs in the same filename order as List.
func (s *Store) IDs() ([]int64, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "state"))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, entry := range entries {
		var id int64
		if _, err := fmt.Sscanf(entry.Name(), "job-%d.json", &id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func sameIndexFile(a indexEntry, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && os.SameFile(a.info, info) && a.info.Size() == info.Size() && a.info.ModTime() == info.ModTime() && a.change == statChangeTime(stat)
}

// ListIssue validates all journal files and retains JSON only for the current
// issue. Cached JSON is decoded anew so callers cannot mutate it.
func (s *Store) ListIssue(repo string, issue int64) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.Root, "state"))
	if err != nil {
		return nil, err
	}
	next := make(map[string]indexEntry)
	var records []Record
	for _, entry := range entries {
		var id int64
		if _, err := fmt.Sscanf(entry.Name(), "job-%d.json", &id); err != nil {
			continue
		}
		// Match List's canonical ID-based file lookup, even for unusual names.
		path := s.file(id)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, readError(id, path, err)
		}
		cached, ok := s.index[path]
		if !ok || !sameIndexFile(cached, info) || (cached.repo == repo && cached.issue == issue && cached.data == nil) {
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, readError(id, path, err)
			}
			// Never associate bytes read during a concurrent replacement with the new file.
			after, err := os.Stat(path)
			if err != nil {
				return nil, readError(id, path, err)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return nil, readError(id, path, errors.New("ファイルの属性を読めません"))
			}
			var record Record
			if err := decodeRecord(id, path, data, &record); err != nil {
				// A corrupt file may belong to this Issue. Callers decide whether a
				// branch is still used from this list, so fail rather than skip it.
				if sameIndexFile(indexEntry{info: info, change: statChangeTime(stat)}, after) {
					return nil, err
				}
				return nil, readError(id, path, errors.New("読む間に置き換わりました"))
			}
			cached = indexEntry{info: info, change: statChangeTime(stat), repo: record.Repo, issue: record.Issue}
			if record.Repo == repo && record.Issue == issue {
				cached.data = data
			}
			if !sameIndexFile(cached, after) {
				return nil, readError(id, path, errors.New("読む間に置き換わりました"))
			}
		}
		if cached.repo == repo && cached.issue == issue {
			var record Record
			if err := json.Unmarshal(cached.data, &record); err != nil {
				return nil, &CorruptError{ID: id, Path: path, Cause: "JSONとして読めません（" + err.Error() + "）"}
			}
			record.loaded = true
			records = append(records, record)
		} else {
			cached.data = nil
		}
		next[path] = cached
	}
	s.index = next
	return records, nil
}
