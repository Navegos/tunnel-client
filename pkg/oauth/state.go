package oauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go.openai.org/api/tunnel-client/pkg/types"
)

// DefaultDiscoveryTimeout bounds how long we wait for OAuth metadata discovery.
const DefaultDiscoveryTimeout = 5 * time.Second

// DiscoveryResult captures OAuth metadata returned by the MCP server.
type DiscoveryResult struct {
	URL        string          `json:"url,omitempty"`
	FetchedAt  time.Time       `json:"fetched_at,omitempty"`
	StatusCode int             `json:"status_code,omitempty"`
	Headers    http.Header     `json:"headers,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
	BodyText   string          `json:"body_text,omitempty"`
}

// DiscoveryState tracks the result of a background OAuth metadata fetch.
type DiscoveryState struct {
	done   chan struct{}
	mu     sync.Mutex
	result *DiscoveryResult
	err    error
	once   sync.Once
}

// NewDiscoveryState constructs a DiscoveryState ready for updates.
func NewDiscoveryState() *DiscoveryState {
	return &DiscoveryState{done: make(chan struct{})}
}

// Set records the OAuth discovery result and signals waiters.
func (s *DiscoveryState) Set(result *DiscoveryResult, err error) {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.result = result
		s.err = err
		close(s.done)
	})
}

// Wait blocks until the OAuth metadata is available or the timeout elapses.
func (s *DiscoveryState) Wait(timeout time.Duration) (*DiscoveryResult, error, bool) {
	if s == nil {
		return nil, nil, false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, s.err, true
	case <-timer.C:
		return nil, nil, false
	}
}

// BuildDiscoveryResult converts the tunnel response into a UI-friendly payload.
func BuildDiscoveryResult(resp *types.TunnelResponse, sourceURL *url.URL, fetchedAt time.Time) *DiscoveryResult {
	if resp == nil {
		return nil
	}

	result := &DiscoveryResult{
		FetchedAt:  fetchedAt,
		StatusCode: resp.ResponseCode(),
		Headers:    resp.Headers(),
	}
	if sourceURL != nil {
		result.URL = sourceURL.String()
	}

	payload := resp.Payload()
	if json.Valid(payload) {
		result.Body = payload
	} else if len(payload) > 0 {
		result.BodyText = string(payload)
	}

	return result
}
