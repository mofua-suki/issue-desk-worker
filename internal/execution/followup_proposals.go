package execution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"issue-desk-worker/internal/journal"
)

type reviewFindingSpec struct {
	Group             string `json:"group"`
	Problem           string `json:"problem"`
	Expected          string `json:"expected"`
	Test              string `json:"test"`
	ExternalCheckOnly *bool  `json:"externalCheckOnly,omitempty"`
}

type followupProposalItem struct {
	InterruptReason   string `json:"interruptReason,omitempty"`
	Problem           string `json:"problem"`
	Expected          string `json:"expected"`
	Test              string `json:"test"`
	ExternalCheckOnly *bool  `json:"externalCheckOnly,omitempty"`
}

// The normalized review retains one item per improvement. Default details also
// preserve older saved review text for records and historical reconciliation.
func reviewProposalItems(review Review) []followupProposalItem {
	return reviewProposalItemsForDelivery(review, false)
}

// Old attempted batches must be reconstructed exactly for marker recovery.
func reviewProposalItemsForDelivery(review Review, historical bool) []followupProposalItem {
	var items []followupProposalItem
	add := func(problem string, improvementIndex, findingIndex int) {
		item := followupProposalItem{Problem: problem, Expected: "次の指摘を解消する: " + problem, Test: "指摘に対応する確認手段と期待結果で解消を確認する。報告訂正は訂正内容と根拠を照合し、製品の挙動を変える場合は挙動の回帰テストを実行する: " + problem}
		if improvementIndex >= 0 && improvementIndex < len(review.ImprovementSpecs) && review.ImprovementSpecs[improvementIndex].Problem == problem {
			item = review.ImprovementSpecs[improvementIndex]
		}
		var spec *reviewFindingSpec
		if findingIndex >= 0 && findingIndex < len(review.FindingSpecs) && review.FindingSpecs[findingIndex].Problem == problem {
			spec = &review.FindingSpecs[findingIndex]
		} else {
			for i := range review.FindingSpecs {
				if review.FindingSpecs[i].Problem == problem {
					spec = &review.FindingSpecs[i]
					break
				}
			}
		}
		if historical && spec == nil && findingIndex >= 0 && findingIndex < len(review.FindingSpecs) {
			spec = &review.FindingSpecs[findingIndex]
		}
		if spec != nil {
			item.Expected, item.Test, item.ExternalCheckOnly = spec.Expected, spec.Test, spec.ExternalCheckOnly
		}
		for _, interrupt := range review.Interrupts {
			if interrupt.Text == problem {
				item.InterruptReason = normalizedInterruptReason(interrupt.Reason)
				break
			}
		}
		items = append(items, item)
	}
	for i, problem := range review.Improvements {
		add(problem, i, -1)
	}
	for i, problem := range review.Findings {
		add(problem, -1, i)
	}
	for _, problem := range review.ResolvableUncertainties {
		add(problem, -1, -1)
	}
	return items
}

// Legacy reviews have no explicit classification. Require both an external
// location and a check, and retain any item mentioning implementation work.
func externalCheckOnly(item followupProposalItem) bool {
	if item.ExternalCheckOnly != nil {
		return *item.ExternalCheckOnly
	}
	text := item.Problem
	if item.Expected != "次の指摘を解消する: "+item.Problem || item.Test != "次の指摘の解消を確認するテストを追加・実行する: "+item.Problem && item.Test != "指摘に対応する確認手段と期待結果で解消を確認する。報告訂正は訂正内容と根拠を照合し、製品の挙動を変える場合は挙動の回帰テストを実行する: "+item.Problem {
		text += " " + item.Expected + " " + item.Test
	}
	text = strings.ReplaceAll(text, "確認済み根拠:", "")
	text = strings.ToLower(strings.ReplaceAll(text, "実装PC", "作業PC"))
	contains := func(words []string) bool {
		for _, word := range words {
			if strings.Contains(text, word) {
				return true
			}
		}
		return false
	}
	locations := []string{"配備後", "配備先", "本番", "実機", "別pc", "他のpc", "コントローラー", "gitea api", "実データ", "外部db", "外部データベース", "外部サービス"}
	checks := []string{"確認", "照合", "確かめ", "検証", "接続試験"}
	work := []string{"直す", "修正", "追加", "変更", "実装", "テストを足す", "改修", "作成"}
	return contains(locations) && contains(checks) && !contains(work)
}

// Eligibility is checked on the complete review item, before examining its
// individual wire fragments. The review supplies the factual evidence; the
// worker only checks the requested shape and obvious omissions.
func eligibleFollowup(item followupProposalItem) bool {
	if strings.TrimSpace(item.Problem) == "" || externalCheckOnly(item) {
		return false
	}
	first, rest, ok := strings.Cut(strings.TrimSpace(item.Expected), "\n")
	if !ok || !strings.HasPrefix(first, "確認済み根拠:") || strings.TrimSpace(strings.TrimPrefix(first, "確認済み根拠:")) == "" || strings.TrimSpace(rest) == "" {
		return false
	}
	test := strings.TrimSpace(item.Test)
	if test == "" || strings.HasPrefix(test, "指摘に対応する確認手段と期待結果で解消を確認する。") || strings.HasPrefix(test, "次の指摘の解消を確認するテストを追加・実行する:") {
		return false
	}
	// The method precedes the labelled values, so neither value can stand in
	// for the method. Both colon forms have the same meaning.
	test = strings.ReplaceAll(test, "再現入力：", "再現入力:")
	test = strings.ReplaceAll(test, "期待結果：", "期待結果:")
	input, result := strings.Index(test, "再現入力:"), strings.Index(test, "期待結果:")
	if input < 0 || result < 0 || strings.Count(test, "再現入力:") != 1 || strings.Count(test, "期待結果:") != 1 {
		return false
	}
	firstIndex, second := min(input, result), max(input, result)
	firstLabel := "再現入力:"
	secondLabel := "期待結果:"
	if result < input {
		firstLabel, secondLabel = secondLabel, firstLabel
	}
	return followupHasContent(test[:firstIndex]) &&
		followupHasContent(test[firstIndex+len(firstLabel):second]) &&
		followupHasContent(test[second+len(secondLabel):])
}

func followupHasContent(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) && !unicode.IsPunct(r) }) >= 0
}

// Split rather than truncate: the intake contract limits each field to 2000
// UTF-16 units and each proposal to five items, even for very long review text.
func proposalTextParts(text string) []string {
	parts := []string{}
	start, units := 0, 0
	for i, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > 2000 {
			parts = append(parts, text[start:i])
			start, units = i, 0
		}
		units += width
	}
	return append(parts, text[start:])
}

func proposalWireItems(items []followupProposalItem) []followupProposalItem {
	var out []followupProposalItem
	for _, item := range items {
		problems, expected, tests := proposalTextParts(item.Problem), proposalTextParts(item.Expected), proposalTextParts(item.Test)
		for i := 0; i < max(len(problems), len(expected), len(tests)); i++ {
			part := followupProposalItem{InterruptReason: normalizedInterruptReason(item.InterruptReason)}
			if i < len(problems) {
				part.Problem = problems[i]
			}
			if i < len(expected) {
				part.Expected = expected[i]
			}
			if i < len(tests) {
				part.Test = tests[i]
			}
			out = append(out, part)
		}
	}
	return out
}

func normalizedInterruptReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if !utf8.ValidString(reason) || utf8.RuneCountInString(reason) > 500 {
		return ""
	}
	return reason
}

func wireProposalItem(item followupProposalItem) followupProposalItem {
	item.InterruptReason = ""
	return item
}

func followupProposalID(r *journal.Record, batch int) string {
	sum := sha256.Sum256([]byte(r.Head))
	return fmt.Sprintf("worker-%d-%d-%x-%d", r.ID, r.Generation, sum[:16], batch)
}

func (e *Executor) followupProposalNotice(r *journal.Record, batch int, items []followupProposalItem) (notification, error) {
	id := followupProposalID(r, batch)
	if r.PR < 1 {
		return notification{}, errors.New("後続案を投稿できません: 元PRがありません")
	}
	provider := r.Provider
	if provider != "claude" {
		provider = "codex"
	}
	reason := ""
	wireItems := make([]followupProposalItem, len(items))
	for i, item := range items {
		if i == 0 {
			reason = normalizedInterruptReason(item.InterruptReason)
		}
		wireItems[i] = wireProposalItem(item)
	}
	proposal := map[string]any{
		"version": 3, "kind": "followup", "id": id,
		"source": fmt.Sprintf("%s#%d", r.Repo, r.Issue),
		"title":  fmt.Sprintf("PR #%d の軽微な指摘への対応（%d）", r.PR, batch+1),
		"pr":     strings.TrimRight(e.Config.GiteaURL, "/") + fmt.Sprintf("/%s/pulls/%d", r.Repo, r.PR),
		"items":  wireItems, "dependsOn": []string{}, "provider": provider, "changeScope": nil,
	}
	if reason != "" {
		proposal["priority"] = map[string]string{"level": "high", "reason": reason}
	}
	marker := "<!-- issue-desk-proposal " + string(encode(proposal)) + " -->"
	if _, err := parseProposalComment(marker); err != nil {
		return notification{}, err
	}
	// Do not redact serialized JSON: doing so could corrupt the wire contract or
	// quietly change an expected/test field. Leave a recorded failure instead.
	if e.containsCredential(marker) {
		return notification{}, errors.New("後続案に秘密情報の伏字化が必要なため投稿しません")
	}
	identity := "<!-- issue-desk-followup:" + id + " -->"
	body := "承認時の軽微な指摘を後続案として残します。元PRのマージ後に受付が起票します。\n\n" + marker + "\n\n" + identity
	return notification{JobID: r.ID, Epoch: r.Epoch, Host: e.Config.Host, Author: e.Config.Assignments.WorkerAccounts[e.Config.Host], Repo: r.Repo, Target: r.Issue, Kind: "followup", Head: r.Head, Marker: identity, Body: body}, nil
}

// Delivery errors are data, not merge gates. Only failure to persist the result
// (or cancellation) stops execution, as continuing would lose the failure record.
func (e *Executor) postFollowupProposals(ctx context.Context, r *journal.Record) error {
	var review Review
	if json.Unmarshal(r.Review, &review) != nil || !review.Approved() {
		return nil
	}
	beforeRetention := string(r.Review)
	retainSentReviewDeliveries(r)
	if string(r.Review) != beforeRetention {
		if err := e.Store.Save(r); err != nil {
			return err
		}
	}
	if err := e.reconcileDeferredDeliveries(ctx, r); err != nil {
		return err
	}
	_ = json.Unmarshal(r.Review, &review)
	excluded := make(map[string]bool, len(review.RecordOnly))
	for _, text := range review.RecordOnly {
		if key := strings.Join(strings.Fields(text), " "); key != "" {
			excluded[key] = true
		}
	}
	var selected []followupProposalItem
	for _, item := range reviewProposalItems(review) {
		if !excluded[strings.Join(strings.Fields(item.Problem), " ")] && !externalCheckOnly(item) {
			selected = append(selected, item)
		}
	}
	restoreDeferredDeliveryResults(r, r.Review)
	// Resolve attempted parts using the original list and original identities.
	// Only then filter each remaining part against its own complete candidate.
	_, _, batchBase := deferredProposalWireItems(r, review, selected)
	var items []followupProposalItem
	var sources []deferredReviewSource
	for _, candidate := range selected {
		parts, partSources, _ := deferredProposalWireItems(r, review, []followupProposalItem{candidate})
		if eligibleFollowup(candidate) {
			items = append(items, parts...)
			sources = append(sources, partSources...)
		}
	}
	items, sources = groupProposalItems(items, sources)
	for offset, batch := 0, batchBase; offset < len(items); batch++ {
		id := followupProposalID(r, batch)
		state := journal.FollowupProposal{ID: id, Generation: r.Generation, Head: r.Head, Status: "failed", DeliveryOutcome: "not_attempted"}
		end := offset + 1
		reason := normalizedInterruptReason(items[offset].InterruptReason)
		for end < len(items) && end-offset < 5 && normalizedInterruptReason(items[end].InterruptReason) == reason {
			end++
		}
		batchItems, batchSources := items[offset:end], sources[offset:end]
		n, err := e.followupProposalNotice(r, batch, batchItems)
		if err == nil {
			var sent bool
			var saveErr error
			sent, err = e.syncNotification(ctx, n, func() error {
				// Persist the association after outbox attempted, before POST.
				state.Error = "後続案の投稿結果が未確認です"
				state.DeliveryOutcome = "unknown"
				saveDeferredDelivery(r, batchItems, batchSources, state, n.Author, false)
				saveErr = e.Store.Save(r)
				return saveErr
			})
			if saveErr != nil {
				return saveErr
			}
			if sent && err == nil {
				state.Status, state.Error, state.DeliveryOutcome = "sent", "", "confirmed"
			}
			if !sent && err == nil {
				err = errors.New("後続案の投稿を確認できません（応答不明またはlease喪失）")
			}
		}
		if err != nil {
			state.Error = clip(e.redact(err.Error()), 500)
			if rejectedPost(err) {
				state.DeliveryOutcome = "rejected"
			}
		}
		// A definite rejection can be retried; an attempted/unknown response
		// stays bound to the original identity, even on another PC or head.
		_, deliveries := retainedReviewData(r.Review)
		attempted := state.Status == "sent"
		for _, delivery := range deliveries {
			attempted = attempted || delivery.Proposal.ID == id
		}
		if attempted {
			saveDeferredDelivery(r, batchItems, batchSources, state, n.Author, rejectedPost(err))
		} else {
			updateFollowupResult(r, state)
		}
		if err := e.Store.Save(r); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		offset = end
	}
	return nil
}

// Keep first-seen group order; an all-normal review keeps its original batching.
func groupProposalItems(items []followupProposalItem, sources []deferredReviewSource) ([]followupProposalItem, []deferredReviewSource) {
	var grouped []followupProposalItem
	var groupedSources []deferredReviewSource
	seen := map[string]bool{}
	for _, item := range items {
		reason := normalizedInterruptReason(item.InterruptReason)
		if seen[reason] {
			continue
		}
		seen[reason] = true
		for i, candidate := range items {
			if normalizedInterruptReason(candidate.InterruptReason) == reason {
				grouped = append(grouped, candidate)
				groupedSources = append(groupedSources, sources[i])
			}
		}
	}
	return grouped, groupedSources
}
