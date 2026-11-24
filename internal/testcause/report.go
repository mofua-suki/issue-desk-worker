package testcause

import "strings"

// Classify recalculates persisted labels from independently verifiable evidence.
func Classify(targetValue any, baselineValues []any) Object {
	target := cleanObservation(targetValue)
	observations := []Object{}
	bases, matching := []Object{}, []Object{}
	for _, raw := range baselineValues {
		if clean := cleanObservation(raw); clean != nil {
			observations = append(observations, clean)
			if target != nil && clean["sha"] == target["baseSha"] {
				bases = append(bases, clean)
				if clean["complete"] == true && clean["command"] == target["command"] && clean["environment"] == target["environment"] {
					matching = append(matching, clean)
				}
			}
		}
	}
	base := Object(nil)
	if len(matching) > 0 {
		base = matching[0]
	} else if len(bases) > 0 {
		base = bases[0]
	} else if len(observations) > 0 {
		base = observations[0]
	}
	var shaMatches, commandMatches, environmentMatches any
	if base != nil && target != nil {
		shaMatches = base["sha"] == target["baseSha"]
		if hash.MatchString(str(base["command"])) && hash.MatchString(str(target["command"])) {
			commandMatches = base["command"] == target["command"]
		}
		if hash.MatchString(str(base["environment"])) && hash.MatchString(str(target["environment"])) {
			environmentMatches = base["environment"] == target["environment"]
		}
	}
	kind, reason := "unknown", "基点証拠がありません。失敗検査だけを過去SHAで実行する安全な既存経路がありません。"
	switch {
	case target == nil:
		reason = "対象の構造化された比較証拠がありません。"
	case target["sha"] == nil || target["baseSha"] == nil:
		missing := []string{}
		if target["sha"] == nil {
			missing = append(missing, "対象SHA")
		}
		if target["baseSha"] == nil {
			missing = append(missing, "基点SHA")
		}
		reason = strings.Join(missing, "・") + "を確認できません。"
	case target["conditionsComplete"] != true:
		reason = "コマンドまたは環境条件が欠測か形式不正のため、原因を確定できません。"
	case target["complete"] != true:
		reason = "対象の証拠が不完全です。途中打切りや取得不足から原因を確定できません。"
	case target["passed"] == true || len(arr(target["failures"])) == 0 && target["environmentFailure"] == nil:
		reason = "対象の失敗を確認できる証拠がありません。"
	case target["environmentFailure"] != nil:
		kind, reason = "environment", "既存の環境判定がテスト開始前の作業領域外の権限不足を確認しました。"
	case len(matching) > 0:
		outcomes := map[string]bool{}
		for _, b := range matching {
			outcome := "passed"
			if b["passed"] != true {
				pairs := []any{}
				for _, raw := range arr(b["failures"]) {
					f := obj(raw)
					pairs = append(pairs, []any{f["test"], f["fingerprint"]})
				}
				outcome = string(encoding(pairs))
			}
			outcomes[outcome] = true
		}
		if len(outcomes) > 1 {
			reason = "同じ比較条件の完全な基点観測に合否または失敗診断の矛盾があります。"
		} else if matching[0]["passed"] == true && target["sha"] != matching[0]["sha"] {
			kind, reason = "change", "同じ基点・実行条件の検査は合格し、対象では失敗しています。"
		} else if matching[0]["passed"] == true {
			reason = "同じSHAの合否が異なるため、変更由来と確定できません。"
		} else {
			same := true
			for _, raw := range arr(target["failures"]) {
				f := obj(raw)
				found := false
				for _, b := range arr(matching[0]["failures"]) {
					other := obj(b)
					found = found || other["test"] == f["test"] && other["fingerprint"] == f["fingerprint"]
				}
				same = same && found
			}
			if same {
				kind, reason = "baseline", "同じ基点・実行条件でテスト識別と診断指紋が一致しました。"
			} else {
				reason = "失敗名または診断指紋が異なります。同名だけでは同一原因を確定できません。"
			}
		}
	case base != nil:
		if shaMatches != true {
			reason = "保存された基点証拠のSHAが比較基点と一致しません。"
		} else if base["complete"] != true {
			reason = "基点証拠が不完全です。"
		} else if commandMatches != true || environmentMatches != true {
			reason = "コマンドまたは環境条件が一致しません。条件差だけでは原因を確定できません。"
		} else {
			reason = "完全で条件が一致する基点観測がありません。"
		}
	}
	route := "issue-repair"
	if kind == "baseline" {
		route = "main-repair"
	} else if kind == "environment" {
		route = "environment-hold"
	}
	out := Object{"version": float64(1), "kind": kind, "reason": reason, "route": route, "target": target, "baseline": base, "baseShaMatches": shaMatches, "commandMatches": commandMatches, "environmentMatches": environmentMatches}
	if len(matching) > 1 {
		out["baselines"] = matching
	}
	return out
}

// Report applies the wire budget after sanitization, never retains a classification
// while truncating the evidence that justified it.
func Report(value any, budget int) Object {
	v := obj(value)
	baselines := arr(v["baselines"])
	if typed, ok := v["baselines"].([]Object); ok {
		baselines = make([]any, len(typed))
		for i, baseline := range typed {
			baselines[i] = baseline
		}
	}
	if len(baselines) == 0 && v["baseline"] != nil {
		baselines = []any{v["baseline"]}
	}
	result := Classify(v["target"], baselines)
	if len(encoding(result)) > budget {
		return Classify(nil, nil)
	}
	return result
}
