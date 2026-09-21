package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestAdminConfigReturnsEffectiveConfiguration(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	request := func(method, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway/admin/config", nil)
		req.Header.Set("X-Admin-Key", key)
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}
	if rr := request(http.MethodGet, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rr.Code)
	}
	if rr := request(http.MethodPost, "admin"); rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("wrong-method response = %d allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
	rr := request(http.MethodGet, "admin")
	if rr.Code != http.StatusOK {
		t.Fatalf("config status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"timeout":"3s"`) || !strings.Contains(rr.Body.String(), `"max_concurrent_requests":256`) {
		t.Fatalf("effective defaults missing from config: %s", rr.Body.String())
	}
}

func TestBackendDrainStopsNewTrafficAndPreservesInflight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/inflight" {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte("a"))
	}))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("b")) }))
	defer backendB.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","backends":[{"url":"` + backendA.URL + `"},{"url":"` + backendB.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	setDrain := func(method, backend string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway/admin/backends/drain?route=api&backend="+backend, nil)
		req.Header.Set("X-Admin-Key", "admin")
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}

	inflight := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/inflight", nil))
		inflight <- rr
	}()
	<-started
	if rr := setDrain(http.MethodPost, strings.TrimPrefix(backendA.URL, "http://")); rr.Code != http.StatusOK {
		t.Fatalf("drain A failed: %d %s", rr.Code, rr.Body.String())
	}
	close(release)
	if rr := <-inflight; rr.Code != http.StatusOK || rr.Body.String() != "a" {
		t.Fatalf("in-flight request was interrupted: %d %q", rr.Code, rr.Body.String())
	}
	for i := 0; i < 10; i++ {
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/new", nil))
		if rr.Code != http.StatusOK || rr.Body.String() != "b" {
			t.Fatalf("drained backend received new request %d: %d %q", i, rr.Code, rr.Body.String())
		}
	}
	if rr := setDrain(http.MethodPost, strings.TrimPrefix(backendB.URL, "http://")); rr.Code != http.StatusOK {
		t.Fatalf("drain B failed: %d %s", rr.Code, rr.Body.String())
	}
	if gw.Ready() {
		t.Fatal("gateway must not be ready when all route backends are draining")
	}
	if rr := setDrain(http.MethodDelete, strings.TrimPrefix(backendA.URL, "http://")); rr.Code != http.StatusOK {
		t.Fatalf("activate A failed: %d %s", rr.Code, rr.Body.String())
	}
	if !gw.Ready() {
		t.Fatal("gateway should become ready when a backend is activated")
	}
}

func TestRouteAdmissionControlShedsExcessLoad(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	raw := []byte(`{"routes":[{"name":"api","path_prefix":"/api","resilience":{"max_concurrent_requests":1},"backends":[{"url":"` + backend.URL + `"}]}]}`)
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw, err := New("", cfg, "secret", "admin", nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		gw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway/api/first", nil))
	}()
	<-started
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/second", nil))
	if rr.Code != http.StatusServiceUnavailable || rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("shed response = %d retry-after=%q body=%s", rr.Code, rr.Header().Get("Retry-After"), rr.Body.String())
	}
	close(release)
	<-firstDone
	metricsRR := httptest.NewRecorder()
	m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRR.Body.String(), `gatego_load_shed_total{route="api"} 1`) {
		t.Fatalf("load-shed metric missing: %s", metricsRR.Body.String())
	}
}

func TestRouteRequestBodyLimitOverridesServerLimit(t *testing.T) {
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	raw := []byte(`{"server":{"max_request_body_bytes":1024},"routes":[{"name":"api","path_prefix":"/api","methods":["POST"],"max_request_body_bytes":4,"backends":[{"url":"` + backend.URL + `"}]}]}`)
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "http://gateway/api/upload", strings.NewReader("12345")))
	if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), `"limit_bytes":4`) {
		t.Fatalf("body limit response = %d %s", rr.Code, rr.Body.String())
	}
	if called {
		t.Fatal("oversized request must not reach the backend")
	}
}

func TestConfigValidationDoesNotApplyCandidate(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"current","path_prefix":"/current","backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	validate := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/config/validate", strings.NewReader(body))
		req.Header.Set("X-Admin-Key", "admin")
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}
	candidate := `{"routes":[{"name":"candidate","path_prefix":"/candidate","backends":[{"url":"` + backend.URL + `"}]}]}`
	if rr := validate(candidate); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"valid"`) {
		t.Fatalf("valid candidate rejected: %d %s", rr.Code, rr.Body.String())
	}
	if gw.match("/current/test") == nil || gw.match("/candidate/test") != nil {
		t.Fatal("validation must not replace active routes")
	}
	if rr := validate(`{"routes":[]}`); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `"status":"invalid"`) {
		t.Fatalf("invalid candidate accepted: %d %s", rr.Code, rr.Body.String())
	}
}

func TestClientCancellationDoesNotTripBackendCircuit(t *testing.T) {
	started := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","retries":2,"timeout":"5s","backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw, err := New("", cfg, "secret", "admin", nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "http://gateway/api/cancel", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not stop work after client cancellation")
	}
	backendState := gw.routes.Load().Routes[0].Balancer.Backends()[0]
	if snapshot := backendState.Breaker.Snapshot(); snapshot.Failures != 0 || snapshot.State.String() != "closed" {
		t.Fatalf("client cancellation poisoned circuit: %+v", snapshot)
	}
	metricsRR := httptest.NewRecorder()
	m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRR.Body.String(), `gatego_client_canceled_total{route="api",backend="`+backendState.URL.Host+`"} 1`) {
		t.Fatalf("client cancellation metric missing: %s", metricsRR.Body.String())
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
