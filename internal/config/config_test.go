package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBootstrapStrict(t *testing.T) {
	valid := `{"serverUrl":"https://desk.example","serverTokenFile":"private/token","workerId":"x1-pro2"}`
	if _, err := Decode(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`null`, `[]`, `{}`, strings.Replace(valid, `"workerId":"x1-pro2"`, `"workerId":"a","workerId":"b"`, 1), strings.Replace(valid, `"workerId"`, `"runtimeDir":"secret","workerId"`, 1), strings.Replace(valid, "https://", "http://", 1), strings.Replace(valid, "desk.example", "u:p@desk.example", 1), strings.Replace(valid, "desk.example", "desk.example/api", 1), strings.Replace(valid, "desk.example", "desk.example?", 1), strings.Replace(valid, "desk.example", "desk.example/#", 1), valid + `{}`, strings.Replace(valid, `"x1-pro2"`, `null`, 1)} {
		if _, err := Decode(strings.NewReader(s)); err == nil {
			t.Errorf("不正設定を受理しました: %s", s)
		}
	}
}
func TestConfigTokenFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte(`{"serverUrl":"https://desk.example","serverTokenFile":"token","workerId":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	token := "idk_" + strings.Repeat("a", 43)
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if b.ServerTokenFile != tokenFile {
		t.Fatal("相対パスを設定ファイル基準で解決していません")
	}
	if got, err := b.ReadToken(); err != nil || got != token {
		t.Fatal(got, err)
	}
	if err := os.Chmod(tokenFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadToken(); err == nil {
		t.Fatal("公開されたトークンを受理しました")
	}
	if err := os.Chmod(tokenFile, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadToken(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("不正トークンが拒否されないか値が漏れています")
	}
}
func TestConfigSize(t *testing.T) {
	if _, err := Decode(bytes.NewReader(bytes.Repeat([]byte(" "), MaxConfigBytes+1))); err == nil {
		t.Fatal("設定サイズを制限していません")
	}
}
