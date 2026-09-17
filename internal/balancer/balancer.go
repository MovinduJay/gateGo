package balancer

import (
	"errors"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/example/gatego/internal/circuit"
	"github.com/example/gatego/internal/config"
)

var (
	ErrNoBackendAvailable = errors.New("no healthy backends available")
	ErrAllBackendsBusy    = errors.New("all eligible backend bulkheads are full")
)

type Backend struct {
	URL        *url.URL
	Weight     int
	HealthPath string

	currentWeight int
	Healthy       atomic.Bool
	Draining      atomic.Bool
	failures      atomic.Int32
	successes     atomic.Int32
	Breaker       *circuit.Breaker
	bulkhead      chan struct{}
}

func NewBackend(cfg config.BackendConfig, resilience config.ResilienceConfig) (*Backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	b := &Backend{
		URL:        u,
		Weight:     cfg.Weight,
		HealthPath: cfg.HealthPath,
		Breaker:    circuit.New(resilience.CircuitFailureThreshold, resilience.CircuitCooldown.Std()),
		bulkhead:   make(chan struct{}, resilience.MaxConcurrentPerBackend),
	}
	b.Healthy.Store(true)
	return b, nil
}

// TryAcquire reserves one upstream request slot. Keeping this admission local
// to each backend prevents retries for one failed replica from exhausting the
// connection and goroutine capacity needed by healthy replicas.
func (b *Backend) TryAcquire() bool {
	select {
	case b.bulkhead <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *Backend) Release() { <-b.bulkhead }

func (b *Backend) InFlight() int { return len(b.bulkhead) }

func (b *Backend) Capacity() int { return cap(b.bulkhead) }

func (b *Backend) MarkHealth(ok bool, failureThreshold, successThreshold int) {
	if ok {
		b.failures.Store(0)
		if b.successes.Add(1) >= int32(successThreshold) {
			b.Healthy.Store(true)
		}
		return
	}
	b.successes.Store(0)
	if b.failures.Add(1) >= int32(failureThreshold) {
		b.Healthy.Store(false)
	}
}

type WeightedRoundRobin struct {
	mu       sync.Mutex
	backends []*Backend
}

func New(configs []config.BackendConfig, resilience config.ResilienceConfig) (*WeightedRoundRobin, error) {
	b := &WeightedRoundRobin{}
	for _, c := range configs {
		backend, err := NewBackend(c, resilience)
		if err != nil {
			return nil, err
		}
		b.backends = append(b.backends, backend)
	}
	return b, nil
}

func (w *WeightedRoundRobin) Pick() (*Backend, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	var best *Backend
	total := 0
	busy := false
	for _, b := range w.backends {
		if b.Draining.Load() || !b.Healthy.Load() || !b.Breaker.Available() {
			continue
		}
		if len(b.bulkhead) == cap(b.bulkhead) {
			busy = true
			continue
		}
		total += b.Weight
		b.currentWeight += b.Weight
		if best == nil || b.currentWeight > best.currentWeight {
			best = b
		}
	}
	if best == nil {
		if busy {
			return nil, ErrAllBackendsBusy
		}
		return nil, ErrNoBackendAvailable
	}
	if !best.Breaker.Allow() {
		return nil, errors.New("selected backend circuit became unavailable")
	}
	if !best.TryAcquire() {
		return nil, errors.New("selected backend bulkhead is full")
	}
	best.currentWeight -= total
	return best, nil
}

func (w *WeightedRoundRobin) Backends() []*Backend {
	return w.backends
}
