package readiness

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHostChecksWireContract(t *testing.T) {
	r := Report{At: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), SettingsVersion: "settings-17"}
	if r.Value()["ok"] != false {
		t.Fatal("未検査を成功扱いしました")
	}
	r.Record("server", true)
	r.Record("isolation", true)
	raw, _ := json.Marshal(r.Value())
	var decoded struct {
		At              string
		OK              bool
		Items           []Item
		SettingsVersion string
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.OK || len(decoded.Items) != 2 || decoded.At != "2026-11-01T12:00:00Z" || decoded.SettingsVersion != "settings-17" {
		t.Fatalf("%s", raw)
	}
	r.Record("giteaScopes", false)
	raw, _ = json.Marshal(r.Value())
	if r.Value()["ok"] != false || !strings.Contains(string(raw), `"name":"giteaScopes"`) {
		t.Fatalf("失敗が正本にありません: %s", raw)
	}
}
