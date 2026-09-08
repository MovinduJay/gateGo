package balancer

import (
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/gatego/internal/circuit"
	"github.com/example/gatego/internal/config"
)

type Backend struct {
	URL        *url.URL
	Weight     int
	HealthPath string

	currentWeight int
	Healthy       atomic.Bool
	failures      atomic.Int32
	successes     atomic.Int32
	Breaker       *circuit.Breaker
}

func NewBackend(cfg config.BackendConfig) (*Backend, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	b := &Backend{
		URL:        u,
		Weight:     cfg.Weight,
		HealthPath: cfg.HealthPath,
		Breaker:    circuit.New(5, 10*time.Second),
	}
	b.Healthy.Store(true)
	return b, nil
}

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

func New(configs []config.BackendConfig) (*WeightedRoundRobin, error) {
	b := &WeightedRoundRobin{}
	for _, c := range configs {
		backend, err := NewBackend(c)
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
	for _, b := range w.backends {
		if !b.Healthy.Load() || !b.Breaker.Available() {
			continue
		}
		total += b.Weight
		b.currentWeight += b.Weight
		if best == nil || b.currentWeight > best.currentWeight {
			best = b
		}
	}
	if best == nil {
		return nil, errors.New("no healthy backends available")
	}
	if !best.Breaker.Allow() {
		return nil, errors.New("selected backend circuit became unavailable")
	}
	best.currentWeight -= total
	return best, nil
}

func (w *WeightedRoundRobin) Backends() []*Backend {
	return w.backends
}
