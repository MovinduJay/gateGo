package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/gatego/internal/balancer"
	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/metrics"
)

func TestHealthThresholdsAndMetrics(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(false)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resilience := config.ResilienceConfig{CircuitFailureThreshold: 20, CircuitCooldown: config.Duration(time.Second), MaxConcurrentPerBackend: 4, MaxConcurrentRetries: 1}
	lb, err := balancer.New([]config.BackendConfig{{URL: server.URL, HealthPath: "/healthz", Weight: 1}}, resilience)
	if err != nil {
		t.Fatal(err)
	}
	backend := lb.Backends()[0]
	m := metrics.New()
	checker := New(nil, time.Second, time.Second, 2, 1, 2, m)
	target := Target{Route: "api", Backend: backend}

	checker.checkOne(context.Background(), target)
	if !backend.Healthy.Load() {
		t.Fatal("one failed check must not cross the failure threshold")
	}
	checker.checkOne(context.Background(), target)
	if backend.Healthy.Load() {
		t.Fatal("two failed checks should mark the backend unhealthy")
	}
	healthy.Store(true)
	checker.checkOne(context.Background(), target)
	if !backend.Healthy.Load() {
		t.Fatal("successful check should restore backend health")
	}

	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rr.Body.String(), `gatego_backend_healthy{route="api",backend="`+backend.URL.Host+`"} 1`) {
		t.Fatalf("healthy metric missing: %s", rr.Body.String())
	}
}

func TestHealthChecksHaveBoundedConcurrency(t *testing.T) {
	var current, maximum, completed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := current.Add(1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		completed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	resilience := config.ResilienceConfig{CircuitFailureThreshold: 20, CircuitCooldown: config.Duration(time.Second), MaxConcurrentPerBackend: 4, MaxConcurrentRetries: 1}
	var targets []Target
	for i := 0; i < 8; i++ {
		lb, err := balancer.New([]config.BackendConfig{{URL: server.URL, Weight: 1}}, resilience)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, Target{Route: "api", Backend: lb.Backends()[0]})
	}
	checker := New(func() []Target { return targets }, time.Second, time.Second, 2, 1, 2, metrics.New())
	checker.check(context.Background())
	deadline := time.Now().Add(time.Second)
	for completed.Load() < int32(len(targets)) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if completed.Load() != int32(len(targets)) {
		t.Fatalf("completed %d of %d checks", completed.Load(), len(targets))
	}
	if maximum.Load() > 2 {
		t.Fatalf("health concurrency reached %d, want at most 2", maximum.Load())
	}
}
