package webhook

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Path = "/webhooks/issue-desk"

type Configuration struct {
	Bind        string `json:"bind"`
	Port        int    `json:"port"`
	TokenSHA256 string `json:"tokenSha256"`
	TokenFile   string `json:"tokenFile"`
	Managed     bool   `json:"managed"`
}
type Receiver struct {
	mu       sync.RWMutex
	verifier []byte
	wake     chan struct{}
	server   *http.Server
	listener net.Listener
}

func New(verifier string) (*Receiver, error) {
	r := &Receiver{wake: make(chan struct{}, 1)}
	return r, r.SetVerifier(verifier)
}
func (r *Receiver) SetVerifier(verifier string) error {
	var hash []byte
	if verifier != "" {
		if len(verifier) != 64 || strings.ToLower(verifier) != verifier {
			return errors.New("Webhookの検証値が不正です")
		}
		var err error
		hash, err = hex.DecodeString(verifier)
		if err != nil {
			return cause.New("Webhookの検証値が不正です", err)
		}
	}
	r.mu.Lock()
	r.verifier = hash
	r.mu.Unlock()
	return nil
}
func (r *Receiver) Wake() <-chan struct{} { return r.wake }
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Connection", "close")
	if req.Method != "POST" || req.URL.RequestURI() != Path {
		w.WriteHeader(404)
		return
	}
	hash := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	r.mu.RLock()
	valid := len(r.verifier) == 32 && subtle.ConstantTimeCompare(hash[:], r.verifier) == 1
	r.mu.RUnlock()
	if !valid {
		w.WriteHeader(401)
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
	w.WriteHeader(202)
}
func (r *Receiver) Listen(c Configuration) error {
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("Webhookのportが不正です")
	}
	bind := c.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	address := net.JoinHostPort(bind, strconv.Itoa(c.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("指定されたWebhookの受信口（%s）を起動できません: %w", address, err)
	}
	r.listener = listener
	r.server = &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16384}
	server := r.server
	go func() {
		// Serve ends with ErrServerClosed after Close; any other end is recorded.
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "Webhookの受信口（%s）が止まりました: %v\n", address, err)
		}
	}()
	return nil
}
func (r *Receiver) Close() error {
	if r.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.server.Shutdown(ctx)
}
