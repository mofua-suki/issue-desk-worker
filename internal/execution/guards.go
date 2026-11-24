package execution

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"issue-desk-worker/internal/gitwork"
)

var destructiveSQL = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|DATABASE|COLUMN)|TRUNCATE\s+TABLE|DELETE\s+FROM)\b`)
var secretLiteral = regexp.MustCompile(`\b(?:id[kh]_[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,})|sk-ant-[A-Za-z0-9_-]{20,}`)

func testPath(name string) bool {
	for _, root := range []string{"test/", "tests/", "server/test/", "client/test/", "shared/test/"} {
		if strings.HasPrefix(name, root) {
			return true
		}
	}
	return strings.HasSuffix(name, "_test.go")
}
func validateSnapshot(s gitwork.Snapshot) error {
	for _, name := range s.Paths {
		if name == ".claude-staged" || strings.HasPrefix(name, ".claude-staged/") {
			return errors.New(".claude-stagedはコミットできません")
		}
		if name == "config.local.json" || name == ".env" || strings.HasPrefix(name, "private/") || strings.HasPrefix(name, "runtime/") || strings.Contains(name, "/.credentials") || strings.HasPrefix(name, ".ssh/") {
			return errors.New("資格情報・実行状態に該当する変更は公開できません")
		}
		if filepath.IsAbs(name) || strings.HasPrefix(filepath.Clean(name), "..") {
			return errors.New("変更範囲が作業ツリー外です")
		}
	}
	inHunk := false
	for _, line := range strings.Split(s.Diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			inHunk = false
		} else if strings.HasPrefix(line, "@@ ") {
			inHunk = true
		}
		if strings.HasPrefix(line, "+") && (inHunk || !strings.HasPrefix(line, "+++ ")) && secretLiteral.MatchString(line) {
			return errors.New("追加行に資格情報らしい値があります")
		}
	}
	return nil
}

// Markdown used as agent instructions can lead to executable changes.
// Keep these files subject to the same approval as executable sources.
func instructionMarkdown(name string) bool {
	lower := strings.ToLower(filepath.ToSlash(name))
	switch filepath.Base(lower) {
	case "agents.md", "claude.md", "gemini.md", "copilot-instructions.md":
		return true
	}
	for _, directory := range []string{"/.claude/commands/", "/.claude/rules/", "/.claude/skills/", "/.claude/agents/", "/.github/instructions/", "/.github/prompts/", "/.cursor/rules/"} {
		if strings.Contains("/"+lower, directory) {
			return true
		}
	}
	return false
}
func documentPath(name string) bool {
	return strings.HasSuffix(name, ".md") && !instructionMarkdown(name)
}

// quotedDiffPath decodes one Git C-quoted path and returns the remaining header.
func quotedDiffPath(header string) (string, string, bool) {
	if !strings.HasPrefix(header, "\"") {
		return "", "", false
	}
	for i := 1; i < len(header); i++ {
		if header[i] == '\\' {
			i++
			continue
		}
		if header[i] == '"' {
			path, err := strconv.Unquote(header[:i+1])
			return path, header[i+1:], err == nil
		}
	}
	return "", "", false
}

func sqlHold(s gitwork.Snapshot) bool {
	current := ""
	added := []string{}
	held := func() bool {
		if documentPath(current) {
			return false
		}
		return (instructionMarkdown(current) || !testPath(current)) && destructiveSQL.MatchString(strings.Join(added, "\n"))
	}
	for _, line := range strings.Split(s.Diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			if held() {
				return true
			}
			current = ""
			added = nil
			header := strings.TrimPrefix(line, "diff --git ")
			left, rest, leftOK := quotedDiffPath(header)
			right, tail, rightOK := quotedDiffPath(strings.TrimPrefix(rest, " "))
			quotedOK := leftOK && strings.HasPrefix(rest, " ") && rightOK && tail == ""
			for _, name := range s.Paths {
				if line == "diff --git a/"+name+" b/"+name ||
					(quotedOK && left == "a/"+name && right == "b/"+name) {
					current = name
					break
				}
			}
		} else if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added = append(added, strings.TrimPrefix(line, "+"))
		}
	}
	return held()
}

func (e *Executor) permissionFiles(s gitwork.Snapshot, repo string) ([]string, error) {
	if e.Config.ReleasePolicy == nil {
		return nil, errors.New("受付から権限保護対象が配信されていません")
	}
	policy := e.Config.ReleasePolicy
	names := policy.PermissionFiles
	if policy.Repositories != nil {
		found := false
		for project, entry := range policy.Repositories {
			if strings.EqualFold(project, repo) {
				if found {
					return nil, errors.New("リポジトリの権限保護対象が重複しています")
				}
				names = entry.PermissionFiles
				found = true
			}
		}
		if !found {
			return nil, errors.New("対象リポジトリの権限保護対象が配信されていません")
		}
	}
	if names == nil {
		return nil, errors.New("対象リポジトリの権限保護対象が配信されていません")
	}
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	files := []string{}
	for _, name := range s.Paths {
		if set[name] {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return files, nil
}
func scopeContains(scope []string, name string) bool {
	for _, prefix := range scope {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "*" || name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	return false
}

func (e *Executor) validatePending(ctx context.Context, w *gitwork.Workspace) error {
	paths, err := w.Pending(ctx)
	if err != nil {
		return err
	}
	if err := validateSnapshot(gitwork.Snapshot{Paths: paths}); err != nil {
		return err
	}
	if err := w.ValidateFiles(paths); err != nil {
		return err
	}
	for _, name := range paths {
		file, err := os.Open(filepath.Join(w.Tree, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return errors.New("変更したファイルを確認できません")
		}
		buffer := make([]byte, 32768)
		tail := []byte{}
		for {
			n, readErr := file.Read(buffer)
			if n > 0 {
				data := append(tail, buffer[:n]...)
				if secretLiteral.Match(data) || (e.secret != "" && bytes.Contains(data, []byte(e.secret))) {
					file.Close()
					return errors.New("変更したファイルに資格情報らしい値があります")
				}
				if len(data) > 1024 {
					tail = append([]byte{}, data[len(data)-1024:]...)
				} else {
					tail = append([]byte{}, data...)
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				file.Close()
				return errors.New("変更したファイルを最後まで確認できません")
			}
		}
		file.Close()
	}
	return nil
}
