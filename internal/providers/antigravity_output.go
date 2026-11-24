package providers

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var fencedJSONPattern = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

func antigravityOutput(raw string) map[string]json.RawMessage {
	text := strings.TrimSpace(raw)
	candidates := []string{text}
	lines := strings.Split(text, "\n")
	if len(lines) > 1 {
		candidates = append(candidates, lines[len(lines)-1])
	}
	for _, candidate := range candidates {
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(candidate), &object) == nil && object != nil {
			return object
		}
		var array []json.RawMessage
		if json.Unmarshal([]byte(candidate), &array) == nil {
			for n := len(array) - 1; n >= 0; n-- {
				if json.Unmarshal(array[n], &object) == nil && object != nil {
					return object
				}
			}
		}
	}
	return nil
}
func antigravityResult(root map[string]json.RawMessage) json.RawMessage {
	valid := func(raw []byte) json.RawMessage {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return nil
		}
		var status string
		_ = json.Unmarshal(obj["status"], &status)
		if status != "completed" && status != "question" && status != "failed" {
			return nil
		}
		keys := []string{"status", "incomplete", "prerequisiteMissing", "summary", "questions", "unverified", "unverifiedKinds", "tests", "followupIssues", "acceptanceTransfers", "selfCheck", "prevention", "assumptions", "similar"}
		out := map[string]json.RawMessage{}
		for _, key := range keys {
			if value, ok := obj[key]; ok {
				out[key] = value
			}
		}
		if _, ok := out["followupIssues"]; !ok {
			out["followupIssues"] = json.RawMessage(`[]`)
		}
		raw, _ = json.Marshal(out)
		return raw
	}
	if raw := valid(root["structured_output"]); raw != nil {
		return raw
	}
	if _, ok := root["summary"]; ok {
		raw, _ := json.Marshal(root)
		if result := valid(raw); result != nil {
			return result
		}
	}
	for _, key := range []string{"result", "response", "text", "output"} {
		raw, ok := root[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) != nil {
			break
		}
		candidates := []string{strings.TrimSpace(text)}
		fenced := fencedJSONPattern.FindAllStringSubmatch(text, -1)
		for n := len(fenced) - 1; n >= 0; n-- {
			candidates = append(candidates, fenced[n][1])
		}
		begin, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
		if begin >= 0 && end > begin {
			candidates = append(candidates, text[begin:end+1])
		}
		for _, candidate := range candidates {
			if result := valid([]byte(candidate)); result != nil {
				return result
			}
		}
		break
	}
	return nil
}
func decodeAntigravity(raw string, answer *Answer) (json.RawMessage, error) {
	root := antigravityOutput(raw)
	if root == nil {
		return nil, errors.New("AntigravityのJSON応答が不正です")
	}
	antigravityMetadata(root, answer)
	result := antigravityResult(root)
	if result == nil {
		return nil, errors.New("Antigravityの構造化回答を確認できません")
	}
	return result, nil
}

func antigravityMetadata(root map[string]json.RawMessage, answer *Answer) {
	for _, key := range []string{"conversation_id", "conversationId", "conversation", "session_id", "sessionId"} {
		if value, ok := root[key]; ok && string(value) != "null" {
			var s string
			if json.Unmarshal(value, &s) == nil && sessionPattern.MatchString(s) {
				answer.Session = s
			}
			break
		}
	}
	for _, key := range []string{"usage", "usageMetadata"} {
		if value, ok := root[key]; ok && string(value) != "null" {
			answer.Usage = readUsage(value, "antigravity")
			break
		}
	}

	for _, key := range []string{"denied_actions", "deniedActions"} {
		if raw, ok := root[key]; ok && string(raw) != "null" {
			_ = json.Unmarshal(raw, &answer.Denied)
			break
		}
	}
}
