package execd

import (
	"sync"
	"time"
)

type authFailureLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	items  map[string][]time.Time
}

func newAuthFailureLimiter(cfg SecurityConfig) *authFailureLimiter {
	if cfg.AuthFailureLimit <= 0 || cfg.AuthFailureWindowSec <= 0 {
		return nil
	}
	return &authFailureLimiter{
		limit:  cfg.AuthFailureLimit,
		window: time.Duration(cfg.AuthFailureWindowSec) * time.Second,
		now:    time.Now,
		items:  map[string][]time.Time{},
	}
}

func (l *authFailureLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	failures := l.recentLocked(key, l.now())
	l.items[key] = failures
	return len(failures) < l.limit
}

func (l *authFailureLimiter) recordFailure(key string) {
	_ = l.recordFailureAndExceeded(key)
}

func (l *authFailureLimiter) recordFailureAndExceeded(key string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	failures := l.recentLocked(key, now)
	failures = append(failures, now)
	if len(failures) > l.limit+1 {
		failures = failures[len(failures)-(l.limit+1):]
	}
	l.items[key] = failures
	return len(failures) > l.limit
}

func (l *authFailureLimiter) recordSuccess(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.items, key)
}

func (l *authFailureLimiter) recentLocked(key string, now time.Time) []time.Time {
	failures := l.items[key]
	if len(failures) == 0 {
		return failures
	}
	cutoff := now.Add(-l.window)
	kept := failures[:0]
	for _, ts := range failures {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(l.items, key)
	}
	return kept
}
