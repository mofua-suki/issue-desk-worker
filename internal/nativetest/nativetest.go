// Package nativetest is the contract of native (Apple) test requests shared
// by the implementation Worker, which asks for them before publishing, and
// the Apple runner, which runs them.
package nativetest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/requeststore"
)

// Target is what one native test checks: the merge of Head onto Base, whose
// tree must be Tree, on Host.
type Target struct {
	Host string `json:"host"`
	Head string `json:"head"`
	Base string `json:"base"`
	Tree string `json:"tree"`
}

// Result is the outcome the runner reports for a Target.
type Result struct {
	Status  string `json:"status"`
	Head    string `json:"head"`
	Base    string `json:"base"`
	Tree    string `json:"tree"`
	Message string `json:"message"`
}

// RequestState is a native test request as the intake reports it.
type RequestState struct {
	requeststore.Request
	Status string  `json:"status"`
	Result *Result `json:"result"`
}

// ID is the stable request ID of a target for a repository, configuration
// version and attempt.
func ID(repo string, p Target, version string, attempt ...string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + p.Host + "\x00" + p.Head + "\x00" + p.Base + "\x00" + p.Tree + "\x00" + version + "\x00" + strings.Join(attempt, "\x00")))
	return fmt.Sprintf("%d-%s", binary.BigEndian.Uint64(sum[:8])%9999999999999999+1, hex.EncodeToString(sum[8:12]))
}

// Policy confirms, from a fresh configuration snapshot, that host still runs
// native tests for repo under the configuration version c was decoded from.
// c.Host is the host asking; host is the native test host.
func Policy(ctx context.Context, api *intake.Client, c fleetconfig.Configuration, repo, host string) error {
	snapshot, err := api.Config(ctx)
	if err != nil {
		return err
	}
	if c.Version != "" && snapshot.ConfigurationVersion != c.Version {
		return errors.New("検証中に設定バージョンが変更されました")
	}
	var fleet fleetconfig.Fleet
	if json.Unmarshal(snapshot.Config, &fleet) != nil {
		return errors.New("native test設定を確認できません")
	}
	target := fleetconfig.Configuration{Fleet: fleet, Host: host, AssignmentHost: c.Host}
	repos, err := target.NativeRepositories()
	if err != nil {
		return err
	}
	for _, r := range repos {
		if r.Name == repo && r.NativeTestHost == host {
			return nil
		}
	}
	return errors.New("native test設定が変更されました")
}
