package execution

import (
	"errors"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitea"
)

func outcomePull(merged bool, mergeCommit string) gitea.Pull {
	p := gitea.Pull{Number: 7, Merged: merged, State: "open", Mergeable: true, MergeCommitSHA: mergeCommit}
	p.Head.Ref = "feature"
	p.Head.SHA = strings.Repeat("a", 40)
	p.Head.Repo.FullName = "owner/project"
	p.Base.Ref = "main"
	p.Base.Repo.FullName = "owner/project"
	return p
}

func TestMergeOutcomeDecidesFromObservedPull(t *testing.T) {
	head := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	decide := func(p gitea.Pull, ancestor func(string) (bool, error)) map[string]any {
		t.Helper()
		r, err := mergeOutcome(p, 7, "owner/project", "feature", "main", head, "未確認", "理由", ancestor)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := decide(outcomePull(true, head), nil); r["status"] != "merged" || r["mergeCommit"] != head {
		t.Fatalf("同じSHAは成功: %v", r)
	}
	if r := decide(outcomePull(true, other), func(string) (bool, error) { return true, nil }); r["status"] != "merged" || r["mergeCommit"] != other {
		t.Fatalf("祖先を確認できたら成功: %v", r)
	}
	for name, ancestor := range map[string]func(string) (bool, error){
		"祖先でない":  func(string) (bool, error) { return false, nil },
		"確認手段なし": nil,
	} {
		r := decide(outcomePull(true, other), ancestor)
		if r["status"] != "failed" || !strings.Contains(r["message"].(string), "マージ結果未確認") {
			t.Fatalf("%s: 成功とも未マージとも決めず、結果未確認にする: %v", name, r)
		}
	}
	gitErr := errors.New("git")
	r, err := mergeOutcome(outcomePull(true, other), 7, "owner/project", "feature", "main", head, "未確認", "理由", func(string) (bool, error) { return false, gitErr })
	if r != nil || !errors.Is(err, gitErr) || !strings.Contains(err.Error(), "理由") {
		t.Fatalf("祖先確認のエラーとマージ要求の理由を返す: %v %v", r, err)
	}
	r = decide(outcomePull(false, ""), nil)
	if r["status"] != "failed" || !strings.Contains(r["message"].(string), "merged=false, open=true, mergeable=true") || !strings.Contains(r["message"].(string), "理由") {
		t.Fatalf("実際の未マージは観測状態と理由を添える: %v", r)
	}
	wrong := outcomePull(true, head)
	wrong.Base.Ref = "other"
	if r := decide(wrong, nil); r["status"] != "failed" || strings.Contains(r["message"].(string), "merged=false") {
		t.Fatalf("対象が違うPRは未マージと決めない: %v", r)
	}
}
