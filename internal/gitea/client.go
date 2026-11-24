package gitea

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/apipath"
	"issue-desk-worker/internal/cause"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"issue-desk-worker/internal/secretfile"
)

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var safeToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type Client struct {
	Base  string
	token string
	http  *http.Client
}

// Error はGiteaの拒否応答。Messageは応答本文から得た理由で、資格情報を伏せていない。
// 外へ出す側が伏せられるよう、Error()の文面には含めない。
type Error struct {
	Status  int
	Message string
	// Call is the method and path template of the refused request, without
	// numbers or the query, so it names the call without carrying its values.
	Call string
}

// CauseKind names the refusal without the response body.
func (e *Error) CauseKind() string { return e.Error() }

func (e *Error) Error() string {
	if e.Call == "" {
		return fmt.Sprintf("Gitea APIがHTTP %dを返しました", e.Status)
	}
	return fmt.Sprintf("Gitea APIがHTTP %dを返しました（%s）", e.Status, e.Call)
}

// UnreachableError is a request that did not get a complete response. It is
// ErrUnreachable for callers that only ask whether a retry may succeed.
type UnreachableError struct {
	Call string
	Kind string
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("Giteaへ接続できません（%s、%s）", e.Kind, e.Call)
}

func (e *UnreachableError) Is(target error) bool { return target == ErrUnreachable }

// CauseKind names the transport failure without its addresses.
func (e *UnreachableError) CauseKind() string { return e.Error() }

// unreachableKind names a transport failure without its addresses or values.
func unreachableKind(err error) string {
	var dns *net.DNSError
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return "時間切れ"
	case errors.As(err, &dns):
		return "名前を解決できません"
	case errors.As(err, &certificate) || errors.As(err, &record):
		return "TLSの確認に失敗しました"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "接続を拒否されました"
	case errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		return "接続が途中で切れました"
	}
	return "通信に失敗しました"
}

type Repository struct {
	DefaultBranch string `json:"default_branch"`
	Empty         bool   `json:"empty"`
	Permissions   struct {
		Push bool `json:"push"`
	} `json:"permissions"`
}
type Issue struct {
	Number      int64           `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	State       string          `json:"state"`
	PullRequest json.RawMessage `json:"pull_request"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}
type Pull struct {
	Body           string `json:"body"`
	Draft          bool   `json:"draft"`
	Mergeable      bool   `json:"mergeable"`
	Number         int64  `json:"number"`
	URL            string `json:"html_url"`
	Merged         bool   `json:"merged"`
	State          string `json:"state"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
}

func ReadToken(file string) (string, error) {
	data, err := secretfile.Read(file, 1024)
	if err != nil {
		return "", secretfile.Message("監視用Giteaトークン", err)
	}
	token := strings.TrimSpace(string(data))
	if !safeToken.MatchString(token) {
		return "", errors.New("監視用Giteaトークンの形式が不正です")
	}
	return token, nil
}
func New(base, token string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, cause.New("Giteaの接続先が不正です", err)
	}
	if !safeToken.MatchString(token) {
		return nil, errors.New("Giteaトークンの形式が不正です")
	}
	return &Client{Base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// ErrUnreachable is a connection or body-read failure; a later attempt may succeed.
var ErrUnreachable = errors.New("Giteaへ接続できません")

func (c *Client) Request(ctx context.Context, method, path string, body, out any) error {
	_, err := c.do(ctx, method, path, body, out)
	return err
}

// do sends one API request and returns the response header with the decoded body.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+"/api/v1"+path, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("対象Giteaへの要求を作れません（%s %s）", method, pathTemplate(path))
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	call := method + " " + pathTemplate(path)
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &UnreachableError{Call: call, Kind: unreachableKind(err)}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		return res.Header, &Error{Status: res.StatusCode, Message: refusalMessage(raw), Call: call}
	}
	if out == nil || res.StatusCode == 204 {
		return res.Header, nil
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return res.Header, &UnreachableError{Call: call, Kind: "応答の本文を読めません（" + unreachableKind(err) + "）"}
	}
	if len(raw) > maxResponseBytes {
		return res.Header, fmt.Errorf("Giteaの応答が上限（8MiB）を超えました（%s %s）", method, pathTemplate(path))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return res.Header, fmt.Errorf("GiteaのJSON応答が不正です（%s %s）", method, pathTemplate(path))
	}
	return res.Header, nil
}

// maxResponseBytes bounds a successful Gitea response body.
const maxResponseBytes = 8 << 20

// pathTemplate names the API endpoint without the query or the numbers in it.
func pathTemplate(path string) string { return apipath.Template(path) }

// refusalMessage は拒否応答の本文から理由を取り出す。JSONのmessageが無ければ本文をそのまま返す。
func refusalMessage(raw []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &body) == nil {
		if message := strings.TrimSpace(body.Message); message != "" {
			return message
		}
	}
	return strings.TrimSpace(string(raw))
}
func ValidRepo(repo string) bool { return repoPattern.MatchString(repo) }
func (c *Client) Repo(ctx context.Context, repo string) (Repository, error) {
	var out Repository
	if !ValidRepo(repo) {
		return out, errors.New("リポジトリ名が不正です")
	}
	err := c.Request(ctx, "GET", "/repos/"+repo, nil, &out)
	return out, err
}
func (c *Client) Issue(ctx context.Context, repo string, number int64) (Issue, error) {
	var out Issue
	if !ValidRepo(repo) || number < 1 {
		return out, errors.New("Issueの指定が不正です")
	}
	err := c.Request(ctx, "GET", fmt.Sprintf("/repos/%s/issues/%d", repo, number), nil, &out)
	return out, err
}
func (c *Client) Pull(ctx context.Context, repo string, number int64) (Pull, error) {
	var out Pull
	err := c.Request(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &out)
	return out, err
}
func (c *Client) CreatePull(ctx context.Context, repo, branch, base, title, body string) (Pull, error) {
	var out Pull
	err := c.Request(ctx, "POST", "/repos/"+repo+"/pulls", map[string]any{"head": branch, "base": base, "title": title, "body": body}, &out)
	return out, err
}
func (c *Client) Comment(ctx context.Context, repo string, issue int64, body string) error {
	return c.Request(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", repo, issue), map[string]string{"body": body}, nil)
}
func (c *Client) Merge(ctx context.Context, repo string, number int64, head string) error {
	return c.Request(ctx, "POST", fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number), map[string]any{"do": "fast-forward-only", "head_commit_id": head, "delete_branch_after_merge": false, "merge_when_checks_succeed": false}, nil)
}
