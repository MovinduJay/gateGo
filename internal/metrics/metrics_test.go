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
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric %q missing from:\n%s", want, body)
		}
	}
}
