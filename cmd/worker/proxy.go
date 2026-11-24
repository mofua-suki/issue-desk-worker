package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Keep this entrypoint until every PC has replaced its old compose file.
func webhookProxy(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runWebhookProxy(ctx, args, net.Listen)
}

func newProxyTransport(dialer *net.Dialer) *http.Transport {
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}
}

func newProxyDialer() *net.Dialer { return &net.Dialer{Timeout: 30 * time.Second} }

func newProxyServer(transport http.RoundTripper) *http.Server {
	target := &url.URL{Scheme: "http", Host: "issue-desk-client:8790"}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}
	return &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16384,
	}
}

func runWebhookProxy(ctx context.Context, args []string, listen func(string, string) (net.Listener, error)) error {
	flags := flag.NewFlagSet("webhook-proxy", flag.ContinueOnError)
	flags.String("colors-dir", "", "互換用（未使用）")
	address := flags.String("listen", ":8790", "待受アドレス")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("webhook-proxy: 不明な引数")
	}
	listener, err := listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := newProxyServer(newProxyTransport(newProxyDialer()))
	done := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownCtx)
		case <-done:
		}
	}()
	err = server.Serve(listener)
	close(done)
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
