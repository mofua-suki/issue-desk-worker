package execution

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
)

// StopCommands は進行中の確認を取り消し、確認の処理が終わるまで待つ。止めた後は受付へ問い合わせない。
func TestStopCommandsEndsPollingAndWaitsForInFlightPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var polls atomic.Int32
		entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		transport := nativeTransport(func(r *http.Request) (*http.Response, error) {
			if polls.Add(1) == 1 {
				close(entered)
				<-r.Context().Done()
				close(cancelled)
				<-release
			}
			return nativeResponse(http.StatusOK, map[string]any{"commands": []any{}, "next": 0}), nil
		})
		api, err := intake.New(configBootstrap("https://example.test"), "idk_"+strings.Repeat("a", 43), transport)
		if err != nil {
			t.Fatal(err)
		}
		e := &Executor{API: api, Config: Configuration{Host: "fixture"}}
		e.StartCommands(context.Background())
		e.StartCommands(context.Background())
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("命令を確認していません")
		}
		if got := polls.Load(); got != 1 {
			t.Errorf("取得回数 = %d、期待値 1", got)
		}
		stopped := make(chan struct{})
		go func() { e.StopCommands(); close(stopped) }()
		<-cancelled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("取得処理の解放前に停止が戻りました")
		default:
		}
		close(release)
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("取得処理の解放後に停止が戻りません")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if got := polls.Load(); got != 1 {
			t.Errorf("停止後の取得回数 = %d、期待値 1", got)
		}
		e.StopCommands()
	})
}
