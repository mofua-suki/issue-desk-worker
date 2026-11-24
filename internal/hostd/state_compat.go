package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"strings"
	"time"
)

type updateAlias UpdateState

var stateTimeKeys = []string{"started", "deadline", "until", "lazySince", "updateStarted", "readyDeadline"}

func (s *UpdateState) UnmarshalJSON(raw []byte) error {
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return errors.New("更新状態のJSONが不正です")
	}
	for _, key := range stateTimeKeys {
		value, exists := values[key]
		if !exists {
			continue
		}
		var number json.Number
		if len(value) > 0 && value[0] != '"' && string(value) != "null" {
			if json.Unmarshal(value, &number) != nil {
				return errors.New("更新状態の時刻が不正です")
			}
			millis, err := number.Int64()
			if err != nil {
				return cause.New("更新状態の時刻が不正です", err)
			}
			values[key] = encode(time.UnixMilli(millis).UTC())
		}
	}
	normalized, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return json.Unmarshal(normalized, (*updateAlias)(s))
}
func (s UpdateState) MarshalJSON() ([]byte, error) {
	raw, err := json.Marshal(updateAlias(s))
	if err != nil {
		return nil, err
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil, errors.New("更新状態を保存できません")
	}
	times := []time.Time{s.Started, s.Deadline, s.Until, s.LazySince, s.UpdateStarted, s.ReadyDeadline}
	for i, key := range stateTimeKeys {
		if times[i].IsZero() {
			delete(values, key)
		} else {
			values[key] = encode(times[i].UTC())
		}
	}
	return json.Marshal(values)
}
func (r *Runtime) completeTarget(ctx context.Context, target Target) (Target, error) {
	image, err := r.composeImage(ctx, r.Config.ClientDir+"/.env", r.Config.ClientDir+"/compose.yaml")
	if err != nil {
		return target, err
	}
	colon := strings.LastIndex(image, ":")
	slash := strings.LastIndex(image, "/")
	if colon < slash || slash < 0 {
		return target, errors.New("復旧用image名が不正です")
	}
	repository := image[:colon]
	expectedSuffix := "/issue-desk-client" + target.Flavor
	if !strings.HasSuffix(repository, expectedSuffix) {
		return target, errors.New("復旧用imageの種別が異なります")
	}
	target.Registry = strings.TrimSuffix(repository, expectedSuffix)
	digests, err := r.imageDigests(ctx, repository+":"+target.Tag)
	if err != nil {
		return target, err
	}
	for _, digest := range digests {
		if strings.HasPrefix(digest, repository+"@") {
			target.Digest = strings.TrimPrefix(digest, repository+"@")
			return target, nil
		}
	}
	return target, fmt.Errorf("復旧用digestがありません")
}
