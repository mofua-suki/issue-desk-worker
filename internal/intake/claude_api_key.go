package intake

import (
	"context"
	"errors"
	"net/url"
)

// ClaudeAPIKey is the Claude API key state the intake distributes to this PC.
type ClaudeAPIKey struct {
	State   string  `json:"state"`
	Version *string `json:"version"`
	Key     *string `json:"key"`
}

// ClaudeAPIKey reads this PC's key with the PC token. Only the shapes in the
// API contract are accepted; errors never contain the response body.
func (c *Client) ClaudeAPIKey(ctx context.Context) (ClaudeAPIKey, error) {
	var out ClaudeAPIKey
	if err := c.call(ctx, "GET", "/api/v1/hosts/"+url.PathEscape(c.host)+"/claude-api-key", nil, &out); err != nil {
		return ClaudeAPIKey{}, err
	}
	switch {
	case out.State == "registered" && out.Version != nil && out.Key != nil:
	case out.State == "unregistered" && out.Version == nil && out.Key == nil:
	case out.State == "cleared" && out.Version != nil && out.Key == nil:
	default:
		return ClaudeAPIKey{}, errors.New("受信したClaude APIキーの配布応答が不正です")
	}
	return out, nil
}
