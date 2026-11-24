package execution

import (
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var uncDrivePattern = regexp.MustCompile(`^//[a-zA-Z]/`)
var slashDrivePattern = regexp.MustCompile(`^/[a-zA-Z]:/`)
var readDenialPattern = regexp.MustCompile(`(?i)^(read_file|ViewFile|Read)(?:\((.*)\)|(?:\s+denied)?)$`)
var lowerDrivePattern = regexp.MustCompile(`^[a-z]:/`)

type denialKinds struct{ Inside, Pathless int }

func comparableAGYPath(value string) string {
	s := strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(s), "file://") {
		s = s[7:]
		if decoded, err := url.PathUnescape(s); err == nil {
			s = decoded
		}
	}
	s = strings.ReplaceAll(s, "\\", "/")
	if uncDrivePattern.MatchString(s) {
		s = s[2:3] + ":" + s[3:]
	}
	if slashDrivePattern.MatchString(s) {
		s = s[1:]
	}
	s = strings.TrimSuffix(s, "/**")
	s = strings.TrimSuffix(s, "/*")
	return strings.ToLower(strings.TrimRight(s, "/"))
}
func agyCovers(root, target string) bool {
	return root != "" && (target == root || strings.HasPrefix(target, root+"/"))
}
func (e *Executor) classifyAGYDenials(denied []json.RawMessage) denialKinds {
	kinds := denialKinds{}
	root := path.Clean(comparableAGYPath(filepath.Join(e.Config.RuntimeDir, "workspaces")))
	protected := []string{filepath.Dir(e.Config.WorkerTokenFile), filepath.Join(e.Config.RuntimeDir, "state"), filepath.Join(e.Config.RuntimeDir, "control"), "/data/agy"}
	if e.Config.Antigravity != nil {
		p := filepath.Dir(e.Config.Antigravity.SettingsFile)
		protected = append(protected, p)
		if filepath.Base(p) == "antigravity-cli" {
			protected = append(protected, filepath.Dir(p))
		}
	}
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR"} {
		if v := os.Getenv(key); v != "" {
			protected = append(protected, v)
		}
	}
	for _, raw := range denied {
		var entry map[string]any
		if json.Unmarshal(raw, &entry) != nil {
			var text string
			if json.Unmarshal(raw, &text) != nil {
				continue
			}
			if json.Unmarshal([]byte(text), &entry) != nil {
				m := readDenialPattern.FindStringSubmatch(strings.TrimSpace(text))
				if m == nil {
					continue
				}
				entry = map[string]any{"tool": m[1]}
				if m[2] != "" {
					entry["path"] = m[2]
				}
			}
		}
		read, command := false, false
		for _, key := range []string{"action", "tool", "display_name", "tool_name", "name"} {
			v, _ := entry[key].(string)
			if strings.EqualFold(v, "RunCommand") || strings.EqualFold(v, "Bash") {
				command = true
			}
			if strings.EqualFold(v, "read_file") || strings.EqualFold(v, "Read") || strings.EqualFold(v, "ViewFile") {
				read = true
			}
		}
		if !read || command {
			continue
		}
		var value any
		for _, key := range []string{"path", "file_path", "filePath", "uri"} {
			if x, ok := entry[key]; ok && x != nil {
				value = x
				break
			}
		}
		if value == nil {
			for _, parent := range []string{"arguments", "input"} {
				nested, _ := entry[parent].(map[string]any)
				for _, key := range []string{"path", "file_path"} {
					if x, ok := nested[key]; ok && x != nil {
						value = x
						break
					}
				}
				if value != nil {
					break
				}
			}
		}
		if value == nil || value == "" {
			kinds.Pathless++
			continue
		}
		file, ok := value.(string)
		if !ok {
			continue
		}
		file = comparableAGYPath(file)
		if file == "" {
			kinds.Pathless++
			continue
		}
		target := file
		if !strings.HasPrefix(file, "/") && !lowerDrivePattern.MatchString(file) {
			target = root + "/" + file
		}
		target = path.Clean(target)
		inside := agyCovers(root, target)
		for _, folder := range protected {
			if agyCovers(path.Clean(comparableAGYPath(folder)), target) {
				inside = false
				break
			}
		}
		if inside {
			kinds.Inside++
		}
	}
	return kinds
}
