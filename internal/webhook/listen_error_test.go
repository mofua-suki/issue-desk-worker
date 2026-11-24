package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// 受信口を開けないときは、場所と元の失敗を示す。
func TestListenErrorNamesAddress(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	hash := sha256.Sum256([]byte("Bearer allowed"))
	r, err := New(hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	err = r.Listen(Configuration{Port: port})
	if err == nil {
		r.Close()
		t.Fatal("使用中のportで起動しました")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:"+strconv.Itoa(port)) || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("場所と理由がありません: %v", err)
	}
}
