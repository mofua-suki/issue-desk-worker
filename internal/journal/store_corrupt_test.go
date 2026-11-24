package journal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 壊れた記録が1件あっても、ほかの仕事は一覧に出し、壊れた記録は消さずに1回だけ知らせる。
func TestCorruptJournalIsSkippedAndKept(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var reported []*CorruptError
	store.Corrupt = func(e *CorruptError) { reported = append(reported, e) }
	for _, r := range []*Record{{ID: 1, Repo: "owner/repo", Issue: 7}, {ID: 3, Repo: "owner/repo", Issue: 7}} {
		if err := store.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	broken := []byte("{broken")
	if err := os.WriteFile(store.file(2), broken, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.file(4), []byte(`{"id":5}`), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		all, err := store.List()
		if err != nil || len(all) != 2 || all[0].ID != 1 || all[1].ID != 3 {
			t.Fatalf("一覧が壊れた記録で止まりました: %v %v", all, err)
		}
		// 壊れた記録はこのIssueのものかもしれない。ブランチを使っているかの判定に使うので、飛ばさず失敗にする。
		_, err = store.ListIssue("owner/repo", 7)
		var corrupt *CorruptError
		if !errors.As(err, &corrupt) || corrupt.ID != 2 {
			t.Fatalf("Issueの一覧が壊れた記録を飛ばしました: %v", err)
		}
	}
	if len(reported) != 2 || reported[0].ID != 2 || reported[1].ID != 4 || !strings.Contains(reported[1].Cause, "5") {
		t.Fatalf("壊れた記録の知らせが1件1回になっていません: %v", reported)
	}
	if data, err := os.ReadFile(store.file(2)); err != nil || !bytes.Equal(data, broken) {
		t.Fatal("壊れた記録を消しました")
	}
	// 直したら（別の版になったら）次に壊れたときにまた知らせる。
	if err := os.WriteFile(store.file(2), []byte("{again"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil || len(reported) != 3 {
		t.Fatalf("新しく壊れた版を知らせていません: %v %v", reported, err)
	}
}

// 1件を読むときは、壊れた記録を新しい仕事と扱わず、番号と場所の入った失敗を返す。
func TestCorruptJournalLoadNamesJobAndPath(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.WriteFile(store.file(9), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = store.Load(9)
	var corrupt *CorruptError
	if !errors.As(err, &corrupt) || corrupt.ID != 9 || !strings.Contains(err.Error(), store.file(9)) || !strings.Contains(err.Error(), "仕事9") {
		t.Fatalf("番号と場所がありません: %v", err)
	}
}
