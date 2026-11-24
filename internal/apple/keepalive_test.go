package apple

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// 長い処理の間もheartbeatを送り、見張りの無いときはheartbeatの失敗で処理を止めない。
func TestKeepAliveBeatsDuringLongWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var beats atomic.Int32
		a := appleRoundRunner(t, func(r *http.Request) *http.Response {
			if strings.HasPrefix(r.URL.Path, "/api/v1/hosts/") {
				beats.Add(1)
				return nativeResponse(503, map[string]any{})
			}
			return nativeResponse(200, map[string]any{})
		})
		ctx, stop := a.keepAlive(context.Background(), false, nil)
		time.Sleep(26 * time.Second)
		if ctx.Err() != nil {
			t.Fatal("heartbeatの失敗で処理を止めました")
		}
		stop()
		if beats.Load() < 5 {
			t.Fatalf("処理の間にheartbeatを送っていません: %d", beats.Load())
		}
	})
}

// 見張りがあるときは、heartbeatか見張りが失敗したら処理を止める。
func TestKeepAliveStopsWhenWatchOrHeartbeatFails(t *testing.T) {
	for name, heartbeat := range map[string]int{"watch": 200, "heartbeat": 503} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := appleRoundRunner(t, func(r *http.Request) *http.Response { return nativeResponse(heartbeat, map[string]any{}) })
				ctx, stop := a.keepAlive(context.Background(), true, func(context.Context) error {
					if heartbeat == 200 {
						return errNativeClaimLost
					}
					return nil
				})
				defer stop()
				time.Sleep(6 * time.Second)
				if ctx.Err() == nil {
					t.Fatal("失敗で止めていません")
				}
			})
		})
	}
}
