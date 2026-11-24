package redact

import (
	"strings"
	"testing"
)

func TestSecretAndKnownFormats(t *testing.T) {
	token := "idk_" + strings.Repeat("a", 43)
	got := Secret("token "+token+" and fixture-secret and issue-desk-worker", "fixture-secret")
	if strings.Contains(got, "aaaa") || strings.Contains(got, "fixture-secret") || !strings.Contains(got, "issue-desk-worker") {
		t.Fatal(got)
	}
	if got := Known("short idh_short and sk-ant-x"); strings.Contains(got, "idh_short") || strings.Contains(got, "sk-ant-x") {
		t.Fatalf("短い断片が残りました: %s", got)
	}
	if !HasCredential(token, "") || HasCredential("idh_short", "") || !HasCredential("x fixture-secret", "fixture-secret") {
		t.Fatal("whole credentials only")
	}
}
