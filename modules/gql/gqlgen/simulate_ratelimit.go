package gqlgen

import (
	"sync"
	"time"
)

const simulateRateLimit = 30

var simulateRateLimiter = newTokenBucket(simulateRateLimit, time.Minute)

type tokenBucket struct {
	mu       sync.Mutex
	tokens   int
	max      int
	interval time.Duration
	last     time.Time
}

func newTokenBucket(max int, interval time.Duration) *tokenBucket {
	return &tokenBucket{
		tokens:   max,
		max:      max,
		interval: interval,
		last:     time.Now(),
	}
}

func (tb *tokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.last)
	refill := int(elapsed / (tb.interval / time.Duration(tb.max)))
	if refill > 0 {
		tb.tokens += refill
		if tb.tokens > tb.max {
			tb.tokens = tb.max
		}
		tb.last = now
	}

	if tb.tokens > 0 {
		tb.tokens--
		return true
	}
	return false
}
