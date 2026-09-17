package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/metrics"
)

func TestRetryFailsOverToSecondBackend(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer healthy.Close()

	raw := []byte(`{
	 "routes":[{
	   "name":"api","path_prefix":"/api","retries":1,"timeout":"2s",
	   "rate_limit":{"requests_per_second":100,"burst":100},
	   "backends":[
	     {"url":"` + failing.URL + `","weight":1},
	     {"url":"` + healthy.URL + `","weight":1}
	   ]
	 }]
	}`)
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected failover success, got %d body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-GateGo-Backend") == "" {
		t.Fatal("expected selected backend header")
	}
}

func TestCircuitAwareReadinessAndAdminReset(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw, err := New("", cfg, "secret", "admin", nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	target := gw.routes.Load().Routes[0].Balancer.Backends()[0]
	for i := 0; i < cfg.Routes[0].Resilience.CircuitFailureThreshold; i++ {
		target.Breaker.Failure()
	}
	if gw.Ready() {
		t.Fatal("gateway must not be ready when its only backend circuit is open")
	}

	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/circuits/reset?route=api&backend="+target.URL.Host, nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated reset status = %d, want 401", rr.Code)
	}
	req.Header.Set("X-Admin-Key", "admin")
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"count":1`) {
		t.Fatalf("reset failed: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !gw.Ready() || target.Breaker.State().String() != "closed" {
		t.Fatal("manual reset should restore circuit eligibility")
	}

	metricsRR := httptest.NewRecorder()
	m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRR.Body.String(), `state="closed"} 1`) {
		t.Fatalf("closed circuit metric missing after reset: %s", metricsRR.Body.String())
	}
}

func TestPathPrefixRespectsSegmentBoundary(t *testing.T) {
	if !pathMatches("/api", "/api") || !pathMatches("/api/users", "/api") {
		t.Fatal("expected /api route to match its segment")
	}
	if pathMatches("/apiary", "/api") {
		t.Fatal("/api must not match /apiary")
	}
}

func TestUntrustedForwardedHeadersAreRemoved(t *testing.T) {
	h := http.Header{}
	h.Set("X-Forwarded-For", "1.2.3.4")
	h.Set("X-Authenticated-Subject", "spoofed")
	sanitizeInboundHeaders(h, false)
	if h.Get("X-Forwarded-For") != "" || h.Get("X-Authenticated-Subject") != "" {
		t.Fatal("untrusted proxy or identity headers survived sanitization")
	}
}

func TestPostRetryRequiresIdempotencyKey(t *testing.T) {
	newGateway := func() (*Gateway, *int, func()) {
		failCalls := 0
		failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			failCalls++
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		raw := []byte(`{"routes":[{"name":"api","path_prefix":"/api","retries":1,"timeout":"2s","rate_limit":{"requests_per_second":100,"burst":100},"backends":[{"url":"` + failing.URL + `","weight":1},{"url":"` + healthy.URL + `","weight":1}]}]}`)
		cfg, err := config.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return gw, &failCalls, func() { failing.Close(); healthy.Close() }
	}

	gw, failCalls, cleanup := newGateway()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/api/order", nil)
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	cleanup()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST without idempotency key must not retry; got %d", rr.Code)
	}
	if *failCalls != 1 {
		t.Fatalf("expected exactly one attempt, got %d", *failCalls)
	}

	gw, _, cleanup = newGateway()
	defer cleanup()
	req = httptest.NewRequest(http.MethodPost, "http://gateway/api/order", nil)
	req.Header.Set("Idempotency-Key", "order-123")
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST with idempotency key should be eligible for retry; got %d", rr.Code)
	}
}
