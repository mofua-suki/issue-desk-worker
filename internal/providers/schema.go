package providers

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

//go:embed schemas/*.json
var assets embed.FS

// Schema は、AIへ渡す回答契約。OpenAIの構造化出力は、propertiesの全キーがrequiredに入っていることを求める。
func Schema(review bool) []byte { return build(review, true) }

// 旧レビューの受信時だけ省略を許す欄。送信用の契約では必須のままにする。
var optionalReviewReceiveFields = map[string]bool{"recordOnly": true, "previousFindings": true, "interrupts": true}

// build の strict が偽のときは、旧形式との互換性のため追加項目を省略できる。
func build(review, strict bool) []byte {
	name := "result.schema.json"
	if review {
		name = "review.schema.json"
	}
	data, _ := assets.ReadFile("schemas/" + name)
	if review && !strict {
		var schema map[string]any
		_ = json.Unmarshal(data, &schema)
		required := schema["required"].([]any)
		filtered := make([]any, 0, len(required))
		for _, key := range required {
			if !optionalReviewReceiveFields[key.(string)] {
				filtered = append(filtered, key)
			}
		}
		schema["required"] = filtered
		finding := schema["properties"].(map[string]any)["findingSpecs"].(map[string]any)["items"].(map[string]any)
		finding["required"] = []string{"group", "problem", "expected", "test"}
		data, _ = json.Marshal(schema)
	} else if !review {
		var schema map[string]any
		_ = json.Unmarshal(data, &schema)
		properties := schema["properties"].(map[string]any)
		properties["deleteFiles"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		properties["addDependencies"] = map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "version", "dev"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"}, "dev": map[string]any{"type": "boolean"}}}}
		if !strict {
			required := schema["required"].([]any)
			filtered := make([]any, 0, len(required))
			for _, key := range required {
				if key != "scopeExtensions" {
					filtered = append(filtered, key)
				}
			}
			schema["required"] = filtered
		}
		if strict {
			properties["assumptions"].(map[string]any)["minItems"] = 1
			required, _ := schema["required"].([]any)
			schema["required"] = append(required, "deleteFiles", "addDependencies")
		}
		data, _ = json.Marshal(schema)
	}
	return data
}

func Validate(data []byte, review bool) error {
	var schema map[string]any
	if err := json.Unmarshal(build(review, false), &schema); err != nil {
		return err
	}
	if !json.Valid(data) {
		return errors.New("AIの回答が単一のJSONではありません")
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return cause.New("AIの回答がJSONではありません", err)
	}
	return validate(schema, value, "result")
}
func validate(s map[string]any, v any, where string) error {
	types := []string{}
	switch t := s["type"].(type) {
	case string:
		types = append(types, t)
	case []any:
		for _, x := range t {
			types = append(types, x.(string))
		}
	}
	if len(types) > 0 {
		ok := false
		for _, kind := range types {
			switch kind {
			case "null":
				ok = ok || v == nil
			case "object":
				_, yes := v.(map[string]any)
				ok = ok || yes
			case "array":
				_, yes := v.([]any)
				ok = ok || yes
			case "string":
				_, yes := v.(string)
				ok = ok || yes
			case "boolean":
				_, yes := v.(bool)
				ok = ok || yes
			case "number", "integer":
				n, yes := v.(json.Number)
				if yes {
					f, err := n.Float64()
					ok = ok || (err == nil && (kind != "integer" || f == math.Trunc(f)))
				}
			}
		}
		if !ok {
			return fmt.Errorf("AI回答の型が違います: %s", where)
		}
	}
	if choices, ok := s["enum"].([]any); ok {
		yes := false
		for _, x := range choices {
			if fmt.Sprint(x) == fmt.Sprint(v) {
				yes = true
			}
		}
		if !yes {
			return fmt.Errorf("AI回答の値が契約外です: %s", where)
		}
	}
	if v == nil {
		return nil
	}
	switch value := v.(type) {
	case map[string]any:
		if required, ok := s["required"].([]any); ok {
			for _, raw := range required {
				key := raw.(string)
				if _, exists := value[key]; !exists {
					return fmt.Errorf("AI回答の必須項目がありません: %s.%s", where, key)
				}
			}
		}
		properties, _ := s["properties"].(map[string]any)
		for key, item := range value {
			if spec, ok := properties[key].(map[string]any); ok {
				if err := validate(spec, item, where+"."+key); err != nil {
					return err
				}
			} else if s["additionalProperties"] == false {
				return fmt.Errorf("AI回答に契約外の項目があります: %s", where)
			}
		}
	case []any:
		if n, ok := s["minItems"].(float64); ok && len(value) < int(n) {
			return errors.New("AI回答の配列が不足しています")
		}
		if n, ok := s["maxItems"].(float64); ok && len(value) > int(n) {
			return errors.New("AI回答の配列が上限を超えています")
		}
		if spec, ok := s["items"].(map[string]any); ok {
			for _, item := range value {
				if err := validate(spec, item, where+"[]"); err != nil {
					return err
				}
			}
		}
	case string:
		size := utf8.RuneCountInString(value)
		if n, ok := s["minLength"].(float64); ok && size < int(n) {
			return errors.New("AI回答の文字数が不足しています")
		}
		if n, ok := s["maxLength"].(float64); ok && size > int(n) {
			return errors.New("AI回答の文字数が上限を超えています")
		}
		if pattern, ok := s["pattern"].(string); ok {
			match, err := regexp.MatchString(pattern, value)
			if err != nil || !match {
				return cause.New("AI回答の文字列形式が不正です", err)
			}
		}
	case json.Number:
		n, _ := value.Float64()
		if min, ok := s["minimum"].(float64); ok && n < min {
			return errors.New("AI回答の数値が下限未満です")
		}
		if max, ok := s["maximum"].(float64); ok && n > max {
			return errors.New("AI回答の数値が上限を超えています")
		}
	}
	return nil
}
