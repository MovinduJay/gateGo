package circuit

import (
	"testing"
	"time"
)

func TestBreakerOpensAndRecovers(t *testing.T) {
	now := time.Unix(1000, 0)
	b := New(2, 5*time.Second)
	b.now = func() time.Time { return now }
	if !b.Allow() {
		t.Fatal("closed breaker should allow")
	}
	b.Failure()
	if !b.Allow() {
		t.Fatal("one failure should still allow")
	}
	b.Failure()
	if b.Allow() {
		t.Fatal("breaker should be open")
	}
	now = now.Add(6 * time.Second)
	if !b.Allow() {
		t.Fatal("half-open probe should be allowed")
	}
	if b.Allow() {
		t.Fatal("only one half-open probe should be allowed")
	}
	b.Success()
	if !b.Allow() {
		t.Fatal("successful probe should close breaker")
	}
}

func TestAvailableDoesNotConsumeHalfOpenProbe(t *testing.T) {
	now := time.Unix(2000, 0)
	b := New(1, time.Second)
	b.now = func() time.Time { return now }
	b.Failure()
	now = now.Add(2 * time.Second)
	if !b.Available() || !b.Available() {
		t.Fatal("availability checks must not reserve the half-open probe")
	}
	if !b.Allow() {
		t.Fatal("first half-open probe should be acquired")
	}
	if b.Available() {
		t.Fatal("probe must be unavailable while half-open request is in flight")
	}
}

func TestSnapshotAndManualReset(t *testing.T) {
	now := time.Unix(3000, 0)
	b := New(2, 5*time.Second)
	b.now = func() time.Time { return now }
	b.Failure()
	b.Failure()
	snapshot := b.Snapshot()
	if snapshot.State != Open || snapshot.Failures != 2 || snapshot.FailureThreshold != 2 || snapshot.RetryAfter != 5*time.Second {
		t.Fatalf("unexpected open snapshot: %+v", snapshot)
	}
	if !b.Reset() {
		t.Fatal("reset should report a state change")
	}
	snapshot = b.Snapshot()
	if snapshot.State != Closed || snapshot.Failures != 0 || snapshot.RetryAfter != 0 {
		t.Fatalf("unexpected reset snapshot: %+v", snapshot)
	}
}
