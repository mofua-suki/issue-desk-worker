package providers

import (
	"bytes"
	"encoding/json"
	"strings"
)

type redactedCallError struct {
	err     error
	message string
}

func (e *redactedCallError) Error() string { return e.message }
func (e *redactedCallError) Unwrap() error { return e.err }

// redactCallResult handles decoded JSON strings as well as escaped spellings of the key.
func redactCallResult(raw json.RawMessage, err error, key string) (json.RawMessage, error) {
	if err != nil && strings.Contains(err.Error(), key) {
		err = &redactedCallError{err: err, message: strings.ReplaceAll(err.Error(), key, "[redacted]")}
	}
	if len(raw) == 0 {
		return raw, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if dec.Decode(&value) != nil {
		return nil, err
	}
	var redact func(any) any
	redact = func(v any) any {
		switch x := v.(type) {
		case string:
			return strings.ReplaceAll(x, key, "[redacted]")
		case []any:
			for i := range x {
				x[i] = redact(x[i])
			}
			return x
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, v := range x {
				out[strings.ReplaceAll(k, key, "[redacted]")] = redact(v)
			}
			return out
		default:
			return v
		}
	}
	result, marshalErr := json.Marshal(redact(value))
	if marshalErr != nil {
		return nil, err
	}
	return result, err
}
