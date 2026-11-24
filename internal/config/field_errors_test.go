package config

import (
	"strings"
	"testing"
)

// 設定の誤りは、足りない項目・使えない項目・空の項目の名前を示す。
func TestDecodeNamesFields(t *testing.T) {
	for in, want := range map[string]string{
		`{"serverUrl":"https://desk.example"}`:                                                    "serverTokenFile・workerId",
		`{"serverUrl":"https://desk.example","serverTokenFile":"/t","workerId":"pc","extra":"x"}`: `"extra"`,
		`{"serverUrl":"https://desk.example","serverTokenFile":" ","workerId":"pc"}`:              "serverTokenFile",
	} {
		if _, err := Decode(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", in, err)
		}
	}
}

// 設定ファイルを開けないときは、その種類を示す。
func TestLoadNamesOpenFailure(t *testing.T) {
	_, err := Load(t.TempDir() + "/none.json")
	if err == nil || !strings.Contains(err.Error(), "見つかりません") {
		t.Fatalf("開けない理由がありません: %v", err)
	}
}
