package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/cause"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type savedInstruction struct {
	ID      string `json:"id"`
	JobID   int64  `json:"jobId"`
	Text    string `json:"text"`
	Context string `json:"context"`
	At      int64  `json:"at"`
}

func (e *Executor) instructionDir(id int64) string {
	return filepath.Join(e.Config.RuntimeDir, "control", "instructions", fmt.Sprint(id))
}
func (e *Executor) saveInstruction(cmd command) error {
	if strings.TrimSpace(cmd.Payload.Text) == "" && strings.TrimSpace(cmd.Payload.Context) == "" {
		return errors.New("追加指示の本文が空です")
	}
	sum := sha256.Sum256([]byte(cmd.ID))
	dir := e.instructionDir(cmd.Payload.JobID)
	file := filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
	if raw, err := os.ReadFile(file); err == nil {
		var saved savedInstruction
		if json.Unmarshal(raw, &saved) != nil || saved.ID != cmd.ID || saved.JobID != cmd.Payload.JobID || saved.Text != cmd.Payload.Text || saved.Context != cmd.Payload.Context {
			return errors.New("同じcommand IDの指示内容が一致しません")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	n := savedInstruction{cmd.ID, cmd.Payload.JobID, cmd.Payload.Text, cmd.Payload.Context, time.Now().UnixNano()}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return atomicfile.Write(file, ".instruction-", 0600, false, encode(n), nil)
}
func (e *Executor) attachInstructions(d *Descriptor) error {
	dir := e.instructionDir(d.ID)
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	list := []savedInstruction{}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil || len(raw) > 200000 {
			return cause.New("保存した追加指示を読めません", err)
		}
		var value savedInstruction
		if json.Unmarshal(raw, &value) != nil || value.JobID != d.ID || value.ID == "" {
			return errors.New("保存した追加指示が不正です")
		}
		list = append(list, value)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].At == list[j].At {
			return list[i].ID < list[j].ID
		}
		return list[i].At < list[j].At
	})
	var existing []map[string]any
	if d.View == nil {
		d.View = map[string]json.RawMessage{}
	}
	_ = json.Unmarshal(d.View["instructions"], &existing)
	seen := map[string]bool{}
	for _, item := range existing {
		if id, ok := item["id"].(string); ok {
			seen[id] = true
		}
	}
	for _, item := range list {
		id := "command:" + item.ID
		if !seen[id] {
			existing = append(existing, map[string]any{"id": id, "text": item.Text, "context": item.Context})
			seen[id] = true
		}
	}
	d.View["instructions"] = encode(existing)
	return nil
}
