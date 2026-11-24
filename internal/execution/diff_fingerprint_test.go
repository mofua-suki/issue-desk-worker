package execution

import (
	"context"
	"regexp"
	"testing"
)

// A held change reports its fingerprint with its head, so the intake can keep
// an approval when only the default branch is merged in later.
func TestHeldReportCarriesDiffFingerprintWithHead(t *testing.T) {
	f := newPipeline(t, "sql")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatal(state, err)
	}
	pattern := regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	if !pattern.MatchString(state.DiffFingerprint) || state.HeadSHA == "" {
		t.Fatalf("保留の報告に指紋とheadがありません: %q %q", state.DiffFingerprint, state.HeadSHA)
	}
	if state.DiffFingerprint == "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatal("変更があるのに空差分の指紋を報告しました")
	}
	for _, sent := range f.ReceivedStates {
		if sent.DiffFingerprint != "" && sent.HeadSHA == "" {
			t.Fatalf("headなしで指紋を送りました: %+v", sent)
		}
	}
}

func TestMergedReportOmitsDiffFingerprint(t *testing.T) {
	f := newPipeline(t, "approve")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatal(state, err)
	}
	if state.DiffFingerprint != "" {
		t.Fatalf("マージ後の報告に指紋を付けました: %s", state.DiffFingerprint)
	}
}
