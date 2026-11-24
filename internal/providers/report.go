package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func concrete(s string) bool {
	value := strings.ToLower(strings.TrimSpace(s))
	switch value {
	case "", "なし", "無し", "特になし", "該当なし", "n/a", "none", "-":
		return false
	}
	return true
}
func ValidateReport(data []byte, remediation bool) error {
	var result struct {
		Status      string                                  `json:"status"`
		Assumptions []struct{ Assumption, Evidence string } `json:"assumptions"`
		Prevention  struct{ Kind, Detail string }           `json:"prevention"`
		Similar     struct {
			Searched, Handled, Guard string
			Found                    []string
		} `json:"similar"`
		SelfCheck []struct {
			Criterion, Source, Location, Test string
			Production                        struct{ Origin, Stored, Caller, Outlet string }
			SimilarLocations                  []struct{ Location, Result string }
			TestChanges                       struct {
				Changed bool
				Details string
			}
		} `json:"selfCheck"`
	}
	if json.Unmarshal(data, &result) != nil {
		return errors.New("実装の報告が不正です")
	}
	if result.Status != "completed" {
		return nil
	}
	var missing []string
	need := func(path, value string) {
		if !concrete(value) {
			missing = append(missing, path)
		}
	}
	if len(result.Assumptions) == 0 {
		missing = append(missing, "assumptions")
	}
	for i, a := range result.Assumptions {
		need(fmt.Sprintf("assumptions[%d].assumption", i), a.Assumption)
		need(fmt.Sprintf("assumptions[%d].evidence", i), a.Evidence)
	}
	assumptionEnd := len(missing)
	if remediation {
		need("prevention.detail", result.Prevention.Detail)
		need("similar.searched", result.Similar.Searched)
		need("similar.handled", result.Similar.Handled)
		need("similar.guard", result.Similar.Guard)
	}
	remediationEnd := len(missing)
	if len(result.SelfCheck) == 0 {
		missing = append(missing, "selfCheck")
	}
	for i, check := range result.SelfCheck {
		prefix := fmt.Sprintf("selfCheck[%d]", i)
		need(prefix+".criterion", check.Criterion)
		need(prefix+".location", check.Location)
		need(prefix+".source", check.Source)
		if check.Location != "対象外" {
			need(prefix+".test", check.Test)
		}
		need(prefix+".production.origin", check.Production.Origin)
		need(prefix+".production.stored", check.Production.Stored)
		need(prefix+".production.caller", check.Production.Caller)
		need(prefix+".production.outlet", check.Production.Outlet)
		need(prefix+".testChanges.details", check.TestChanges.Details)
		if len(check.SimilarLocations) == 0 {
			missing = append(missing, prefix+".similarLocations")
		}
		for j, same := range check.SimilarLocations {
			path := fmt.Sprintf("%s.similarLocations[%d]", prefix, j)
			need(path+".location", same.Location)
			need(path+".result", same.Result)
		}
	}
	if len(missing) > 0 {
		var groups []string
		if assumptionEnd > 0 {
			groups = append(groups, "前提と根拠: "+strings.Join(missing[:assumptionEnd], ", "))
		}
		if remediationEnd > assumptionEnd {
			groups = append(groups, "再発防止/同種箇所: "+strings.Join(missing[assumptionEnd:remediationEnd], ", "))
		}
		if len(missing) > remediationEnd {
			groups = append(groups, "自己点検: "+strings.Join(missing[remediationEnd:], ", "))
		}
		return fmt.Errorf("実装報告の具体的な内容が不足しています: %s", strings.Join(groups, "; "))
	}
	return nil
}
