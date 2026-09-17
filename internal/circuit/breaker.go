package circuit

import (
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

type Breaker struct {
	mu               sync.Mutex
	state            State
	failures         int
	failureThreshold int
	cooldown         time.Duration
	openedAt         time.Time
	halfOpenInFlight bool
	now              func() time.Time
}

type Snapshot struct {
	State            State
	Failures         int
	FailureThreshold int
	Cooldown         time.Duration
	RetryAfter       time.Duration
}

func New(failureThreshold int, cooldown time.Duration) *Breaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if cooldown <= 0 {
		cooldown = 10 * time.Second
	}
	return &Breaker{failureThreshold: failureThreshold, cooldown: cooldown, now: time.Now}
}

func (b *Breaker) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open {
		return b.now().Sub(b.openedAt) >= b.cooldown
	}
	if b.state == HalfOpen {
		return !b.halfOpenInFlight
	}
	return true
}

func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open && b.now().Sub(b.openedAt) >= b.cooldown {
		b.state = HalfOpen
		b.halfOpenInFlight = false
	}
	if b.state == Open {
		return false
	}
	if b.state == HalfOpen {
		if b.halfOpenInFlight {
			return false
		}
		b.halfOpenInFlight = true
	}
	return true
}

func (b *Breaker) Success() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := b.state != Closed
	b.failures = 0
	b.state = Closed
	b.halfOpenInFlight = false
	return changed
}

func (b *Breaker) Failure() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	previous := b.state
	b.halfOpenInFlight = false
	b.failures++
	if b.state == HalfOpen || b.failures >= b.failureThreshold {
		b.state = Open
		b.openedAt = b.now()
	}
	return previous != b.state
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) >= b.cooldown {
		return HalfOpen
	}
	return b.state
}

func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.state
	retryAfter := time.Duration(0)
	if state == Open {
		retryAfter = b.cooldown - b.now().Sub(b.openedAt)
		if retryAfter <= 0 {
			state = HalfOpen
			retryAfter = 0
		}
	}
	return Snapshot{State: state, Failures: b.failures, FailureThreshold: b.failureThreshold, Cooldown: b.cooldown, RetryAfter: retryAfter}
}

func (b *Breaker) Reset() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := b.state != Closed || b.failures != 0 || b.halfOpenInFlight
	b.state = Closed
	b.failures = 0
	b.openedAt = time.Time{}
	b.halfOpenInFlight = false
	return changed
}
