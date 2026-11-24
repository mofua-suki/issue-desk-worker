package requeststore

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// 依頼の記録は保存して読み直せ、受付の内容と違えば違う項目を示す。
func TestStoreSavesLoadsAndNamesMismatch(t *testing.T) {
	s := Under(t.TempDir())
	r := Request{ID: "1794000000000-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: json.RawMessage(`{"number":1}`)}
	p, err := s.Load(r)
	if err != nil || p.Stage != "claimed" {
		t.Fatal(p, err)
	}
	p.Stage = "result"
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if saved, err := s.Saved(r.ID); err != nil || saved.Stage != "result" {
		t.Fatal(saved, err)
	}
	other := r
	other.Repo = "owner/other"
	if _, err := s.Load(other); err == nil || !strings.Contains(err.Error(), "リポジトリ") {
		t.Fatalf("食い違いを示していません: %v", err)
	}
	if err := s.Save(&Progress{Request: Request{ID: "../x"}}); err == nil {
		t.Fatal("不正な依頼IDを保存しました")
	}
}

// 壊れた記録は依頼IDと原因を示し、無い記録は「無い」として返す。
func TestStoreCorruptAndMissing(t *testing.T) {
	s := Under(t.TempDir())
	r := Request{ID: "1794000000000-aabbccdd", Kind: "merge_manual"}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.File(r.ID), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r); err == nil || !strings.Contains(err.Error(), "壊れています") || !strings.Contains(err.Error(), r.ID) {
		t.Fatalf("壊れた記録を示していません: %v", err)
	}
	if _, err := s.Saved("1794000000000-00000000"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("無い記録: %v", err)
	}
}

// パッチや承認の発言に < > & があっても、保存し直した依頼を同じ依頼として読み直せる。
func TestStoreReloadsPayloadWithHTMLCharacters(t *testing.T) {
	s := Under(t.TempDir())
	r := Request{ID: "1794000000000-aabbccdd", Kind: "pr_create", Repo: "owner/project", Payload: json.RawMessage(`{"patch":"a && b <- c > d"}`)}
	if _, err := s.Load(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r); err != nil {
		t.Fatalf("同じ依頼を食い違いとしました: %v", err)
	}
	if raw, err := os.ReadFile(s.File(r.ID)); err != nil || !strings.Contains(string(raw), `\u0026`) {
		t.Fatalf("保存し直しで & が置き換わる前提が崩れています: %s %v", raw, err)
	}
	big := Request{ID: "1794000000000-aabbccde", Kind: "pr_create", Payload: json.RawMessage(`{"id":9007199254740993}`)}
	if _, err := s.Load(big); err != nil {
		t.Fatal(err)
	}
	other := big
	other.Payload = json.RawMessage(`{"id":9007199254740992}`)
	if _, err := s.Load(other); err == nil {
		t.Fatal("桁の多い数の違いを見逃しました")
	}
	changed := r
	changed.Payload = json.RawMessage(`{"patch":"other"}`)
	if _, err := s.Load(changed); err == nil || !strings.Contains(err.Error(), "内容") {
		t.Fatalf("違う内容を見逃しました: %v", err)
	}
}
