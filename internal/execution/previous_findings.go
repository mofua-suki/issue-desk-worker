package execution

import (
	"encoding/json"
	"issue-desk-worker/internal/journal"
)

type previousFinding struct {
	Index    int    `json:"index"`
	Source   int    `json:"-"`
	Finding  string `json:"finding"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Problem  string `json:"problem"`
}

func previousFindingList(prior Review) []previousFinding {
	list := []previousFinding{}
	for i, finding := range prior.Findings {
		if prior.Severities[i] != "must" && prior.Kinds[i] != "security" && prior.Kinds[i] != "data" && prior.Kinds[i] != "blocking" {
			continue
		}
		problem := ""
		if i < len(prior.FindingSpecs) {
			problem = prior.FindingSpecs[i].Problem
		}
		list = append(list, previousFinding{len(list), i, finding, prior.Kinds[i], prior.Severities[i], problem})
	}
	return list
}

func currentReview(raw json.RawMessage) Review { var r Review; _ = json.Unmarshal(raw, &r); return r }

func validPreviousFindingLinks(prior, current Review, comparable bool) map[int]bool {
	result := map[int]bool{}
	if !comparable {
		return result
	}
	list := previousFindingList(prior)
	indexes, currents := map[int]int{}, map[int]int{}
	for _, link := range current.PreviousFindings {
		indexes[link.Index]++
		if link.Current != nil {
			currents[*link.Current]++
		}
	}
	for _, link := range current.PreviousFindings {
		if link.Status != "unresolved" || link.Current == nil || link.Index < 0 || link.Index >= len(list) || indexes[link.Index] != 1 || currents[*link.Current] != 1 {
			continue
		}
		i := *link.Current
		if i >= 0 && i < len(current.Findings) && i < len(current.Severities) && current.Severities[i] == "must" {
			result[i] = true
		}
	}
	return result
}

type previousFindingReference struct {
	Index  int `json:"index"`
	Source int `json:"source"`
}

func previousFindingPrompt(r *journal.Record) string {
	prior, ok := comparableReview(firstJSON(r.Review, r.PreviousReview))
	refs := []previousFindingReference{}
	if ok {
		for _, item := range previousFindingList(prior) {
			refs = append(refs, previousFindingReference{item.Index, item.Source})
		}
	}
	return "前回の重大指摘一覧（データ）: " + string(encode(refs)) + "\n各項目のsourceは根拠(データ)のpreviousReview内の0始まりの位置です。その位置のfindingsが本文、findingKindsがkind、findingSeveritiesがseverity、findingSpecsがあればそのproblemが対応する問題です。indexをpreviousFindingsのindexに使用してください。各indexについて解消状況を返し、未解消ならstatusをunresolved、currentを今回のfindingsの0始まりの番号にし、解消済みならstatusをresolved、currentをnullにします。一覧が空なら空配列を返してください。\n"
}
