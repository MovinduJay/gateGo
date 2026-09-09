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

func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = Closed
	b.halfOpenInFlight = false
}

func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halfOpenInFlight = false
	b.failures++
	if b.state == HalfOpen || b.failures >= b.failureThreshold {
		b.state = Open
		b.openedAt = b.now()
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && b.now().Sub(b.openedAt) >= b.cooldown {
		return HalfOpen
	}
	return b.state
}
