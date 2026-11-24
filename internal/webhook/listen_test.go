package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// Listen は既定で127.0.0.1に受信口を開き、Close で閉じる。portが不正・使用中なら起動しない。
func TestListenAndClose(t *testing.T) {
	hash := sha256.Sum256([]byte("Bearer allowed"))
	r, err := New(hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("起動前の Close: %v", err)
	}
	for _, port := range []int{0, 65536} {
		if err := r.Listen(Configuration{Port: port}); err == nil {
			t.Fatalf("不正なport %dで起動しました", port)
		}
	}
	port := freePort(t)
	if err := r.Listen(Configuration{Port: port}); err != nil {
		t.Fatal(err)
	}
	// 閉じた後の確認が前の接続を使い回さないよう、接続を保たない。
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	url := "http://127.0.0.1:" + strconv.Itoa(port) + Path
	req, _ := http.NewRequest("POST", url, nil)
	req.Header.Set("Authorization", "Bearer allowed")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatalf("受信口の応答: %d", res.StatusCode)
	}
	select {
	case <-r.Wake():
	case <-time.After(5 * time.Second):
		t.Fatal("起こしていません")
	}
	other, err := New(hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Listen(Configuration{Port: port}); err == nil {
		other.Close()
		t.Fatal("使用中のportで起動しました")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := client.Post(url, "application/json", nil); err == nil {
		res.Body.Close()
		t.Fatal("閉じた後も受け付けています")
	}
}
