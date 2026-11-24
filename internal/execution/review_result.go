package execution

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"

	"issue-desk-worker/internal/intake"
)

// The archive is only a transport representation inside the existing review
// JSON. Journals, comparisons and provider prompts always use the full review.
// Bound expansion before accepting a carried report from another Worker.
const maxReviewArchiveSize = 8 << 20

// String references preserve JSON types: every string value in Value is a
// decimal dictionary index. Object keys and non-string values stay unchanged.
// Interning, unlike a limited compression window, also removes repetitions of
// distinct long problem/expected/test fields across findings and retention data.
type reviewArchive struct {
	Version int             `json:"version"`
	Strings []string        `json:"strings"`
	Value   json.RawMessage `json:"value"`
}

func archiveRetainedReview(raw json.RawMessage) (json.RawMessage, bool, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object["reviewArchive"] != nil {
		expanded, valid := expandCarriedReview(raw)
		if !valid {
			return nil, true, errors.New("保持レビューのアーカイブを展開できません")
		}
		var review Review
		if err := json.Unmarshal(expanded, &review); err != nil {
			return nil, true, err
		}
		items, deliveries := retainedReviewData(expanded)
		return raw, len(items) > 0 || len(deliveries) > 0, nil
	}
	items, deliveries := retainedReviewData(raw)
	if len(items) == 0 && len(deliveries) == 0 {
		return raw, false, nil
	}
	if len(raw) > maxReviewArchiveSize {
		return nil, true, errors.New("保持レビューが持ち越し可能なサイズを超えています")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, true, err
	}
	archive := reviewArchive{Version: 1}
	indices := map[string]string{}
	var intern func(any) any
	intern = func(v any) any {
		switch v := v.(type) {
		case string:
			if index, ok := indices[v]; ok {
				return index
			}
			index := strconv.Itoa(len(archive.Strings))
			indices[v] = index
			archive.Strings = append(archive.Strings, v)
			return index
		case []any:
			for i := range v {
				v[i] = intern(v[i])
			}
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				v[key] = intern(v[key])
			}
		}
		return v
	}
	archive.Value = encode(intern(value))
	var review Review
	_ = json.Unmarshal(raw, &review)
	return encode(map[string]any{
		"reviewArchive": archive,
		"summary":       clip(review.Summary, 2000),
		// An archive cannot be used as comparison evidence without decoding it.
		"reviewComparisonBlocked": true,
	}), true, nil
}

func expandCarriedReview(raw json.RawMessage) (json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object["reviewArchive"] == nil {
		return raw, true
	}
	invalid := encode(map[string]bool{"reviewComparisonBlocked": true})
	var archive reviewArchive
	var value any
	if json.Unmarshal(object["reviewArchive"], &archive) != nil || archive.Version != 1 {
		return invalid, false
	}
	decoder := json.NewDecoder(bytes.NewReader(archive.Value))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return invalid, false
	}
	remaining := maxReviewArchiveSize
	valid := true
	var expand func(any) any
	expand = func(v any) any {
		if !valid {
			return nil
		}
		switch v := v.(type) {
		case string:
			index, err := strconv.Atoi(v)
			if err != nil || index < 0 || index >= len(archive.Strings) {
				valid = false
				return nil
			}
			text := archive.Strings[index]
			remaining -= len(text)
			if remaining < 0 {
				valid = false
				return nil
			}
			return text
		case []any:
			for i := range v {
				v[i] = expand(v[i])
			}
		case map[string]any:
			for key, field := range v {
				v[key] = expand(field)
			}
		}
		return v
	}
	full := encode(expand(value))
	var expanded map[string]json.RawMessage
	if !valid || len(full) > maxReviewArchiveSize || json.Unmarshal(full, &expanded) != nil || expanded == nil || expanded["reviewArchive"] != nil {
		return invalid, false
	}
	return full, true
}

// resultUnits counts the minimal JSON representation of decoded values, in
// UTF-16 units, including required escaping and structural characters.
func resultUnits(v any) int {
	switch v := v.(type) {
	case string:
		n := 2
		for _, r := range v {
			switch {
			case r == '"' || r == '\\' || r == '\b' || r == '\f' || r == '\n' || r == '\r' || r == '\t':
				n += 2
			case r < 0x20:
				n += 6
			case r >= 0x10000:
				n += 2
			default:
				n++
			}
		}
		return n
	case map[string]any:
		n := 2
		for k, value := range v {
			n += resultUnits(k) + 1 + resultUnits(value) + 1
		}
		if len(v) > 0 {
			n--
		}
		return n
	case []any:
		n := 2
		for _, value := range v {
			n += resultUnits(value) + 1
		}
		if len(v) > 0 {
			n--
		}
		return n
	case json.Number:
		f, _ := strconv.ParseFloat(string(v), 64)
		if math.IsInf(f, 0) {
			return 4 // JSON.stringify serializes non-finite numbers as null.
		}
		if f == 0 {
			return 1
		}
		return len(encode(f))
	case bool:
		if v {
			return 4
		}
		return 5
	case nil:
		return 4
	default:
		return utf8.RuneCount(encode(v))
	}
}

func boundFinalResult(epoch int64, state *intake.State) error {
	requestSize := func() int {
		return len(encode(struct {
			Epoch int64 `json:"epoch"`
			intake.State
		}{epoch, *state}))
	}
	// 受付は報告の状態によらず result を32768単位まで受け付け、超えると400で断る
	// （server/internal/jobs/report.go の checkObject）。断られた最終報告は lease が切れ続けて
	// 失敗に変わり、保留の理由も失われるため、すべての最終報告で省ける報告を省いて上限に収める。
	var view map[string]json.RawMessage
	_ = json.Unmarshal(state.View, &view)
	_, permissionHold := view["permissionHold"]
	strict := permissionHold || state.Status == "merged"
	fits := func() bool {
		if requestSize() > 128*1024 {
			return false
		}
		if !hasJSON(state.Result) {
			return true
		}
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(state.Result))
		decoder.UseNumber()
		return decoder.Decode(&decoded) == nil && resultUnits(decoded) <= 32768
	}
	if fits() {
		return nil
	}
	// Continue from the redacted result actually destined for intake.
	decoder := json.NewDecoder(bytes.NewReader(state.Result))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return err
	}
	// Required retention data must never be replaced by an omitted marker.
	// Intern repeated detail and delivery text losslessly before dropping
	// optional reports. If even that cannot fit, keep the journal and fail.
	protected := map[string]bool{}
	for _, key := range []string{"review", "previousReview"} {
		if value, ok := result[key]; ok {
			raw := encode(value)
			packed, required, err := archiveRetainedReview(raw)
			if err != nil {
				return err
			}
			if required {
				result[key], protected[key] = packed, true
			}
		}
	}
	state.Result = encode(result)
	// 保持データを損失なく詰めただけの結果。省略を加える前の形として残す。
	packed := state.Result
	for _, key := range []string{"previousImplementationResult", "previousReview", "implementationResult", "review", "fixedTest"} {
		if fits() {
			return nil
		}
		if _, ok := result[key]; ok && !protected[key] {
			result[key] = map[string]any{"omitted": true, "reason": "受付の本文上限を超えるため省略しました"}
			state.Result = encode(result)
		}
	}
	if !fits() {
		// 省けない保持データだけで result の上限を超える保留・失敗の報告は、本文の上限内なら
		// これまでどおり送る（受付が断る点は変わらない）。マージ完了と権限承認待ちは
		// 記録を残して失敗にする。
		if !strict && requestSize() <= 128*1024 {
			// 届かない報告のために実装結果などを削らない。省略を戻し、本文の上限内なら
			// 詰めただけの形で従来どおり送る。
			omitted := state.Result
			state.Result = packed
			if requestSize() > 128*1024 {
				state.Result = omitted
			}
			log.Printf("final_report event=result_over_intake_limit status=%s", state.Status)
			return nil
		}
		return errors.New("最終状態が受付のresultまたは本文上限を超えています")
	}
	return nil
}
