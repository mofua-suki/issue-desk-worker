package hostd

import (
	"encoding/json"
	"errors"
	"io"
	"issue-desk-worker/internal/cause"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var hostPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var tagPattern = regexp.MustCompile(`^[0-9a-f]{12}(-client-python)?$`)
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type Configuration struct {
	URL          string      `json:"url"`
	Host         string      `json:"host"`
	ClientDir    string      `json:"clientDir"`
	WaitMinutes  int         `json:"waitMinutes"`
	ReadySeconds int         `json:"readySeconds"`
	Apps         []App       `json:"apps"`
	Readers      []Reader    `json:"readers"`
	LiveChecks   []LiveCheck `json:"liveChecks"`
	Home         string      `json:"-"`
}

func Load(file string) (Configuration, error) {
	var c Configuration
	if !filepath.IsAbs(file) {
		return c, errors.New("hostd設定は絶対パスで指定してください")
	}
	f, err := os.Open(file)
	if err != nil {
		return c, cause.New("hostd設定を読めません", err)
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil {
		return c, errors.New("hostd設定が不正です")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return c, errors.New("hostd設定の末尾が不正です")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") || !hostPattern.MatchString(c.Host) || !filepath.IsAbs(c.ClientDir) {
		return c, cause.New("hostdの接続先・ホスト・配置先が不正です", err)
	}
	c.URL = strings.TrimRight(c.URL, "/")
	c.Home = filepath.Dir(file)
	if c.WaitMinutes == 0 {
		c.WaitMinutes = 120
	}
	if c.ReadySeconds == 0 {
		c.ReadySeconds = 180
	}
	if c.WaitMinutes < 1 || c.WaitMinutes > 525600 || c.ReadySeconds < 1 || c.ReadySeconds > 3600 {
		return c, errors.New("hostdの待機時間が不正です")
	}
	return c, nil
}
func secret(file string, pattern *regexp.Regexp) (string, error) {
	fd, err := syscall.Open(file, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", cause.New("hostd資格情報を開けません", err)
	}
	f := os.NewFile(uintptr(fd), file)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", cause.New("hostd資格情報は0600の通常ファイルにしてください", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return "", errors.New("hostd資格情報の所有者が異なります")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(raw) > 8192 {
		return "", cause.New("hostd資格情報を読めません", err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" || pattern != nil && !pattern.MatchString(value) {
		return "", errors.New("hostd資格情報の形式が不正です")
	}
	return value, nil
}
