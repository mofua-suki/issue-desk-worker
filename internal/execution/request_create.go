package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"net/http"
	"strings"
	"unicode/utf8"
)

type prPayload struct {
	Branch    string `json:"branch"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Patch     string `json:"patch"`
	Base      string `json:"base"`
	PatchHash string `json:"patchSha256"`
	Emergency bool   `json:"emergency"`
}

func validRequestBranch(branch string) bool {
	return gitwork.ValidBranchShape(branch) && !strings.HasPrefix(branch, "codex/issue-")
}

func (e *Executor) findRequestPull(ctx context.Context, r monitorRequest, branch string) (*gitea.Pull, error) {
	var found *gitea.Pull
	err := e.Gitea.EachPull(ctx, r.Repo, "all", 1000, func(p gitea.Pull) (bool, error) {
		if p.Head.Ref == branch && p.Head.Repo.FullName == r.Repo && strings.Contains(p.Body, "<!-- issue-desk-pr-request:"+r.ID+" -->") {
			found = &p
		}
		return found != nil, nil
	})
	if errors.Is(err, gitea.ErrListIncomplete) {
		return nil, errors.New("PR一覧を最後まで確認できません")
	}
	return found, err
}
func (e *Executor) createRequestedPullProgress(ctx context.Context, progress *requestProgress) (map[string]any, error) {
	r := progress.Request
	var p prPayload
	if json.Unmarshal(r.Payload, &p) != nil || p.Emergency || strings.TrimSpace(p.Title) == "" || strings.ContainsAny(p.Title, "\r\n") || utf8.RuneCountInString(p.Title) > 255 || strings.TrimSpace(p.Body) == "" || utf8.RuneCountInString(p.Body) > 60000 {
		return refusal("PRの作成依頼が不正です"), nil
	}
	if p.Patch != "" {
		sum := sha256.Sum256([]byte(p.Patch))
		if p.Branch != "" || len(p.Patch) > 200000 || strings.ContainsRune(p.Patch, 0) || strings.Contains(p.Patch, "GIT binary patch") || !gitwork.ValidSHA(p.Base) || p.PatchHash != hex.EncodeToString(sum[:]) {
			return refusal("パッチの内容・base・SHA256が不正です"), nil
		}
		p.Branch = "chat/" + r.ID
	} else if !validRequestBranch(p.Branch) {
		return refusal("PRの対象ブランチが不正です"), nil
	}
	prior, err := e.findRequestPull(ctx, r, p.Branch)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		if progress.Base != "" && (!samePull(*prior, r.Repo, p.Branch, progress.Base, progress.Head)) {
			return nil, errors.New("照合したPRが保存済みの対象・基点・headと一致しません")
		}
		return createdResult(*prior, p), nil
	}
	if progress.Stage == "pr-attempted" {
		return nil, errors.New("PR投稿の結果が未確認です。同じIDの目印を照合中で、自動再投稿しません")
	}
	repo, err := e.Gitea.Repo(ctx, r.Repo)
	if err != nil {
		return nil, err
	}
	if p.Branch == repo.DefaultBranch {
		return refusal("既定ブランチからPRは作れません"), nil
	}
	if p.Patch == "" {
		// 同じブランチから既定ブランチへの開いているPRが既にあれば、新しく作らずそのPRを結果にする(GiteaはHTTP 409で重複作成を拒む)。
		existing, err := e.findOpenBranchPull(ctx, r.Repo, p.Branch, repo.DefaultBranch)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return createdResult(*existing, p), nil
		}
	}
	if p.Patch != "" {
		if err := e.prepareRequestedPatch(ctx, r, p, repo.DefaultBranch); err != nil {
			return nil, err
		}
	}
	var branch struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	route := branchRoute(r.Repo, p.Branch)
	if err := e.Gitea.Request(ctx, "GET", route, nil, &branch); err != nil {
		return nil, err
	}
	if !gitwork.ValidSHA(branch.Commit.ID) {
		return nil, errors.New("ブランチのコミットが不正です")
	}
	title := e.redact(p.Title)
	body := strings.ReplaceAll(e.redact(p.Body), "<!-- issue-desk-pr-request:", "&lt;!-- issue-desk-pr-request:") + "\n\n<!-- issue-desk-pr-request:" + r.ID + " -->\n"
	progress.Stage = "pr-attempted"
	progress.Branch = p.Branch
	progress.Base = repo.DefaultBranch
	progress.Head = branch.Commit.ID
	if err := e.saveRequest(progress); err != nil {
		return nil, err
	}
	pull, err := e.Gitea.CreatePull(ctx, r.Repo, p.Branch, repo.DefaultBranch, title, body)
	if err != nil {
		if rejectedPost(err) {
			progress.Stage = "claimed"
			if saveErr := e.saveRequest(progress); saveErr != nil {
				return nil, saveErr
			}
		}
		found, findErr := e.findRequestPull(ctx, r, p.Branch)
		var conflict *gitea.Error
		if found == nil && findErr == nil && errors.As(err, &conflict) && conflict.Status == http.StatusConflict {
			// HTTP 409は同じブランチのPRが既にあることを示す確定した拒否なので、再試行で待ち続けず結果として終える。
			if p.Patch == "" {
				if existing, openErr := e.findOpenBranchPull(ctx, r.Repo, p.Branch, repo.DefaultBranch); openErr == nil && existing != nil {
					return createdResult(*existing, p), nil
				}
			}
			return refusal("GiteaがPRの作成を拒否しました(HTTP 409。同じブランチのPRが既にある可能性があります)"), nil
		}
		if findErr != nil || found == nil {
			return nil, err
		}
		pull = *found
	}
	if pull.Number < 1 {
		return nil, errors.New("PRの作成結果が不正です")
	}
	return createdResult(pull, p), nil
}

// findOpenBranchPull は、同じリポジトリ内の同じブランチを元にし、baseへ向いた開いているPRを返す(無ければnil)。
// 別の枝へ向いたPRは、依頼の求めるPRではないので数えない。
func (e *Executor) findOpenBranchPull(ctx context.Context, repo, branch, base string) (*gitea.Pull, error) {
	var found *gitea.Pull
	err := e.Gitea.EachPull(ctx, repo, "open", 1000, func(p gitea.Pull) (bool, error) {
		if p.State == "open" && p.Head.Ref == branch && p.Base.Ref == base && strings.EqualFold(p.Head.Repo.FullName, repo) {
			found = &p
		}
		return found != nil, nil
	})
	if errors.Is(err, gitea.ErrListIncomplete) {
		return nil, errors.New("PR一覧を最後まで確認できません")
	}
	return found, err
}
func createdResult(p gitea.Pull, r prPayload) map[string]any {
	out := map[string]any{"status": "created", "number": p.Number, "url": p.URL, "head": p.Head.SHA, "base": p.Base.Ref}
	if r.Patch != "" {
		out["branch"] = r.Branch
		out["patchSha256"] = r.PatchHash
	}
	return out
}
