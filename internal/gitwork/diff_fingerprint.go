package gitwork

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"issue-desk-worker/internal/cause"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"issue-desk-worker/internal/process"
)

// DiffFingerprintEntry is one changed path between the merge base and head.
// A missing side has Mode "000000" and empty Content.
type DiffFingerprintEntry struct {
	Path                   []byte
	OldMode, NewMode       string
	OldContent, NewContent []byte
}

var fingerprintMode = regexp.MustCompile(`^[0-7]{6}$`)

// maxFingerprintBlob bounds one blob read; a larger file leaves the
// fingerprint unreported rather than holding the whole file in memory.
const maxFingerprintBlob = 64 << 20

// FingerprintEntries hashes the entries in the intake's diffFingerprint
// format (api/openapi/40-jobs-merge-requests.yaml of issue-desk-intake). The
// entries are sorted by the raw bytes of their paths; the caller's order does
// not matter.
func FingerprintEntries(entries []DiffFingerprintEntry) (string, error) {
	sorted := append([]DiffFingerprintEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i].Path, sorted[j].Path) < 0 })
	h := sha256.New()
	var size [8]byte
	for i, e := range sorted {
		if len(e.Path) == 0 || (i > 0 && bytes.Equal(sorted[i-1].Path, e.Path)) || !fingerprintMode.MatchString(e.OldMode) || !fingerprintMode.MatchString(e.NewMode) {
			return "", errors.New("差分の指紋の項目が不正です")
		}
		binary.BigEndian.PutUint64(size[:], uint64(len(e.Path)))
		h.Write(size[:])
		h.Write(e.Path)
		h.Write([]byte(e.OldMode))
		h.Write([]byte(e.NewMode))
		for _, content := range [][]byte{e.OldContent, e.NewContent} {
			binary.BigEndian.PutUint64(size[:], uint64(len(content)))
			h.Write(size[:])
			h.Write(content)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// DiffFingerprint returns the fingerprint of the change from the unique merge
// base of base and head to head, as dir's repository sees them. A head that
// only took in base stands for the commit before that merge, so the change
// keeps its fingerprint even when base changed the same files. It fails when
// the merge base is missing or ambiguous; callers then omit the field.
func DiffFingerprint(ctx context.Context, c Config, dir, base, head string) (string, error) {
	if dir == "" || base == "" || strings.HasPrefix(base, "-") || !sha.MatchString(head) {
		return "", errors.New("差分の指紋の対象が不正です")
	}
	head, err := fingerprintOwnHead(ctx, c, dir, base, head)
	if err != nil {
		return "", err
	}
	bases, err := c.checked(ctx, dir, "merge-base", "--all", base, head)
	if err != nil {
		return "", err
	}
	if !sha.MatchString(bases) {
		return "", errors.New("merge-baseが一意に決まりません")
	}
	raw, err := c.checked(ctx, dir, "diff-tree", "-r", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", bases, head, "--")
	if err != nil {
		return "", err
	}
	fields := strings.Split(raw, "\x00")
	var entries []DiffFingerprintEntry
	for i := 0; i < len(fields); i += 2 {
		if fields[i] == "" && i == len(fields)-1 {
			break
		}
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if !strings.HasPrefix(fields[i], ":") || len(meta) != 5 || i+1 >= len(fields) || fields[i+1] == "" {
			return "", errors.New("差分の一覧を読めません")
		}
		e := DiffFingerprintEntry{Path: []byte(fields[i+1]), OldMode: meta[0], NewMode: meta[1]}
		if e.OldContent, err = fingerprintContent(ctx, c, dir, meta[0], meta[2]); err != nil {
			return "", err
		}
		if e.NewContent, err = fingerprintContent(ctx, c, dir, meta[1], meta[3]); err != nil {
			return "", err
		}
		entries = append(entries, e)
	}
	return FingerprintEntries(entries)
}

// fingerprintOwnHead steps back over each merge at head that did nothing but
// take in base: a commit of two parents whose second parent is already in
// base, whose first parent is not, and whose tree is exactly what Git makes
// of the two parents without a conflict. A merge edited by hand, or one that
// needed a conflict resolved, is not stepped over; it is part of the change.
func fingerprintOwnHead(ctx context.Context, c Config, dir, base, head string) (string, error) {
	for {
		line, err := c.checked(ctx, dir, "rev-list", "--parents", "-n", "1", head)
		if err != nil {
			return "", err
		}
		commits := strings.Fields(line)
		if len(commits) != 3 || commits[0] != head || !sha.MatchString(commits[1]) || !sha.MatchString(commits[2]) {
			return head, nil
		}
		own, taken := commits[1], commits[2]
		if in, err := fingerprintInBase(ctx, c, dir, taken, base); err != nil || !in {
			return head, err
		}
		if in, err := fingerprintInBase(ctx, c, dir, own, base); err != nil || in {
			return head, err
		}
		tree, err := c.checked(ctx, dir, "rev-parse", "--verify", head+"^{tree}")
		if err != nil {
			return "", err
		}
		// A conflict, or a Git that cannot merge without a working tree,
		// leaves the merge as part of the change.
		merged, err := c.Run(ctx, dir, "merge-tree", "--write-tree", "--no-messages", own, taken)
		if err != nil {
			return "", err
		}
		if merged.Code != 0 || !sha.MatchString(tree) || strings.TrimSpace(merged.Out) != tree {
			return head, nil
		}
		head = own
	}
}

func fingerprintInBase(ctx context.Context, c Config, dir, commit, base string) (bool, error) {
	out, err := c.Run(ctx, dir, "merge-base", "--is-ancestor", commit, base)
	if err != nil || (out.Code != 0 && out.Code != 1) {
		return false, cause.New("既定ブランチとの関係を読めません", err)
	}
	return out.Code == 0, nil
}

func fingerprintContent(ctx context.Context, c Config, dir, mode, object string) ([]byte, error) {
	switch {
	case mode == "000000":
		return nil, nil
	case !sha.MatchString(object):
		return nil, errors.New("差分のオブジェクトIDが不正です")
	case mode == "160000":
		return []byte(object), nil
	}
	sizeText, err := c.checked(ctx, dir, "cat-file", "-s", object)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size < 0 || size > maxFingerprintBlob {
		return nil, cause.New("差分のファイルを読めません", err)
	}
	// The limit also applies to stderr, so a tiny blob must not make a Git
	// warning fail the read; the length check below catches a wrong size.
	limit := int(size) + 1
	if limit < 64<<10 {
		limit = 64 << 10
	}
	out, err := process.Run(ctx, c.Git, c.args("cat-file", "blob", object), process.Options{Dir: dir, Timeout: 5 * time.Minute, MaxOutputBytes: limit, Env: map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}})
	if err != nil || out.Code != 0 || int64(len(out.Out)) != size {
		return nil, cause.New("差分のファイルを読めません", err)
	}
	return []byte(out.Out), nil
}
