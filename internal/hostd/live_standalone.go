package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/process"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var hostTokenPattern = regexp.MustCompile(`^idh_[A-Za-z0-9_-]{43}$`)

// LiveCheckOnce preserves the existing host-only config, bearer file and pending-result state.
func LiveCheckOnce(ctx context.Context, file string) error { return liveCheckOnce(ctx, file, nil) }
func liveCheckOnce(ctx context.Context, file string, transport http.RoundTripper) error {
	var c struct {
		URL        string `json:"url"`
		Host       string `json:"host"`
		TokenFile  string `json:"tokenFile"`
		StateFile  string `json:"stateFile"`
		LiveChecks []struct {
			LiveCheck
			Checkout bool `json:"checkout"`
		} `json:"liveChecks"`
	}
	if !filepath.IsAbs(file) {
		return errors.New("実地確認設定は絶対パスにしてください")
	}
	raw, err := os.ReadFile(file)
	if err != nil || len(raw) > 1<<20 {
		return cause.New("実地確認設定を読めません", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil {
		return errors.New("実地確認設定が不正です")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !hostPattern.MatchString(c.Host) || !filepath.IsAbs(c.TokenFile) || !filepath.IsAbs(c.StateFile) {
		return cause.New("実地確認の接続先・本人・資格情報/状態パスが不正です", err)
	}
	token, err := secret(c.TokenFile, hostTokenPattern)
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.StateFile)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return cause.New("実地確認状態の親を本人所有の0700にしてください", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return errors.New("実地確認状態の所有者が異なります")
	}
	fd, err := syscall.Open(c.StateFile+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return cause.New("実地確認の排他を取得できません", err)
	}
	defer syscall.Close(fd)
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("実地確認が実行中です")
	}
	checks := make([]LiveCheck, len(c.LiveChecks))
	for i, check := range c.LiveChecks {
		checks[i] = check.LiveCheck
		checks[i].checkout = check.Checkout
	}
	r := &Runtime{Config: Configuration{URL: strings.TrimRight(c.URL, "/"), Host: c.Host, Home: dir, LiveChecks: checks}, token: token, HTTP: &http.Client{Timeout: 30 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Run: process.Run, Now: time.Now, liveStateName: filepath.Base(c.StateFile)}
	return r.runLiveChecks(ctx)
}
