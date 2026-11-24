package providers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const emptyAssumptionsReport = `{"status":"completed","summary":"report","incomplete":null,"prerequisiteMissing":null,"questions":[],"tests":[],"unverified":[],"unverifiedKinds":[],"followupIssues":[],"selfCheck":[],"prevention":{"kind":"test","detail":"regression"},"assumptions":[],"similar":{"searched":"paths","found":[],"handled":"paths","guard":"tests"},"acceptanceTransfers":[]}`

// Real driver invocation with a CLI stand-in inspects actual argv and schema files.
// The stand-in does not claim to prove OS sandbox enforcement by a deployed CLI.
func TestReportOnlyDriverUsesImplementationContractAndReadOnlyResume(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			tree := filepath.Join(root, "workspaces", "job")
			_ = os.MkdirAll(tree, 0700)
			argsFile := filepath.Join(root, "args")
			schemaFile := filepath.Join(root, "schema")
			answerFile := filepath.Join(root, "fixture")
			if err := os.WriteFile(answerFile, []byte(emptyAssumptionsReport), 0600); err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "' || exit 1\n"
			if provider == "codex" {
				script += "while [ \"$#\" -gt 0 ]; do case \"$1\" in --output-schema) shift; /bin/cp \"$1\" '" + schemaFile + "' || exit 1;; -o) shift; /bin/cp '" + answerFile + "' \"$1\" || exit 1;; esac; shift; done\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"same-session\"}'\n"
			} else {
				event, _ := json.Marshal(map[string]any{"type": "result", "structured_output": json.RawMessage(emptyAssumptionsReport)})
				script += "printf '%s\\n' '" + string(event) + "'\n"
			}
			exe := filepath.Join(root, "cli")
			if err := os.WriteFile(exe, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			policy := Policy{Root: root, Tree: tree, TokenFile: filepath.Join(root, "private", "token"), ReadOnly: true, Temp: filepath.Join(root, "temp")}
			callbacks := 0
			a, err := Call(context.Background(), Request{Settings: Settings{Provider: provider, Model: "fixture", Effort: "medium"}, Policy: policy, Codex: exe, Claude: exe, Session: "same-session", ReportOnly: true, Timeout: time.Second, OnSession: func(id string) error {
				callbacks++
				if id != "same-session" {
					t.Fatal(id)
				}
				return nil
			}})
			if err != nil || a.Session != "same-session" || Validate(a.JSON, false) != nil {
				t.Fatal(a, err)
			}
			raw, _ := os.ReadFile(argsFile)
			args := strings.Split(strings.TrimSpace(string(raw)), "\n")
			joined := string(raw)
			if provider == "codex" {
				for _, expected := range []string{"resume\nsame-session\n-", `"extends"=":read-only"`, `"` + tree + `"="read"`, `"` + filepath.Join(root, "state") + `"="deny"`, `"` + filepath.Join(root, "control") + `"="deny"`, `"` + filepath.Join(root, "private") + `"="deny"`} {
					if !strings.Contains(joined, expected) {
						t.Error("missing policy/resume", expected)
					}
				}
				if callbacks != 1 {
					t.Fatal("resume callback not checked")
				}
				raw, _ = os.ReadFile(schemaFile)
			} else {
				for _, expected := range []string{"--resume\nsame-session", "--tools\nRead,Glob,Grep", "--restricted", "--permission-mode\ndontAsk"} {
					if !strings.Contains(joined, expected) {
						t.Error("missing Claude constraint", expected)
					}
				}
				if strings.Contains(joined, "--no-session-persistence") || strings.Contains(joined, "--session-id") {
					t.Fatal("new/review conversation")
				}
				for i, arg := range args {
					if arg == "--json-schema" {
						raw = []byte(args[i+1])
					}
				}
				var settings map[string]any
				_ = json.Unmarshal([]byte(policy.ClaudeSettings()), &settings)
				denied := settings["permissions"].(map[string]any)["deny"].([]any)
				for _, tool := range []string{"Edit", "Write", "NotebookEdit", "Bash", "Task", "Agent", "mcp__*"} {
					found := false
					for _, value := range denied {
						found = found || value == tool
					}
					if !found {
						t.Error("missing deny", tool)
					}
				}
			}
			var schema map[string]any
			if json.Unmarshal(raw, &schema) != nil {
				t.Fatal(string(raw))
			}
			if schema["properties"].(map[string]any)["assumptions"].(map[string]any)["minItems"] != float64(1) {
				t.Fatal("not implementation schema")
			}
			if _, exists := schema["properties"].(map[string]any)["decision"]; exists {
				t.Fatal("review schema used")
			}

			fresh := Request{Settings: Settings{Provider: provider, Model: "fixture", Effort: "medium"}, Policy: policy, Codex: exe, Claude: exe, Timeout: time.Second}
			if _, err := Call(t.Context(), fresh); err != nil {
				t.Fatal(err)
			}
			raw, err = os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			joined = string(raw)
			if strings.Contains(joined, "\nresume\n") || strings.Contains(joined, "\n--resume\n") || strings.Contains(joined, "\n--session-id\n") {
				t.Fatal("新規報告が既存会話を使いました", joined)
			}
			if provider == "claude" && !strings.Contains(joined, "--no-session-persistence\n") {
				t.Fatal("Claudeが新規報告会話を保存します")
			}
		})
	}
}

func TestReportOnlyDriverRejectsUnsafeRequestsBeforeCLI(t *testing.T) {
	for _, kind := range []string{"antigravity", "session", "invalid-session", "write", "review"} {
		t.Run(kind, func(t *testing.T) {
			r := Request{Settings: Settings{Provider: "codex"}, Policy: Policy{ReadOnly: true}, Session: "same", ReportOnly: true, Codex: "/does-not-exist"}
			switch kind {
			case "antigravity":
				r.Settings.Provider = kind
				r.ReportOnly = false
				r.Session = ""
				r.Antigravity = "/does-not-exist"
			case "session":
				r.Session = ""
			case "invalid-session":
				r.Session = "../escape"
			case "write":
				r.Policy.ReadOnly = false
			case "review":
				r.Review = true
			}
			_, err := Call(context.Background(), r)
			if err == nil || strings.Contains(err.Error(), "does-not-exist") {
				t.Fatal("unsafe request reached CLI", err)
			}
		})
	}
}

func TestReportValidationRulesRemainStrict(t *testing.T) {
	for _, value := range []string{"", "なし", "無し", "特になし", "該当なし", "n/a", "none", "-", " NONE "} {
		if concrete(value) {
			t.Errorf("accepted %q", value)
		}
	}
	if err := Validate([]byte(emptyAssumptionsReport), false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReport([]byte(emptyAssumptionsReport), true); err == nil {
		t.Fatal("empty assumptions accepted")
	}
	for _, status := range []string{"question", "failed"} {
		raw := []byte(strings.Replace(emptyAssumptionsReport, `"completed"`, `"`+status+`"`, 1))
		if err := Validate(raw, false); err != nil {
			t.Fatal(err)
		}
		if err := ValidateReport(raw, true); err != nil {
			t.Fatal("non-completed rejected", err)
		}
	}
}

func TestReportOnlyRejectsReturnedNewConversation(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			tree := filepath.Join(root, "tree")
			_ = os.MkdirAll(tree, 0700)
			event := `{"type":"thread.started","thread_id":"new-session"}`
			if provider == "claude" {
				event = `{"type":"system","session_id":"new-session"}`
			}
			script := "#!/bin/sh\nprintf '%s\\n' '" + event + "'\n"
			exe := filepath.Join(root, "cli")
			_ = os.WriteFile(exe, []byte(script), 0700)
			_, err := Call(context.Background(), Request{Settings: Settings{Provider: provider}, Policy: Policy{Root: root, Tree: tree, ReadOnly: true}, Codex: exe, Claude: exe, Session: "original", ReportOnly: true, Timeout: time.Second})
			if err == nil || !strings.Contains(err.Error(), "会話ID") {
				t.Fatal(err)
			}
		})
	}
}

func TestReportValidationRequiredDetailsAndRemediationAreUnchanged(t *testing.T) {
	var result map[string]any
	_ = json.Unmarshal([]byte(emptyAssumptionsReport), &result)
	result["assumptions"] = []any{map[string]any{"assumption": "fixture scope", "evidence": "fixture record", "verified": true}}
	result["selfCheck"] = []any{map[string]any{"criterion": "fixture acceptance", "source": "issue", "location": "fixture.go:1", "test": "fixture regression", "production": map[string]any{"origin": "fixture input", "stored": "fixture row", "caller": "fixture call", "outlet": "fixture output"}, "similarLocations": []any{map[string]any{"location": "fixture.go:1", "result": "paths compared"}}, "testChanges": map[string]any{"changed": false, "details": "existing expectations unchanged"}}}
	data, _ := json.Marshal(result)
	if err := ValidateReport(data, true); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"assumption", "evidence", "criterion", "location", "source", "test", "origin", "stored", "caller", "outlet", "details", "searched", "handled", "guard", "detail"} {
		t.Run(field, func(t *testing.T) {
			var changed map[string]any
			_ = json.Unmarshal(data, &changed)
			check := changed["selfCheck"].([]any)[0].(map[string]any)
			switch field {
			case "assumption", "evidence":
				changed["assumptions"].([]any)[0].(map[string]any)[field] = "none"
			case "origin", "stored", "caller", "outlet":
				check["production"].(map[string]any)[field] = "none"
			case "details":
				check["testChanges"].(map[string]any)[field] = "none"
			case "searched", "handled", "guard":
				changed["similar"].(map[string]any)[field] = "none"
			case "detail":
				changed["prevention"].(map[string]any)[field] = "none"
			default:
				check[field] = "none"
			}
			raw, _ := json.Marshal(changed)
			if err := ValidateReport(raw, true); err == nil {
				t.Fatal("required detail relaxed", field)
			}
		})
	}
}
