package balancer

import (
	"errors"
	"testing"
	"time"

	"github.com/example/gatego/internal/config"
)

func TestWeightedRoundRobinDistribution(t *testing.T) {
	lb, err := New([]config.BackendConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}}, config.ResilienceConfig{
		CircuitFailureThreshold: 20,
		CircuitCooldown:         config.Duration(10 * time.Second),
		MaxConcurrentPerBackend: 16,
		MaxConcurrentRetries:    8,
	})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		b, err := lb.Pick()
		if err != nil {
			t.Fatal(err)
		}
		counts[b.URL.Host]++
		b.Release()
	}
	if counts["a"] != 30 || counts["b"] != 10 {
		t.Fatalf("unexpected distribution: %#v", counts)
	}
}

func TestBulkheadRejectsWhenBackendIsAtCapacity(t *testing.T) {
	resilience := config.ResilienceConfig{
		CircuitFailureThreshold: 20,
		CircuitCooldown:         config.Duration(10 * time.Second),
		MaxConcurrentPerBackend: 1,
		MaxConcurrentRetries:    1,
	}
	lb, err := New([]config.BackendConfig{{URL: "http://a", Weight: 1}}, resilience)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := lb.Pick()
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Release()
	if _, err := lb.Pick(); !errors.Is(err, ErrAllBackendsBusy) {
		t.Fatalf("expected bulkhead rejection, got %v", err)
	}
}

func TestDrainingBackendIsExcluded(t *testing.T) {
	resilience := config.ResilienceConfig{
		CircuitFailureThreshold: 20,
		CircuitCooldown:         config.Duration(10 * time.Second),
		MaxConcurrentPerBackend: 4,
		MaxConcurrentRetries:    1,
	}
	lb, err := New([]config.BackendConfig{{URL: "http://a", Weight: 3}, {URL: "http://b", Weight: 1}}, resilience)
	if err != nil {
		t.Fatal(err)
	}
	lb.Backends()[0].Draining.Store(true)
	for i := 0; i < 20; i++ {
		backend, err := lb.Pick()
		if err != nil {
			t.Fatal(err)
		}
		if backend.URL.Host != "b" {
			t.Fatalf("draining backend selected: %s", backend.URL)
		}
		backend.Release()
	}
	lb.Backends()[1].Draining.Store(true)
	if _, err := lb.Pick(); !errors.Is(err, ErrNoBackendAvailable) {
		t.Fatalf("all draining should make route unavailable, got %v", err)
	}
}

func TestLeastConnectionsPrefersLessLoadedBackend(t *testing.T) {
	resilience := config.ResilienceConfig{
		CircuitFailureThreshold: 20,
		CircuitCooldown:         config.Duration(10 * time.Second),
		MaxConcurrentPerBackend: 4,
		MaxConcurrentRetries:    1,
	}
	lb, err := NewWithStrategy([]config.BackendConfig{{URL: "http://a", Weight: 1}, {URL: "http://b", Weight: 1}}, resilience, "least_connections")
	if err != nil {
		t.Fatal(err)
	}
	first, err := lb.Pick()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := lb.Pick()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	if first == second {
		t.Fatalf("least-connections selected loaded backend %s", first.URL)
	}
}
