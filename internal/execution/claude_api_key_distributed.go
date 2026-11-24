package execution

import (
	"context"
	"sync"
	"time"
)

const (
	distributedKeyRefresh = 5 * time.Minute
	distributedKeyRetry   = time.Minute
	distributedKeyTimeout = 5 * time.Second
)

// distributedKeyCache keeps the intake-distributed key in memory only.
type distributedKeyCache struct {
	mu       sync.Mutex
	key      string
	next     time.Time
	fetching bool
}

// distributedClaudeAPIKey returns the key the intake distributes to this PC.
// Read failures, unregistered and cleared keys all mean no key, so a lost
// connection never keeps using a key that may have been cleared.
func (e *Executor) distributedClaudeAPIKey() string {
	if e.API == nil {
		return ""
	}
	c := &e.distributedKey
	c.mu.Lock()
	now := time.Now()
	// Only one caller reads the intake; the scheduler and heartbeat never wait
	// on another caller's request and use the current value instead.
	if now.Before(c.next) || c.fetching {
		key := c.key
		c.mu.Unlock()
		return key
	}
	c.fetching = true
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), distributedKeyTimeout)
	got, err := e.API.ClaudeAPIKey(ctx)
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetching = false
	c.key, c.next = "", now.Add(distributedKeyRefresh)
	if err != nil {
		c.next = now.Add(distributedKeyRetry)
		return ""
	}
	if got.State == "registered" && len(*got.Key) <= 4096 && claudeAPIKeyPattern.MatchString(*got.Key) {
		c.key = *got.Key
	}
	return c.key
}
