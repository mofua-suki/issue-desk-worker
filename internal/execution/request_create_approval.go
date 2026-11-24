package execution

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"issue-desk-worker/internal/gitwork"
)

var createRequestMarker = regexp.MustCompile(`<!-- issue-desk-pr-request:([^\s<>]+) -->`)

// preapprovedByCreate は、PR番号が決まる前に作成依頼（pr_create）へ付けた承認でマージしてよいかを見る。
// 受付は merge_manual の approvalRequest が指す作成依頼の結果（同じrepo・送り手・PR番号・head）を確かめてから、
// その承認と approvalDelegated を写す。Workerは、運用代行の印・8文字以上の承認・PR本文の作成依頼の印が
// ちょうど1つで approvalRequest と同じことを確かめる。どのPCが作成したPRでも同じ判定になる。
func preapprovedByCreate(p mergeRequestPayload, body string) bool {
	if p.ApprovalRequest == "" || !gitwork.ValidRequestID(p.ApprovalRequest) || !p.ApprovalDelegated || utf8.RuneCountInString(strings.TrimSpace(p.Approval)) < 8 {
		return false
	}
	markers := createRequestMarker.FindAllStringSubmatch(body, -1)
	return len(markers) == 1 && markers[0][1] == p.ApprovalRequest
}
