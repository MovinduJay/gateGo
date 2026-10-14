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
	var failingAttempt, healthyAttempt, routeHeader string
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failingAttempt = r.Header.Get("X-GateGo-Attempt")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyAttempt = r.Header.Get("X-GateGo-Attempt")
		routeHeader = r.Header.Get("X-GateGo-Route")
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
	if failingAttempt != "1" || healthyAttempt != "2" || routeHeader != "api" {
		t.Fatalf("upstream metadata: failing attempt=%q healthy attempt=%q route=%q", failingAttempt, healthyAttempt, routeHeader)
	}
}

func TestResponseCacheHitAndAdminPurge(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalls++
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"version-1"`)
		_, _ = w.Write([]byte("response"))
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","cache":{"enabled":true,"ttl":"1m","max_entries":10,"max_body_bytes":1024},"backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw, err := New("", cfg, "secret", "admin", nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/item?view=full", nil))
		return rr
	}
	if rr := request(); rr.Code != http.StatusOK || rr.Header().Get("X-GateGo-Cache") != "MISS" {
		t.Fatalf("first response = %d cache=%q body=%q", rr.Code, rr.Header().Get("X-GateGo-Cache"), rr.Body.String())
	}
	if rr := request(); rr.Code != http.StatusOK || rr.Header().Get("X-GateGo-Cache") != "HIT" || rr.Body.String() != "response" {
		t.Fatalf("cached response = %d cache=%q body=%q", rr.Code, rr.Header().Get("X-GateGo-Cache"), rr.Body.String())
	}
	if backendCalls != 1 {
		t.Fatalf("backend calls = %d, want 1", backendCalls)
	}
	conditional := httptest.NewRequest(http.MethodGet, "http://gateway/api/item?view=full", nil)
	conditional.Header.Set("If-None-Match", `W/"version-1"`)
	conditionalRR := httptest.NewRecorder()
	gw.ServeHTTP(conditionalRR, conditional)
	if conditionalRR.Code != http.StatusNotModified || conditionalRR.Body.Len() != 0 || conditionalRR.Header().Get("X-GateGo-Cache") != "HIT" {
		t.Fatalf("conditional cache response = %d cache=%q body=%q", conditionalRR.Code, conditionalRR.Header().Get("X-GateGo-Cache"), conditionalRR.Body.String())
	}
	if backendCalls != 1 {
		t.Fatalf("conditional request reached backend; calls=%d", backendCalls)
	}
	unauthorized := httptest.NewRecorder()
	gw.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "http://gateway/admin/cache/purge?route=api", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized purge = %d, want 401", unauthorized.Code)
	}
	purge := httptest.NewRequest(http.MethodPost, "http://gateway/admin/cache/purge?route=api", nil)
	purge.Header.Set("X-Admin-Key", "admin")
	purgeRR := httptest.NewRecorder()
	gw.ServeHTTP(purgeRR, purge)
	if purgeRR.Code != http.StatusOK || !strings.Contains(purgeRR.Body.String(), `"entries":1`) {
		t.Fatalf("purge = %d %s", purgeRR.Code, purgeRR.Body.String())
	}
	request()
	if backendCalls != 2 {
		t.Fatalf("backend calls after purge = %d, want 2", backendCalls)
	}
	metricsRR := httptest.NewRecorder()
	m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRR.Body.String(), `gatego_cache_requests_total{route="api",result="hit"} 2`) {
		t.Fatalf("cache hit metric missing: %s", metricsRR.Body.String())
	}
}

func TestResponseCacheSkipsPrivateResponses(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Cache-Control", "private, max-age=60")
		_, _ = w.Write([]byte("private"))
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","cache":{"enabled":true},"backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		gw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway/item", nil))
	}
	if calls != 2 {
		t.Fatalf("private response was cached; calls=%d", calls)
	}
}

func TestResponseCacheBypassesCredentialedRequest(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte("public"))
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","cache":{"enabled":true},"backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "http://gateway/item", nil)
		req.Header.Set("Cookie", "session=private")
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		if rr.Header().Get("X-GateGo-Cache") != "BYPASS" {
			t.Fatalf("cache status = %q, want BYPASS", rr.Header().Get("X-GateGo-Cache"))
		}
	}
	if calls != 2 {
		t.Fatalf("credentialed request used cache; calls=%d", calls)
	}
	metricsRR := httptest.NewRecorder()
	gw.metrics.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRR.Body.String(), `gatego_cache_requests_total{route="api",result="bypass"} 2`) {
		t.Fatalf("cache bypass metric missing: %s", metricsRR.Body.String())
	}
}

func TestAdminCacheDiagnostics(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("cached")) }))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","cache":{"enabled":true,"ttl":"45s","max_entries":12,"max_body_bytes":2048},"backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	gw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway/item", nil))
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/cache", nil)
	req.Header.Set("X-Admin-Key", "admin")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"entries":1`) || !strings.Contains(rr.Body.String(), `"capacity":12`) || !strings.Contains(rr.Body.String(), `"ttl_ms":45000`) {
		t.Fatalf("cache diagnostics = %d %s", rr.Code, rr.Body.String())
	}
}

func TestResponseCacheTTLHonorsSharedMaxAge(t *testing.T) {
	header := http.Header{"Cache-Control": []string{"public, max-age=30, s-maxage=5"}}
	if got := responseCacheTTL(header, time.Minute); got != 5*time.Second {
		t.Fatalf("TTL = %s, want 5s", got)
	}
	header.Set("Cache-Control", "max-age=120")
	if got := responseCacheTTL(header, time.Minute); got != time.Minute {
		t.Fatalf("capped TTL = %s, want 1m", got)
	}
	header.Set("Cache-Control", "max-age=0")
	if got := responseCacheTTL(header, time.Minute); got != 0 {
		t.Fatalf("zero TTL = %s, want 0", got)
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
	readinessReq := httptest.NewRequest(http.MethodGet, "http://gateway/admin/readiness", nil)
	readinessReq.Header.Set("X-Admin-Key", "admin")
	readinessRR := httptest.NewRecorder()
	gw.ServeHTTP(readinessRR, readinessReq)
	if readinessRR.Code != http.StatusServiceUnavailable || !strings.Contains(readinessRR.Body.String(), `"eligible":0`) || !strings.Contains(readinessRR.Body.String(), `"open_circuits":1`) {
		t.Fatalf("readiness diagnostics = %d %s", readinessRR.Code, readinessRR.Body.String())
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
	etag := rr.Header().Get("ETag")
	revision := rr.Header().Get("X-GateGo-Config-Revision")
	if etag == "" || revision == "" || etag != `"`+revision+`"` {
		t.Fatalf("invalid config revision headers: etag=%q revision=%q", etag, revision)
	}
	conditional := httptest.NewRequest(http.MethodGet, "http://gateway/admin/config", nil)
	conditional.Header.Set("X-Admin-Key", "admin")
	conditional.Header.Set("If-None-Match", etag)
	conditionalRR := httptest.NewRecorder()
	gw.ServeHTTP(conditionalRR, conditional)
	if conditionalRR.Code != http.StatusNotModified || conditionalRR.Body.Len() != 0 {
		t.Fatalf("conditional config response = %d body=%q", conditionalRR.Code, conditionalRR.Body.String())
	}
}

func TestConfigRevisionIsDeterministicAndChangesWithConfig(t *testing.T) {
	one, err := config.Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	two, err := config.Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := config.Parse([]byte(`{"routes":[{"name":"api","timeout":"4s","backends":[{"url":"http://a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if configRevision(one) != configRevision(two) {
		t.Fatal("equivalent effective configs must have the same revision")
	}
	if configRevision(one) == configRevision(changed) {
		t.Fatal("different effective configs must have different revisions")
	}
}

func TestMaintenanceModeWithdrawsReadinessAndRejectsTraffic(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	admin := func(method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway/admin/maintenance", nil)
		req.Header.Set("X-Admin-Key", "admin")
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}
	if rr := admin(http.MethodPost); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"maintenance":true`) {
		t.Fatalf("enable maintenance = %d %s", rr.Code, rr.Body.String())
	}
	if gw.Ready() {
		t.Fatal("maintenance gateway must not be ready")
	}
	traffic := httptest.NewRecorder()
	gw.ServeHTTP(traffic, httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil))
	if traffic.Code != http.StatusServiceUnavailable || traffic.Header().Get("Retry-After") != "1" || backendCalls != 0 {
		t.Fatalf("maintenance traffic = %d retry-after=%q backend-calls=%d", traffic.Code, traffic.Header().Get("Retry-After"), backendCalls)
	}
	health := httptest.NewRecorder()
	gw.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "http://gateway/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health during maintenance = %d", health.Code)
	}
	if rr := admin(http.MethodDelete); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"maintenance":false`) {
		t.Fatalf("disable maintenance = %d %s", rr.Code, rr.Body.String())
	}
	traffic = httptest.NewRecorder()
	gw.ServeHTTP(traffic, httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil))
	if traffic.Code != http.StatusOK || backendCalls != 1 {
		t.Fatalf("restored traffic = %d backend-calls=%d", traffic.Code, backendCalls)
	}
}

func TestMaintenanceWaitsForInflightDrainWithBound(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		gw.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway/api/work", nil))
	}()
	<-started
	maintenance := func(wait string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/maintenance?wait="+wait, nil)
		req.Header.Set("X-Admin-Key", "admin")
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}
	if rr := maintenance("20ms"); rr.Code != http.StatusAccepted || !strings.Contains(rr.Body.String(), `"in_flight":1`) {
		t.Fatalf("bounded drain = %d %s", rr.Code, rr.Body.String())
	}
	close(release)
	<-done
	if rr := maintenance("1s"); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"drained":true`) {
		t.Fatalf("completed drain = %d %s", rr.Code, rr.Body.String())
	}
}

func TestAdminEventsAreAuthenticatedFilteredAndBounded(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://backend"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	enable := httptest.NewRequest(http.MethodPost, "http://gateway/admin/maintenance", nil)
	enable.Header.Set("X-Admin-Key", "admin")
	gw.ServeHTTP(httptest.NewRecorder(), enable)

	unauthorized := httptest.NewRecorder()
	gw.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://gateway/admin/events", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized events status = %d", unauthorized.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/events?type=maintenance&limit=1", nil)
	req.Header.Set("X-Admin-Key", "admin")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"type":"maintenance"`) || !strings.Contains(rr.Body.String(), `"enabled":true`) || strings.Contains(rr.Body.String(), `"type":"gateway_started"`) {
		t.Fatalf("filtered events response = %d %s", rr.Code, rr.Body.String())
	}

	invalid := httptest.NewRequest(http.MethodGet, "http://gateway/admin/events?limit=201", nil)
	invalid.Header.Set("X-Admin-Key", "admin")
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, invalid)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d", rr.Code)
	}
	invalid = httptest.NewRequest(http.MethodGet, "http://gateway/admin/events?after=invalid", nil)
	invalid.Header.Set("X-Admin-Key", "admin")
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, invalid)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor status = %d", rr.Code)
	}
}

func TestAdminRoutesReportsRuntimeCapacity(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","load_balancing":"least_connections","resilience":{"max_concurrent_requests":12,"max_concurrent_retries":3,"total_timeout":"2s"},"shadow":{"url":"http://candidate","max_concurrent":2},"backends":[{"url":"http://backend"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/routes", nil)
	req.Header.Set("X-Admin-Key", "admin")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("routes status = %d body=%s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{`"name":"api"`, `"load_balancing":"least_connections"`, `"request_capacity":12`, `"retry_capacity":3`, `"total_timeout_ms":2000`, `"shadow_capacity":2`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("route state missing %s: %s", want, rr.Body.String())
		}
	}
}

func TestRouteAccessPolicyDenyOverridesAllow(t *testing.T) {
	called := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","path_prefix":"/api","access":{"allow_cidrs":["192.0.2.0/24"],"deny_cidrs":["192.0.2.8/32"]},"backends":[{"url":"` + backend.URL + `"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	request := func(remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil)
		req.RemoteAddr = remote + ":1234"
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}
	if rr := request("198.51.100.1"); rr.Code != http.StatusForbidden {
		t.Fatalf("outside allow list status = %d, want 403", rr.Code)
	}
	if rr := request("192.0.2.8"); rr.Code != http.StatusForbidden {
		t.Fatalf("explicitly denied status = %d, want 403", rr.Code)
	}
	if called {
		t.Fatal("denied requests must not reach the backend")
	}
	if rr := request("192.0.2.9"); rr.Code != http.StatusOK {
		t.Fatalf("allowed address status = %d, want 200", rr.Code)
	}
}

func TestRouteCORSPreflightAndResponsePolicy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://unsafe.example")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	raw := `{"routes":[{"name":"api","path_prefix":"/api","methods":["GET"],"cors":{"enabled":true,"allowed_origins":["https://app.example"],"allowed_methods":["GET"],"allowed_headers":["Authorization"],"exposed_headers":["X-Request-ID"],"allow_credentials":true,"max_age_seconds":600},"backends":[{"url":"` + backend.URL + `"}]}]}`
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "http://gateway/api/test", nil)
	preflight.Header.Set("Origin", "https://app.example")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, preflight)
	if rr.Code != http.StatusNoContent || rr.Header().Get("Access-Control-Max-Age") != "600" || rr.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("preflight = %d headers=%v", rr.Code, rr.Header())
	}

	request := httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil)
	request.Header.Set("Origin", "https://app.example")
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, request)
	if rr.Code != http.StatusOK || rr.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatalf("CORS response = %d origin=%q", rr.Code, rr.Header().Get("Access-Control-Allow-Origin"))
	}

	denied := httptest.NewRequest(http.MethodOptions, "http://gateway/api/test", nil)
	denied.Header.Set("Origin", "https://evil.example")
	denied.Header.Set("Access-Control-Request-Method", "GET")
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, denied)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("denied preflight status = %d, want 403", rr.Code)
	}
}

func TestRouteHeaderTransformsApplyAtProxyBoundary(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Remove-Me") != "" || r.Header.Get("X-Environment") != "production" {
			http.Error(w, "request transform missing", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Upstream-Private", "secret")
		w.Header().Set("X-Service", "backend")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	raw := `{"routes":[{"name":"api","path_prefix":"/api","headers":{"request_set":{"X-Environment":"production"},"request_remove":["X-Remove-Me"],"response_set":{"X-Service":"gateway"},"response_remove":["X-Upstream-Private"]},"backends":[{"url":"` + backend.URL + `"}]}]}`
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil)
	req.Header.Set("X-Remove-Me", "client-value")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("X-Service") != "gateway" || rr.Header().Get("X-Upstream-Private") != "" {
		t.Fatalf("transformed response = %d headers=%v body=%s", rr.Code, rr.Header(), rr.Body.String())
	}
}

func TestRouteCanStripPrefixAndPreserveQuery(t *testing.T) {
	var received string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	raw := `{"routes":[{"name":"payments","path_prefix":"/payments","strip_path_prefix":true,"backends":[{"url":"` + backend.URL + `"}]}]}`
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/payments/orders/42?expand=items", nil))
	if rr.Code != http.StatusOK || received != "/orders/42?expand=items" {
		t.Fatalf("proxy response=%d upstream URI=%q", rr.Code, received)
	}
	rr = httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/payments", nil))
	if rr.Code != http.StatusOK || received != "/" {
		t.Fatalf("prefix-root response=%d upstream URI=%q", rr.Code, received)
	}
}

func TestAPIKeyAuthenticationUsesEnvironmentSecretAndStripsCredential(t *testing.T) {
	t.Setenv("GATEGO_TEST_PARTNER_KEY", "correct-secret")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Partner-Key") != "" {
			http.Error(w, "credential leaked upstream", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	raw := `{"routes":[{"name":"partner","path_prefix":"/partner","auth":{"mode":"api_key","api_key_env":"GATEGO_TEST_PARTNER_KEY","header":"X-Partner-Key"},"backends":[{"url":"` + backend.URL + `"}]}]}`
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	call := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://gateway/partner/data", nil)
		req.Header.Set("X-Partner-Key", key)
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, req)
		return rr
	}
	if rr := call("wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d", rr.Code)
	}
	if rr := call("correct-secret"); rr.Code != http.StatusOK {
		t.Fatalf("correct key status = %d body=%s", rr.Code, rr.Body.String())
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

func TestRateLimitResponsesExposeQuotaMetadata(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer backend.Close()
	raw := []byte(`{"routes":[{"name":"api","path_prefix":"/api","rate_limit":{"requests_per_second":0.1,"burst":1},"backends":[{"url":"` + backend.URL + `"}]}]}`)
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	call := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil))
		return rr
	}
	first := call()
	if first.Code != http.StatusOK || first.Header().Get("RateLimit-Limit") != "1" || first.Header().Get("RateLimit-Remaining") != "0" {
		t.Fatalf("first response = %d headers=%v", first.Code, first.Header())
	}
	denied := call()
	if denied.Code != http.StatusTooManyRequests || denied.Header().Get("RateLimit-Reset") != "10" || denied.Header().Get("Retry-After") != "10" {
		t.Fatalf("denied response = %d headers=%v", denied.Code, denied.Header())
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
