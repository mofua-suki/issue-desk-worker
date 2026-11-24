// Package config validates the three-item bootstrap. Server policy is never stored here.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/cause"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"issue-desk-worker/internal/secretfile"
)

const MaxConfigBytes = 65536

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var tokenPattern = regexp.MustCompile(`^idk_[A-Za-z0-9_-]{43}$`)

type Bootstrap struct {
	ServerURL       string `json:"serverUrl"`
	ServerTokenFile string `json:"serverTokenFile"`
	WorkerID        string `json:"workerId"`
}

// requiredFields are the only configuration fields, and all are required.
var requiredFields = []string{"serverUrl", "serverTokenFile", "workerId"}

// clipKey keeps an unknown field name short in the error.
func clipKey(key string) string {
	if r := []rune(key); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return key
}

// Decode rejects duplicate, unknown, absent and trailing fields before reading secrets.
func Decode(r io.Reader) (Bootstrap, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil || len(data) > MaxConfigBytes {
		return Bootstrap{}, cause.New("設定ファイルを上限内で読めません", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return Bootstrap{}, cause.New("設定はJSONオブジェクトにしてください", err)
	}
	values := map[string]string{}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return Bootstrap{}, cause.New("設定のJSONが不正です", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return Bootstrap{}, errors.New("設定の項目名が不正です")
		}
		if _, exists := values[key]; exists {
			return Bootstrap{}, fmt.Errorf("設定の項目が重複しています: %s", key)
		}
		if !slices.Contains(requiredFields, key) {
			return Bootstrap{}, fmt.Errorf("設定に利用できない項目があります: %q（使えるのは %s）", clipKey(key), strings.Join(requiredFields, "・"))
		}
		var value string
		if err := dec.Decode(&value); err != nil || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return Bootstrap{}, fmt.Errorf("設定項目%sは空白のない空でない文字列にしてください", key)
		}
		values[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return Bootstrap{}, cause.New("設定のJSONが不正です", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Bootstrap{}, errors.New("設定のJSONの後に追加データがあります")
	}
	missing := []string{}
	for _, key := range requiredFields {
		if _, ok := values[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Bootstrap{}, fmt.Errorf("設定に必要な項目がありません: %s", strings.Join(missing, "・"))
	}
	b := Bootstrap{values["serverUrl"], values["serverTokenFile"], values["workerId"]}
	if err := b.Validate(); err != nil {
		return Bootstrap{}, err
	}
	return b, nil
}

func (b Bootstrap) Validate() error {
	u, err := url.Parse(b.ServerURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsRune(b.ServerURL, '#') || (u.Path != "" && u.Path != "/") {
		return cause.New("serverUrlは認証情報やパスを含まないHTTPS URLにしてください", err)
	}
	if !hostPattern.MatchString(b.WorkerID) {
		return errors.New("workerIdの形式が不正です")
	}
	if b.ServerTokenFile == "" || strings.ContainsRune(b.ServerTokenFile, 0) {
		return errors.New("serverTokenFileが必要です")
	}
	return nil
}

func Load(file string) (Bootstrap, error) {
	f, err := os.Open(file)
	if err != nil {
		return Bootstrap{}, cause.New("設定ファイルを開けません", err)
	}
	defer f.Close()
	b, err := Decode(f)
	if err != nil {
		return Bootstrap{}, err
	}
	if !filepath.IsAbs(b.ServerTokenFile) {
		b.ServerTokenFile = filepath.Join(filepath.Dir(file), b.ServerTokenFile)
	}
	return b, nil
}

func ValidToken(token string) bool { return tokenPattern.MatchString(token) }

func (b Bootstrap) ReadToken() (string, error) {
	data, err := secretfile.Read(b.ServerTokenFile, 256)
	if err != nil {
		return "", secretfile.Message("PC別トークン", err)
	}
	token := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if !tokenPattern.MatchString(token) {
		return "", errors.New("PC別トークンの形式が不正です")
	}
	return token, nil
}
