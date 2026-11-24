// Package requeststore keeps the progress of intake requests (merge, PR
// creation, full test, native test) on disk, one file per request ID, so a
// request resumes from its saved stage after a restart and a lost result is
// sent again from the same saved result.
package requeststore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/intake"
	"os"
	"path/filepath"

	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/gitwork"
)

// Request is a request as the intake hands it out.
type Request struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Repo    string          `json:"repo"`
	Payload json.RawMessage `json:"payload"`
}

// StartFullTest reserves a repository while run executes and saves its result.
// The returned delivery is performed serially by the scheduler.
type StartFullTest = func(repo string, run func(context.Context) (send func(context.Context) error, err error)) bool

// Progress is the saved state of one request.
type Progress struct {
	NativeVersion string  `json:"nativeConfigurationVersion,omitempty"`
	Request       Request `json:"request"`
	Stage         string  `json:"stage"`
	Branch        string  `json:"branch,omitempty"`
	Base          string  `json:"base,omitempty"`
	Head          string  `json:"head,omitempty"`
	// MergeError はマージ要求の失敗理由（資格情報を伏せたもの）。読み直しに失敗して再開しても理由を失わないよう保存する。
	MergeError string `json:"mergeError,omitempty"`
	// ApprovalRecorded は承認の記録をPRへ投稿済みであることを表す。再開で二重に投稿しない。
	ApprovalRecorded bool            `json:"approvalRecorded,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
}

// Store is the directory of saved requests under a runtime directory.
type Store struct{ Dir string }

// Under is the store of the runtime directory root.
func Under(root string) Store { return Store{Dir: filepath.Join(root, "state", "requests")} }

// File is where the request id is saved.
func (s Store) File(id string) string { return filepath.Join(s.Dir, id+".json") }

// Save replaces the saved progress of p's request atomically.
func (s Store) Save(p *Progress) error {
	if !gitwork.ValidRequestID(p.Request.ID) {
		return errors.New("依頼IDが不正です")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.File(p.Request.ID), ".request-", 0600, true, data, nil)
}

// Load returns the saved progress of r, or saves and returns a new claimed
// one. A saved request must be the same request the intake handed out.
func (s Store) Load(r Request) (*Progress, error) {
	data, err := os.ReadFile(s.File(r.ID))
	if errors.Is(err, os.ErrNotExist) {
		p := &Progress{Request: r, Stage: "claimed"}
		return p, s.Save(p)
	}
	if err != nil {
		return nil, err
	}
	var p Progress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("保存済みの依頼%sの記録が壊れています（%s）: %w", r.ID, s.File(r.ID), err)
	}
	if field := Mismatch(p.Request, r); field != "" {
		return nil, fmt.Errorf("保存済みの依頼%sと受付の内容が一致しません（%sが違います）", r.ID, field)
	}
	return &p, nil
}

// Saved reads the saved progress of id without creating one.
func (s Store) Saved(id string) (*Progress, error) {
	data, err := os.ReadFile(s.File(id))
	if err != nil {
		return nil, err
	}
	var p Progress
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Mismatch names the first field in which the saved request differs from
// the one the intake handed out, or returns "".
func Mismatch(saved, current Request) string {
	switch {
	case saved.ID != current.ID:
		return "依頼ID"
	case saved.Kind != current.Kind:
		return "種類"
	case saved.Repo != current.Repo:
		return "リポジトリ"
	case !sameJSON(saved.Payload, current.Payload):
		return "内容"
	}
	return ""
}

// sameJSON compares two payloads as encoding/json writes them. Saving
// re-encodes the payload, compacting it and escaping <, > and &, so the bytes
// of a saved payload can differ from the intake's for the same request.
// Values, numbers and key order must still match exactly.
func sameJSON(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	x, okA := encoded(a)
	y, okB := encoded(b)
	return okA && okB && bytes.Equal(x, y)
}

func encoded(raw json.RawMessage) ([]byte, bool) {
	var compact, escaped bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil, false
	}
	json.HTMLEscape(&escaped, compact.Bytes())
	return escaped.Bytes(), true
}

// ErrReleased is returned by Release once the intake took the request back.
var ErrReleased = errors.New("依頼を待ちへ戻しました")

// Release returns the request id to the intake's queue.
func Release(ctx context.Context, api *intake.Client, id string) error {
	if err := api.Call(ctx, "POST", "/api/v1/requests/"+id+"/release", map[string]any{}, nil); err != nil {
		return err
	}
	return ErrReleased
}
