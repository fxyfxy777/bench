package policy

import (
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Counter tracks the number of concurrent requests for an instance.
type Counter struct {
	count atomic.Uint64
}

func (c *Counter) Inc() {
	c.count.Add(1)
}

func (c *Counter) Dec() bool {
	for {
		old := c.count.Load()
		if old == 0 {
			return false
		}
		if c.count.CompareAndSwap(old, old-1) {
			return true
		}
	}
}

func (c *Counter) Get() uint64 {
	return c.count.Load()
}

// TokenCounter tracks the number of tokens currently being processed by a Prefill instance.
type TokenCounter struct {
	tokens atomic.Uint64
}

func (c *TokenCounter) Add(n uint64) {
	c.tokens.Add(n)
}

func (c *TokenCounter) Get() uint64 {
	return c.tokens.Load()
}

func (c *TokenCounter) Sub(n uint64) {
	if n == 0 {
		return
	}
	for {
		old := c.tokens.Load()
		if old == 0 {
			return
		}
		var newVal uint64
		if old <= n {
			newVal = 0
		} else {
			newVal = old - n
		}
		if c.tokens.CompareAndSwap(old, newVal) {
			return
		}
	}
}

// CounterManager manages request counters and token counters for all instances.
type CounterManager struct {
	mu         sync.RWMutex
	counterMap map[string]*Counter
	tokenMap   map[string]*TokenCounter
}

// NewCounterManager creates a new CounterManager.
func NewCounterManager() *CounterManager {
	return &CounterManager{
		counterMap: make(map[string]*Counter),
		tokenMap:   make(map[string]*TokenCounter),
	}
}

// GetOrCreateCounter retrieves or creates a request counter for the instance.
func (m *CounterManager) GetOrCreateCounter(instanceID string) *Counter {
	m.mu.RLock()
	if c, ok := m.counterMap[instanceID]; ok {
		m.mu.RUnlock()
		return c
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.counterMap[instanceID]; ok {
		return c
	}
	c := &Counter{}
	m.counterMap[instanceID] = c
	return c
}

// GetCounter retrieves a request counter without creating one.
func (m *CounterManager) GetCounter(instanceID string) (*Counter, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.counterMap[instanceID]
	return c, ok
}

// GetOrCreateTokenCounter retrieves or creates a token counter for the instance.
func (m *CounterManager) GetOrCreateTokenCounter(instanceID string) *TokenCounter {
	m.mu.RLock()
	if c, ok := m.tokenMap[instanceID]; ok {
		m.mu.RUnlock()
		return c
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.tokenMap[instanceID]; ok {
		return c
	}
	c := &TokenCounter{}
	m.tokenMap[instanceID] = c
	return c
}

// GetTokenCounter retrieves a token counter without creating one.
func (m *CounterManager) GetTokenCounter(instanceID string) (*TokenCounter, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.tokenMap[instanceID]
	return c, ok
}

// Release decreases the request counter for the given instance.
func (m *CounterManager) Release(instanceID string) {
	c, ok := m.GetCounter(instanceID)
	if !ok {
		return
	}
	c.Dec()
}

// ReleasePrefillTokens releases the token load for a prefill instance.
func (m *CounterManager) ReleasePrefillTokens(instanceID string, message string) {
	if instanceID == "" || message == "" {
		return
	}
	tc, ok := m.GetTokenCounter(instanceID)
	if !ok {
		return
	}
	tc.Sub(EstimateTokens(message))
}

// EstimateTokens estimates token count based on character count: rune count * 2.
func EstimateTokens(message string) uint64 {
	if message == "" {
		return 0
	}
	return uint64(utf8.RuneCountInString(message) * 2)
}
