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
