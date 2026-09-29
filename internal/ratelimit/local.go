package ratelimit

import (
	"math"
	"sync"
	"time"
)

type Decision struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

type LocalLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	ops     uint64
}

func NewLocal() *LocalLimiter {
	return &LocalLimiter{buckets: map[string]*bucket{}, now: time.Now}
}

func (l *LocalLimiter) Allow(key string, rate float64, burst int) bool {
	return l.Check(key, rate, burst).Allowed
}

func (l *LocalLimiter) Check(key string, rate float64, burst int) Decision {
	if rate <= 0 || burst <= 0 {
		return Decision{Allowed: true, Limit: burst, Remaining: burst}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.ops++
	if l.ops%1024 == 0 {
		cutoff := now.Add(-10 * time.Minute)
		for k, existing := range l.buckets {
			if existing.seen.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), last: now, seen: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * rate
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last, b.seen = now, now
	if b.tokens < 1 {
		return Decision{
			Allowed:    false,
			Limit:      burst,
			Remaining:  int(math.Floor(b.tokens)),
			RetryAfter: time.Duration(math.Ceil((1-b.tokens)/rate*1000)) * time.Millisecond,
		}
	}
	b.tokens--
	return Decision{Allowed: true, Limit: burst, Remaining: int(math.Floor(b.tokens))}
}

func (l *LocalLimiter) Cleanup(olderThan time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-olderThan)
	for k, b := range l.buckets {
		if b.seen.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
