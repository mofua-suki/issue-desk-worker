package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type proxyTransport func(*http.Request) (*http.Response, error)

func (f proxyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLegacyWebhookProxyEntrypoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/webhooks/issue-desk?source=legacy" || r.Header.Get("Authorization") != "Bearer test" {
			t.Errorf("request not preserved: %s %s %q", r.Method, r.URL, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	transport := proxyTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "issue-desk-client:8790" {
			t.Errorf("unexpected target: %s", r.URL.Host)
		}
		r.URL.Host = strings.TrimPrefix(upstream.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})
	server := httptest.NewServer(newProxyServer(transport).Handler)
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/webhooks/issue-desk?source=legacy", strings.NewReader("body"))
	request.Header.Set("Authorization", "Bearer test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status: %s", response.Status)
	}
}

func TestLegacyProxyLimits(t *testing.T) {
	dialer := newProxyDialer()
	transport := newProxyTransport(dialer)
	server := newProxyServer(transport)
	if dialer.Timeout != 30*time.Second || transport.ResponseHeaderTimeout != 30*time.Second || transport.IdleConnTimeout != 30*time.Second {
		t.Fatalf("unexpected transport limits: dial=%v response=%v idle=%v", dialer.Timeout, transport.ResponseHeaderTimeout, transport.IdleConnTimeout)
	}
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 30*time.Second || server.WriteTimeout != 30*time.Second || server.IdleTimeout != 30*time.Second || server.MaxHeaderBytes != 16384 {
		t.Fatalf("unexpected server limits: %+v", server)
	}
}

func TestLegacyProxyFlagsAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- runWebhookProxy(ctx, []string{"--colors-dir", "x", "--listen", ":0"}, func(network, address string) (net.Listener, error) {
			if network != "tcp" || address != ":0" {
				t.Errorf("unexpected listen: %s %s", network, address)
			}
			l, err := net.Listen(network, address)
			if err == nil {
				ready <- l.Addr()
			}
			return l, err
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("startup failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup timed out")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
	if err := runWebhookProxy(context.Background(), []string{"--unknown"}, net.Listen); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

// 内部の転送は環境変数のプロキシを使わない。HTTP_PROXY のあるコンテナでも Webhook を 502 にしないため。
func TestProxyTransportIgnoresEnvironmentProxy(t *testing.T) {
	if newProxyTransport(newProxyDialer()).Proxy != nil {
		t.Fatal("転送は環境変数のプロキシを使いません")
	}
}
