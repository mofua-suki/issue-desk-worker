package providers

import (
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/atomicfile"
	"os"
	"path/filepath"
	"strings"
)

// PrepareAntigravity retains unrelated settings and never reads or writes authentication files.
func PrepareAntigravity(file string, p Policy) error {
	if !filepath.IsAbs(file) {
		return errors.New("Antigravity設定は絶対パスにしてください")
	}
	cfg := map[string]any{}
	if raw, err := os.ReadFile(file); err == nil {
		if json.Unmarshal(raw, &cfg) != nil || cfg == nil {
			return errors.New("Antigravity設定のJSONが不正です")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("Antigravity設定を読めません")
	}
	folders := append(protected(p), filepath.Dir(file), "/data/agy")
	root := filepath.Join(p.Root, "workspaces")
	deny := []string{"RunCommand", "Bash"}
	for _, folder := range folders {
		folder = filepath.Clean(folder)
		if root == folder || strings.HasPrefix(root, folder+string(filepath.Separator)) {
			return errors.New("Antigravity保護設定が作業領域を覆っています")
		}
		for _, tool := range []string{"Read", "Edit", "Write"} {
			deny = append(deny, tool+"("+filepath.ToSlash(folder)+"/**)")
		}
	}
	cfg["permissions"] = map[string]any{"deny": deny}
	cfg["trustedWorkspaces"] = []string{root}
	cfg["allowNonWorkspaceAccess"] = false
	cfg["enableTerminalSandbox"] = true
	cfg["useG1Credits"] = false
	delete(cfg, "modelProvider")
	delete(cfg, "toolPermission")
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if os.MkdirAll(filepath.Dir(file), 0700) != nil {
		return errors.New("Antigravity設定の親を作れません")
	}
	err = atomicfile.Write(file, ".settings-", 0600, false, raw, nil)
	var stage *atomicfile.Error
	if errors.As(err, &stage) && stage.Stage == atomicfile.StageWrite {
		return errors.New("Antigravity設定を書けません")
	}
	return err
}
