package intake

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// A real listener accepts the request and disappears before responding, as a
// server replacement can do. Inspect the error before applying the classifier.
func TestReplacementConnectionFailureClassification(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, e := listener.Accept()
		if e == nil {
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			_ = conn.Close()
		}
	}()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	_, err = client.Get("http://" + listener.Addr().String())
	<-done
	if err == nil {
		t.Fatal("expected interrupted response")
	}
	var op *net.OpError
	t.Logf("observed replacement error: %T, EOF=%v, net.OpError=%v", err, errors.Is(err, io.EOF), errors.As(err, &op))
	classified := transportFailure(context.Background(), err, "")
	if TemporaryFailureReason(classified) != "connection_failed" {
		t.Fatalf("not temporary: %v", classified)
	}
	if TemporaryFailureReason(context.Canceled) != "" {
		t.Fatal("cancellation treated as temporary")
	}
}
