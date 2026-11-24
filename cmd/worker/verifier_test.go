package main

import (
	"issue-desk-worker/internal/intake"
	"strings"
	"testing"
)

func TestManagedVerifierUnknownPreservesAndExplicitKnownClears(t *testing.T) {
	old := strings.Repeat("a", 64)
	next := strings.Repeat("b", 64)
	for _, state := range []*intake.VerifierState{nil, {Known: false, Available: false}, {Known: false, Available: true}, {Known: true, Available: false}} {
		if got := managedVerifier(old, "", state); got != old {
			t.Fatal("unknownで既存検証値を消しました")
		}
	}
	known := &intake.VerifierState{Known: true, Available: true}
	if managedVerifier(old, "", known) != "" {
		t.Fatal("明示無効化を反映しませんでした")
	}
	if managedVerifier(old, next, known) != next {
		t.Fatal("既知の更新を反映しませんでした")
	}
}
