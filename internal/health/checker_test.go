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
	checker := New(nil, time.Second, time.Second, 2, 1, m)
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
