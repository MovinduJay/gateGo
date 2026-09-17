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
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`gatego_retry_rejected_total{route="api"} 1`,
		`gatego_bulkhead_rejected_total{route="api"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metric %q missing from:\n%s", want, body)
		}
	}
}
