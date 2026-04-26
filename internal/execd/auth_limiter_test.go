package execd

import (
	"testing"
	"time"
)

func TestAuthFailureLimiterBlocksAndResets(t *testing.T) {
	base := time.Unix(100, 0)
	limiter := newAuthFailureLimiter(SecurityConfig{
		AuthFailureLimit:     2,
		AuthFailureWindowSec: 10,
	})
	limiter.now = func() time.Time { return base }

	if !limiter.allow("127.0.0.1") {
		t.Fatal("first request should be allowed")
	}
	limiter.recordFailure("127.0.0.1")
	if !limiter.allow("127.0.0.1") {
		t.Fatal("second request should be allowed before limit")
	}
	limiter.recordFailure("127.0.0.1")
	if limiter.allow("127.0.0.1") {
		t.Fatal("request should be blocked at limit")
	}
	limiter.recordSuccess("127.0.0.1")
	if !limiter.allow("127.0.0.1") {
		t.Fatal("successful auth should clear failures")
	}

	limiter.recordFailure("127.0.0.1")
	limiter.recordFailure("127.0.0.1")
	base = base.Add(11 * time.Second)
	if !limiter.allow("127.0.0.1") {
		t.Fatal("expired failures should not block")
	}
}

func TestAuthFailureLimiterDisabled(t *testing.T) {
	if limiter := newAuthFailureLimiter(SecurityConfig{}); limiter != nil {
		t.Fatal("zero config should disable auth limiter")
	}
}

func TestAuthFailureLimiterCapsPerKeyRecords(t *testing.T) {
	base := time.Unix(100, 0)
	limiter := newAuthFailureLimiter(SecurityConfig{
		AuthFailureLimit:     3,
		AuthFailureWindowSec: 60,
	})
	limiter.now = func() time.Time { return base }

	for i := 0; i < 100; i++ {
		limiter.recordFailure("127.0.0.1")
	}
	if got := len(limiter.items["127.0.0.1"]); got != 4 {
		t.Fatalf("expected limiter to retain limit+1 records, got %d", got)
	}
}
