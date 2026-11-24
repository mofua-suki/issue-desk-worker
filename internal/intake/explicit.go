package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/cause"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const MaxRequestBytes = 1 << 20

type Response struct {
	Status int
	Body   []byte
}

// ValidateTarget accepts only API paths on the configured origin, never a complete URL.
// Authentication and operation permissions remain the responsibility of the server.
func ValidateTarget(target string) error {
	u, err := url.ParseRequestURI(target)
	if err != nil || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || u.IsAbs() || u.Host != "" || u.Fragment != "" || strings.ContainsAny(target, "#\\\r\n") {
		return cause.New("接続先は同じServerのAPIパスで指定してください", err)
	}
	if !strings.HasPrefix(u.Path, "/api/") || path.Clean(u.Path) != u.Path || strings.ContainsRune(u.Path, '\\') {
		return errors.New("接続先は正規化された/api/配下のパスにしてください")
	}
	return nil
}

// Explicit sends exactly one caller-specified request. It never creates or rewrites send IDs,
// retries a mutation, follows redirects, or falls back to a screen session or one-use key.
func (c *Client) Explicit(ctx context.Context, method, target string, body json.RawMessage) (Response, error) {
	if err := ValidateTarget(target); err != nil {
		return Response{}, err
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return Response{}, errors.New("利用できないHTTPメソッドです")
	}
	if (method == http.MethodGet || method == http.MethodHead) && len(body) != 0 {
		return Response{}, errors.New("読取要求に本文は付けられません")
	}
	if len(body) > MaxRequestBytes {
		return Response{}, errors.New("要求本文が上限を超えました")
	}
	if len(body) > 0 && !json.Valid(body) {
		return Response{}, errors.New("要求本文のJSONが不正です")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+target, bytes.NewReader(body))
	if err != nil {
		return Response{}, cause.New("受付APIの要求を作れません", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(ContractHeader, ContractVersion)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("受付APIの応答を受け取れませんでした。この要求を自動再送していません: %w", transportFailure(ctx, err, callName(method, target)))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return Response{Status: res.StatusCode}, cause.New("受付APIの応答本文を読めませんでした。この要求を自動再送していません", err)
	}
	if len(data) > maxResponseBytes {
		return Response{Status: res.StatusCode}, errors.New("受付APIの応答本文が上限を超えました")
	}
	return Response{Status: res.StatusCode, Body: data}, nil
}
