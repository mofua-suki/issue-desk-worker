package providers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyAntigravityReplyContract(t *testing.T) {
	for _, name := range []string{"plain", "fenced", "metadata"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tree := filepath.Join(root, "runtime", "workspaces", "job")
			if e := os.MkdirAll(tree, 0700); e != nil {
				t.Fatal(e)
			}
			policy := Policy{Root: filepath.Join(root, "runtime"), Tree: tree, TokenFile: filepath.Join(root, "private", "token")}
			settings := filepath.Join(root, "agy", "settings.json")
			if e := PrepareAntigravity(settings, policy); e != nil {
				t.Fatal(e)
			}
			fixture, e := filepath.Abs("testdata/antigravity/" + name + ".json")
			if e != nil {
				t.Fatal(e)
			}
			exe := filepath.Join(root, "fake-agy")
			if e := os.WriteFile(exe, []byte("#!/bin/sh\nexec /bin/cat '"+fixture+"'\n"), 0700); e != nil {
				t.Fatal(e)
			}
			a, e := Call(context.Background(), Request{Settings: Settings{Provider: "antigravity", Model: "fixture"}, Policy: policy, Antigravity: exe, AntigravitySettings: settings, Timeout: time.Second})
			if e != nil {
				t.Fatal("旧標準応答を拒否しました", e)
			}
			if a.Usage.Input == nil || *a.Usage.Input != 1200 || a.Usage.Output == nil || *a.Usage.Output != 340 || a.Usage.Cached == nil || *a.Usage.Cached != 800 {
				t.Fatal("旧usageを失いました", a.Usage)
			}
			if a.Session != "conv-1" {
				t.Fatal(a.Session)
			}
		})
	}
}
func TestAntigravityMetadataAndResultBoundaries(t *testing.T) {
	root := antigravityOutput("diagnostic\n[{\"conversation\":\"conv-2\",\"deniedActions\":[{\"tool\":\"Read\",\"path\":\"/work/a\"}],\"text\":\"before ```json\\n{\\\"status\\\":\\\"question\\\",\\\"summary\\\":\\\"help\\\"}\\n```\"}]")
	var answer Answer
	antigravityMetadata(root, &answer)
	if answer.Session != "conv-2" || len(answer.Denied) != 1 {
		t.Fatalf("%+v", answer)
	}
	result := antigravityResult(root)
	var obj map[string]any
	if json.Unmarshal(result, &obj) != nil || obj["status"] != "question" {
		t.Fatalf("%s", result)
	}
	root = antigravityOutput(`{"result":"quoted {\"status\":\"completed\",\"summary\":\"ok\",\"unknownCredential\":\"do-not-save\"}"}`)
	result = antigravityResult(root)
	if json.Unmarshal(result, &obj) != nil {
		t.Fatal(string(result))
	}
	if _, found := obj["unknownCredential"]; found {
		t.Fatal("正本に無い項目を残しました")
	}
}
