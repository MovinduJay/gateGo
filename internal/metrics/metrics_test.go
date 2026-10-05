package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContainmentMetricsAreExposed(t *testing.T) {
	m := New()
	m.IncRetryRejected("api")
	m.IncBulkheadRejected("api")
	m.SetCircuitState("api", "backend-b", "open")
	m.SetDraining("api", "backend-b", true)
	m.IncLoadShed("api")
	m.IncClientCanceled("api", "backend-b")
	m.AddInflight("api", 2)
	m.AddRetryInflight("api", 1)
	m.IncConfigReload("success")
	m.IncConfigReload("failure")
	m.SetConfigRevision("abc123")
	m.IncShadowRequest("api", "success")
	m.SetRateLimitBackend("redis")
	m.IncRateLimitFallback()
	m.IncHealthTransition("api", "backend-b", "unhealthy")
	m.IncCacheRequest("api", "hit")
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`gatego_retry_rejected_total{route="api"} 1`,
		`gatego_bulkhead_rejected_total{route="api"} 1`,
		`gatego_backend_circuit_state{route="api",backend="backend-b",state="open"} 1`,
		`gatego_backend_draining{route="api",backend="backend-b"} 1`,
		`gatego_load_shed_total{route="api"} 1`,
		`gatego_client_canceled_total{route="api",backend="backend-b"} 1`,
		`gatego_inflight_requests{route="api"} 2`,
		`gatego_inflight_retries{route="api"} 1`,
		`gatego_config_reloads_total{result="success"} 1`,
		`gatego_config_reloads_total{result="failure"} 1`,
		`gatego_config_info{revision="abc123"} 1`,
		`gatego_shadow_requests_total{route="api",result="success"} 1`,
		`gatego_rate_limit_backend{backend="redis"} 1`,
		`gatego_rate_limit_backend{backend="local"} 0`,
		`gatego_rate_limit_fallbacks_total 1`,
		`gatego_backend_health_transitions_total{route="api",backend="backend-b",state="unhealthy"} 1`,
		`gatego_cache_requests_total{route="api",result="hit"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric %q missing from:\n%s", want, body)
		}
	}
}
