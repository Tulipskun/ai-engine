package sdk

import (
	"errors"
	"net/http"
	"strings"
	"sync"
)

type KeyPool struct {
	mu      sync.Mutex
	keys    []string
	current int
}

func NewKeyPool(keys ...string) *KeyPool {
	cleaned := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			cleaned = append(cleaned, k)
		}
	}
	return &KeyPool{keys: cleaned}
}
func (p *KeyPool) Len() int                 { p.mu.Lock(); defer p.mu.Unlock(); return len(p.keys) }
func (p *KeyPool) Current() (string, error) { return p.At(p.IndexOfCurrent()) }
func (p *KeyPool) At(index int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return "", errors.New("sdk: no API keys configured")
	}
	if index < 0 || index >= len(p.keys) {
		return "", errors.New("sdk: API key index out of range")
	}
	return p.keys[index], nil
}
func (p *KeyPool) IndexOfCurrent() int { p.mu.Lock(); defer p.mu.Unlock(); return p.current }
func (p *KeyPool) Rotate() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return "", errors.New("sdk: no API keys configured")
	}
	p.current = (p.current + 1) % len(p.keys)
	return p.keys[p.current], nil
}

// Provider errors are retryable by default, including provider-specific 400s.
// A 404 is the explicit non-retryable HTTP error.
func RetryableHTTPStatus(status int) bool { return status != http.StatusNotFound }
