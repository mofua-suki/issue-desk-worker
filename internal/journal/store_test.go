package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJournalSurvivesRestartAndExcludesSecondWriter(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(root); err == nil {
		other.Close()
		t.Fatal("同じ状態へ二重起動しました")
	}
	r := &Record{ID: 1, Epoch: 3, Repo: "owner/repo", Issue: 2, Stage: "review", Session: "session", Usage: []json.RawMessage{json.RawMessage(`{"inputTokens":0}`)}}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.Load(1)
	if err != nil || loaded.Stage != "review" || loaded.Epoch != 3 || string(loaded.Usage[0]) != `{"inputTokens":0}` {
		t.Fatal(loaded, err)
	}
	info, err := os.Stat(store.file(1))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("保存した仕事が保護されていません")
	}
}
func TestCorruptRecordIsNotTreatedAsMissing(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.WriteFile(store.file(1), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(1); err == nil {
		t.Fatal("壊れた状態を新規仕事として扱いました")
	}
}

func TestReportCompletionReceiptSurvivesStoreReopen(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	r := &Record{ID: 1, Runs: 4, Session: "same", Stage: "test_report", ReportCompletion: &ReportCompletion{ID: "original-report", Generation: 2, Stage: "test_report", Original: json.RawMessage(`{"status":"completed","deleteFiles":["original"]}`), Attempted: true, Validated: true, EditsStarted: true, EditsApplied: true, Error: "remaining"}}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Load(1)
	if err != nil || saved.Runs != 4 || saved.Session != "same" || saved.ReportCompletion.ID != "original-report" || !saved.ReportCompletion.Attempted || !saved.ReportCompletion.EditsApplied || string(saved.ReportCompletion.Original) != string(r.ReportCompletion.Original) {
		t.Fatalf("receipt lost: %#v %v", saved, err)
	}
}

func TestReportCompletionRunCountSurvivesStoreReopen(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&Record{ID: 1, Generation: 2, Runs: 4, ReportCompletionRuns: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	r, err := store.Load(1)
	if err != nil || r.Runs != 4 || r.ReportCompletionRuns != 2 {
		t.Fatal(r, err)
	}
}
