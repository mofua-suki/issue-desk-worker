package testcause

import (
	"bytes"
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

var sha40Pattern = regexp.MustCompile(`^[a-f0-9]{40}$`)
var syscallNamePattern = regexp.MustCompile(`^[A-Za-z_]{1,100}$`)
var controlOrQuotePattern = regexp.MustCompile(`[\x00-\x1f']`)

type Object = map[string]any

func obj(value any) Object { m, _ := value.(map[string]any); return m }
func arr(value any) []any  { a, _ := value.([]any); return a }
func str(value any) string { s, _ := value.(string); return s }
func known(value any) (float64, bool) {
	n, ok := value.(float64)
	return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func whole(value any) bool {
	n, ok := known(value)
	return ok && n >= 0 && n == math.Trunc(n) && n <= 9007199254740991
}
func length(s string) int { return len(utf16.Encode([]rune(s))) }

var sha = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var hash = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validAt(value any) bool { _, e := time.Parse(time.RFC3339Nano, str(value)); return e == nil }
func encoding(value any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(value)
	s := strings.TrimSuffix(b.String(), "\n")
	s = strings.ReplaceAll(strings.ReplaceAll(s, `\u2028`, "\u2028"), `\u2029`, "\u2029")
	return []byte(s)
}
func copyFields(value Object, keys ...string) Object {
	out := Object{}
	for _, key := range keys {
		out[key] = value[key]
	}
	return out
}
func cleanEvidence(value any) Object {
	v := obj(value)
	empty := Object{"version": float64(1), "names": []any{}, "total": nil, "complete": false, "timeout": nil}
	for _, key := range []string{"version", "names", "total", "complete", "timeout"} {
		if _, has := v[key]; !has {
			return empty
		}
	}
	names, ok := v["names"].([]any)
	complete, boolean := v["complete"].(bool)
	total := v["total"]
	timeout := obj(v["timeout"])
	if v["version"] != float64(1) || !ok || len(names) > 200 || !boolean || total != nil && !whole(total) || len(encoding(value)) > 16000 {
		return empty
	}
	for _, name := range names {
		if _, ok := name.(string); !ok || str(name) == "" || length(str(name)) > 1000 {
			return empty
		}
	}
	if complete && (total != float64(len(names)) || len(names) == 0 || v["timeout"] != nil) {
		return empty
	}
	if v["timeout"] != nil {
		_, ok := timeout["complete"].(bool)
		if !ok || !whole(timeout["seconds"]) || timeout["seconds"] == float64(0) || timeout["complete"] == true && (len(names) > 0 || total != nil && total != float64(0)) {
			return empty
		}
	}
	out := Object{"version": float64(1), "names": names, "total": total, "complete": complete, "timeout": nil}
	if timeout != nil {
		out["timeout"] = copyFields(timeout, "seconds", "complete")
	}
	return out
}
func validRun(v Object) bool {
	for _, key := range []string{"code", "truncated", "killed"} {
		if _, has := v[key]; !has {
			return false
		}
	}
	return true
}
func cleanFailure(value any, observation Object) Object {
	v := obj(value)
	run := obj(v["run"])
	evidence := cleanEvidence(v["evidence"])
	if !validRun(run) || observation["passed"] == true || observation["environmentFailure"] != nil || observation["conditionsComplete"] != true || run["code"] != float64(1) || run["truncated"] != false || run["killed"] != nil || run["environment"] == true || evidence["complete"] != true || len(arr(evidence["names"])) != len(arr(observation["failures"])) {
		return nil
	}
	for i, name := range arr(evidence["names"]) {
		if name != obj(arr(observation["failures"])[i])["test"] {
			return nil
		}
	}
	return Object{"evidence": evidence, "run": Object{"code": float64(1), "truncated": false, "killed": nil}}
}
func validMain(value Object) bool {
	start, e1 := time.Parse(time.RFC3339Nano, str(value["startedAt"]))
	end, e2 := time.Parse(time.RFC3339Nano, str(value["finishedAt"]))
	files, ok := value["failedFiles"].([]any)
	failures := obj(value["failures"])
	names, namesOK := failures["names"].([]any)
	_, completeOK := failures["complete"].(bool)
	ms, msOK := known(value["ms"])
	if (value["status"] != "passed" && value["status"] != "failed") || !sha40Pattern.MatchString(str(value["sha"])) || e1 != nil || e2 != nil || end.Before(start) || !whole(value["files"]) || !msOK || ms < 0 || !ok || len(files) > 200 || !namesOK || len(names) > 200 || !completeOK {
		return false
	}
	if _, ok := value["excerpt"].(string); !ok || length(str(value["excerpt"])) > 3000 {
		return false
	}
	for _, file := range files {
		if _, ok := file.(string); !ok {
			return false
		}
	}
	for _, name := range names {
		if _, ok := name.(string); !ok || length(str(name)) > 1000 {
			return false
		}
	}
	return value["status"] != "passed" || len(files) == 0 && len(names) == 0 && failures["complete"] == true
}
func identityMatches(value, observation Object) bool {
	for _, key := range []string{"sha", "baseSha", "command", "environment", "at", "source"} {
		if value[key] != observation[key] {
			return false
		}
	}
	return true
}
func cleanPass(value any, observation Object) Object {
	v := obj(value)
	record := obj(v["record"])
	run := obj(v["run"])
	execution := obj(record["execution"])
	if !validRun(run) || observation["passed"] != true || observation["conditionsComplete"] != true || len(arr(observation["failures"])) > 0 || !sha.MatchString(str(observation["sha"])) || !sha.MatchString(str(observation["baseSha"])) || (observation["source"] != "full-test" && observation["source"] != "fixed-test") || !validMain(record) || record["status"] != "passed" || record["files"] == float64(0) || record["sha"] != observation["sha"] || record["finishedAt"] != observation["at"] || !hash.MatchString(str(execution["command"])) || !hash.MatchString(str(execution["environment"])) || execution["command"] != observation["command"] || execution["environment"] != observation["environment"] || execution["baseSha"] != observation["baseSha"] || run["code"] != float64(0) || run["truncated"] != false || run["killed"] != nil || run["environment"] == true || !identityMatches(v, observation) {
		return nil
	}
	clean := copyFields(record, "status", "sha", "startedAt", "finishedAt", "files", "ms")
	clean["failedFiles"], clean["excerpt"], clean["failures"], clean["execution"] = []any{}, record["excerpt"], Object{"names": []any{}, "complete": true}, copyFields(execution, "command", "environment", "baseSha")
	out := copyFields(observation, "sha", "baseSha", "command", "environment", "at", "source")
	out["record"], out["run"] = clean, Object{"code": float64(0), "truncated": false, "killed": nil}
	return out
}
func cleanPermission(value any, observation Object) Object {
	v := obj(value)
	run := obj(v["run"])
	path, tree := str(v["path"]), str(v["tree"])
	if !validRun(run) || observation["conditionsComplete"] != true || observation["passed"] == true || len(arr(observation["failures"])) > 0 || !sha.MatchString(str(observation["sha"])) || !sha.MatchString(str(observation["baseSha"])) || !validAt(observation["at"]) || (observation["source"] != "fixed-test" && observation["source"] != "full-test") || run["code"] != float64(1) || run["truncated"] != false || run["killed"] != nil || (v["code"] != "EPERM" && v["code"] != "EACCES") || !syscallNamePattern.MatchString(str(v["syscall"])) || !filepath.IsAbs(path) || !filepath.IsAbs(tree) || length(path) > 1000 || length(tree) > 1000 || controlOrQuotePattern.MatchString(path+tree) || !identityMatches(v, observation) {
		return nil
	}
	relative, e := filepath.Rel(filepath.Clean(tree), filepath.Clean(path))
	if e != nil || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		return nil
	}
	out := copyFields(v, "code", "syscall", "path", "tree", "sha", "baseSha", "command", "environment", "at", "source")
	out["run"] = Object{"code": float64(1), "truncated": false, "killed": nil}
	return out
}
func cleanObservation(value any) Object {
	v := obj(value)
	if v == nil {
		return nil
	}
	for _, key := range []string{"sha", "baseSha", "at", "source", "command", "environment", "conditionsComplete", "complete", "passed", "failures", "environmentFailure"} {
		if _, has := v[key]; !has {
			return nil
		}
	}
	for _, key := range []string{"sha", "baseSha"} {
		if v[key] != nil && !sha.MatchString(str(v[key])) {
			return nil
		}
	}
	for _, key := range []string{"command", "environment"} {
		if v[key] != nil && !hash.MatchString(str(v[key])) {
			return nil
		}
	}
	for _, key := range []string{"conditionsComplete", "complete", "passed"} {
		if _, ok := v[key].(bool); !ok {
			return nil
		}
	}
	failures, ok := v["failures"].([]any)
	if !validAt(v["at"]) || (v["source"] != "fixed-test" && v["source"] != "full-test") || v["conditionsComplete"] == true && (!hash.MatchString(str(v["command"])) || !hash.MatchString(str(v["environment"]))) || !ok || len(failures) > 200 || (v["environmentFailure"] != nil && v["environmentFailure"] != "permission") || v["environmentFailure"] != nil && (v["passed"] == true || len(failures) > 0) || v["passed"] == true && len(failures) > 0 {
		return nil
	}
	seen := map[string]bool{}
	cleanFailures := []any{}
	for _, raw := range failures {
		f := obj(raw)
		name := str(f["test"])
		if name == "" || length(name) > 1000 || !hash.MatchString(str(f["fingerprint"])) || seen[name] {
			return nil
		}
		seen[name] = true
		cleanFailures = append(cleanFailures, copyFields(f, "test", "fingerprint"))
	}
	var permission Object
	if v["environmentFailure"] == "permission" {
		permission = cleanPermission(v["permissionEvidence"], v)
	}
	pass, failure := cleanPass(v["passEvidence"], v), cleanFailure(v["failureEvidence"], v)
	out := copyFields(v, "sha", "baseSha", "at", "source", "command", "environment", "conditionsComplete", "passed")
	out["complete"] = v["complete"] == true && v["conditionsComplete"] == true && (v["environmentFailure"] == nil || permission != nil) && ((v["passed"] == true && pass != nil) || (v["passed"] != true && (permission != nil || failure != nil)))
	out["environmentFailure"], out["permissionEvidence"], out["passEvidence"], out["failureEvidence"], out["failures"] = nil, permission, pass, failure, cleanFailures
	if permission != nil {
		out["environmentFailure"] = "permission"
	}
	return out
}
