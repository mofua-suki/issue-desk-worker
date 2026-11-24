package providers

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"time"
)

type codexUsagePoint struct{ input, cached, output int64 }

const maxCodexBaselineBytes = 64 << 20

type codexUsageBaseline struct {
	path         string
	info         os.FileInfo
	size, offset int64
	digest       [32]byte
	point        codexUsagePoint
	valid        bool
	boundary     bool
}

// Cumulative Codex samples require all three native fields. The shared usage
// parser accepts aliases for other providers; those must not mask a missing or
// invalid cumulative counter when deciding whether subtraction is safe.
func codexCumulativeUsage(raw json.RawMessage) Usage {
	var sample struct {
		Input  *int64 `json:"input_tokens"`
		Cached *int64 `json:"cached_input_tokens"`
		Output *int64 `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &sample) != nil || sample.Input == nil || sample.Cached == nil || sample.Output == nil {
		return Usage{}
	}
	if *sample.Input < 0 || *sample.Cached < 0 || *sample.Output < 0 || *sample.Cached > *sample.Input {
		return Usage{}
	}
	return Usage{Input: sample.Input, Cached: sample.Cached, Output: sample.Output}
}

// A baseline is taken before starting a resumed process, never inferred from a
// sample written by that process. The retained tail also detects replacement.
func snapshotCodexUsage(home, session string, now time.Time) codexUsageBaseline {
	var b codexUsageBaseline
	b.path = codexRollout(home, session, now)
	f, err := os.Open(b.path)
	if err != nil {
		return b
	}
	defer f.Close()
	b.info, err = f.Stat()
	if err != nil || !b.info.Mode().IsRegular() {
		return b
	}
	b.size = b.info.Size()
	b.offset = b.size - limitTailBytes
	if b.offset < 0 {
		b.offset = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(f, b.offset, b.size-b.offset))
	if err != nil || int64(len(data)) != b.size-b.offset || len(data) == 0 || data[len(data)-1] != '\n' {
		return b
	}
	b.digest = sha256.Sum256(data)
	b.boundary = true
	if b.size > maxCodexBaselineBytes {
		return b
	}
	// Scan the full baseline so a quiet tail does not hide the last sample.
	found := false
	baseline := io.NewSectionReader(f, 0, b.size)
	if !scanCodexLines(baseline, func(line []byte) bool {
		p, token, valid := codexUsageLine(line)
		if token {
			b.point, found = p, true
		}
		return valid
	}) {
		return b
	}

	if pos, err := baseline.Seek(0, io.SeekCurrent); err != nil || pos != b.size {
		return b
	}
	b.valid = found
	return b
}

// codexContextUsage reads only a verified execution range, independently of
// cumulative usage recovery and stdout usage.
func codexContextUsage(home, session string, started time.Time, resumed bool, b codexUsageBaseline) Usage {
	path := codexRollout(home, session, time.Now())
	f, err := os.Open(path)
	if err != nil {
		return Usage{}
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return Usage{}
	}
	var offset int64
	if resumed {
		if !b.boundary || path != b.path || !os.SameFile(b.info, stat) || stat.Size() < b.size || stat.Size()-b.size > limitTailBytes {
			return Usage{}
		}
		prior, err := io.ReadAll(io.NewSectionReader(f, b.offset, b.size-b.offset))
		if err != nil || sha256.Sum256(prior) != b.digest {
			return Usage{}
		}
		offset = b.size
	} else if stat.Size() > limitTailBytes {
		return Usage{}
	}
	data, err := io.ReadAll(io.NewSectionReader(f, offset, stat.Size()-offset))
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		return Usage{}
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	if !resumed {
		var meta struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				ID string `json:"id"`
			} `json:"payload"`
		}
		if json.Unmarshal(lines[0], &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != session {
			return Usage{}
		}
		at, err := time.Parse(time.RFC3339Nano, meta.Timestamp)
		if err != nil || at.Before(started) {
			return Usage{}
		}
	}
	var max, last, calls int64
	for _, line := range lines {
		var e struct {
			Type    string `json:"type"`
			Payload struct {
				Type string          `json:"type"`
				Info json.RawMessage `json:"info"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &e) != nil {
			return Usage{}
		}
		if e.Type != "event_msg" || e.Payload.Type != "token_count" {
			continue
		}
		if len(e.Payload.Info) == 0 || string(e.Payload.Info) == "null" {
			continue
		}
		var info map[string]json.RawMessage
		if json.Unmarshal(e.Payload.Info, &info) != nil || info == nil {
			return Usage{}
		}
		raw, exists := info["last_token_usage"]
		if !exists || string(raw) == "null" {
			continue
		}
		var lastUsage map[string]json.RawMessage
		if json.Unmarshal(raw, &lastUsage) != nil || lastUsage == nil {
			return Usage{}
		}
		input, exists := lastUsage["input_tokens"]
		if !exists {
			continue
		}
		n := nonnegativeInt(input)
		if n == nil {
			return Usage{}
		}
		last = *n
		if calls == 0 || last > max {
			max = last
		}
		calls++
	}
	if calls == 0 {
		return Usage{}
	}
	return Usage{ContextMaxTokens: &max, ContextTotalTokens: &last, Calls: &calls}
}

// token_count.info.total_token_usage is cumulative, including prior turns.
func codexUsageLine(line []byte) (codexUsagePoint, bool, bool) {
	var e struct {
		Type    string `json:"type"`
		Payload struct {
			Type string          `json:"type"`
			Info json.RawMessage `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &e) != nil {
		return codexUsagePoint{}, false, false
	}
	if e.Type != "event_msg" || e.Payload.Type != "token_count" {
		return codexUsagePoint{}, false, true
	}
	if len(e.Payload.Info) == 0 || string(e.Payload.Info) == "null" {
		return codexUsagePoint{}, false, true
	}
	var info struct {
		Total json.RawMessage `json:"total_token_usage"`
	}
	if json.Unmarshal(e.Payload.Info, &info) != nil {
		return codexUsagePoint{}, false, false
	}
	if len(info.Total) == 0 {
		return codexUsagePoint{}, false, true
	}
	u := codexCumulativeUsage(info.Total)
	if u.Input == nil || u.Cached == nil || u.Output == nil || *u.Cached > *u.Input {
		return codexUsagePoint{}, false, false
	}
	return codexUsagePoint{*u.Input, *u.Cached, *u.Output}, true, true
}

// scanCodexLines bounds memory per record and rejects incomplete final records.
func scanCodexLines(r io.Reader, visit func([]byte) bool) bool {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// JSON records may exceed the reader buffer, but have a fixed memory cap.
			buf := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull && len(buf) <= limitTailBytes {
				line, err = reader.ReadSlice('\n')
				buf = append(buf, line...)
			}
			line = buf
		}
		if len(line) > limitTailBytes {
			return false
		}
		if err == io.EOF {
			return len(line) == 0
		}
		if err != nil || !visit(line[:len(line)-1]) {
			return false
		}
	}
}

// Verify the entire execution range while retaining only one record at a time.
func codexUsageRange(home, session string, started time.Time, resumed bool, b codexUsageBaseline) (codexUsagePoint, bool, bool) {
	path := codexRollout(home, session, time.Now())
	f, err := os.Open(path)
	if err != nil {
		return codexUsagePoint{}, false, false
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return codexUsagePoint{}, false, false
	}
	var offset int64
	var latest codexUsagePoint
	if resumed {
		if !b.valid || path != b.path || !os.SameFile(b.info, stat) || stat.Size() < b.size {
			return latest, false, false
		}
		prior, err := io.ReadAll(io.NewSectionReader(f, b.offset, b.size-b.offset))
		if err != nil || int64(len(prior)) != b.size-b.offset || sha256.Sum256(prior) != b.digest {
			return latest, false, false
		}
		offset, latest = b.size, b.point
	} else if stat.Size() > limitTailBytes {
		return latest, false, false
	}
	found, first := false, true
	section := io.NewSectionReader(f, offset, stat.Size()-offset)
	valid := scanCodexLines(section, func(line []byte) bool {
		if first && !resumed {
			var meta struct {
				Timestamp string `json:"timestamp"`
				Type      string `json:"type"`
				Payload   struct {
					ID string `json:"id"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" || meta.Payload.ID != session {
				return false
			}
			at, err := time.Parse(time.RFC3339Nano, meta.Timestamp)
			if err != nil || at.Before(started) {
				return false
			}
		}
		first = false
		p, token, ok := codexUsageLine(line)
		if !ok {
			return false
		}
		if token {
			if p.input < latest.input || p.cached < latest.cached || p.output < latest.output {
				return false
			}
			latest, found = p, true
		}
		return true
	})
	pos, err := section.Seek(0, io.SeekCurrent)
	return latest, found, valid && err == nil && pos == stat.Size()-offset && (resumed || !first)
}

func codexUsageDelta(latest, base codexUsagePoint) Usage {
	if latest.input < base.input || latest.cached < base.cached || latest.output < base.output {
		return Usage{}
	}
	i, c, o := latest.input-base.input, latest.cached-base.cached, latest.output-base.output
	if c > i {
		return Usage{}
	}
	return Usage{Input: &i, Cached: &c, Output: &o}
}

// Normalize once, after the process ends: stdout contains the last cumulative
// sample, not a sequence of per-turn amounts to add together.
func resumedCodexUsage(home, session string, started time.Time, b codexUsageBaseline, stdout Usage) Usage {
	latest, found, valid := codexUsageRange(home, session, started, true, b)
	if !valid {
		return Usage{}
	}
	if stdout.Input != nil && stdout.Cached != nil && stdout.Output != nil && *stdout.Cached <= *stdout.Input {
		u := codexUsageDelta(codexUsagePoint{*stdout.Input, *stdout.Cached, *stdout.Output}, b.point)
		if u.Input != nil {
			return u
		}
	}
	if found {
		return codexUsageDelta(latest, b.point)
	}
	return Usage{}
}

func recoverCodexUsage(home, session string, started time.Time, resumed bool, b codexUsageBaseline) Usage {
	latest, found, valid := codexUsageRange(home, session, started, resumed, b)
	var base codexUsagePoint
	if resumed {
		base = b.point
	}
	if !valid || !found {
		return Usage{}
	}
	return codexUsageDelta(latest, base)
}
