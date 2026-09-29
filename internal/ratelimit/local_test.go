package ratelimit

import (
	"testing"
	"time"
)

func TestLocalTokenBucket(t *testing.T) {
	now := time.Unix(1000, 0)
	l := NewLocal()
	l.now = func() time.Time { return now }
	if !l.Allow("u", 1, 2) || !l.Allow("u", 1, 2) {
		t.Fatal("initial burst should pass")
	}
	if l.Allow("u", 1, 2) {
		t.Fatal("bucket should be empty")
	}
	now = now.Add(time.Second)
	if !l.Allow("u", 1, 2) {
		t.Fatal("one token should refill")
	}
}

func TestLocalDecisionReportsRemainingAndRetryDelay(t *testing.T) {
	now := time.Unix(1000, 0)
	limiter := NewLocal()
	limiter.now = func() time.Time { return now }
	first := limiter.Check("client", 2, 2)
	second := limiter.Check("client", 2, 2)
	denied := limiter.Check("client", 2, 2)
	if !first.Allowed || first.Remaining != 1 || !second.Allowed || second.Remaining != 0 {
		t.Fatalf("unexpected allowed decisions: %+v %+v", first, second)
	}
	if denied.Allowed || denied.Limit != 2 || denied.Remaining != 0 || denied.RetryAfter != 500*time.Millisecond {
		t.Fatalf("unexpected denied decision: %+v", denied)
	}
}
