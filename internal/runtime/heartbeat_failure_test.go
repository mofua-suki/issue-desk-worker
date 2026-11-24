package runtime

import (
	"context"
	"errors"
	"issue-desk-worker/internal/failurelog"
	"strings"
	"testing"
	"time"
)

// 裏のheartbeatの失敗を捨てず、同じ失敗が続く間は間引いて知らせる。
func TestHeartbeatFailuresAreReportedAndThinned(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	var reported []string
	r := &Runner{Now: func() time.Time { return now }, OnHeartbeatError: func(err error) { reported = append(reported, err.Error()) }}
	f := newHeartbeatFailures(r)
	ctx := context.Background()
	down := errors.New("受付APIへ接続できません")
	f.observe(ctx, r, down)
	for range 3 {
		now = now.Add(time.Minute)
		f.observe(ctx, r, down)
	}
	if len(reported) != 1 {
		t.Fatalf("続く同じ失敗を間引いていません: %v", reported)
	}
	now = now.Add(failurelog.Repeat)
	f.observe(ctx, r, down)
	if len(reported) != 2 || !strings.Contains(reported[1], "同じ失敗が4回") {
		t.Fatalf("間隔を空けた再報告: %v", reported)
	}
	f.observe(ctx, r, errors.New("受付APIが拒否しました"))
	if len(reported) != 3 {
		t.Fatalf("違う失敗をすぐ知らせていません: %v", reported)
	}
	f.observe(ctx, r, nil)
	f.observe(ctx, r, down)
	if len(reported) != 4 {
		t.Fatalf("回復後の失敗をすぐ知らせていません: %v", reported)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	f.observe(cancelled, r, errors.New("context canceled"))
	if len(reported) != 4 {
		t.Fatalf("停止中の失敗を知らせています: %v", reported)
	}
}
