package integration_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/gateway"
	"github.com/example/gatego/internal/metrics"
)

type backendState struct {
	URL      string `json:"url"`
	Draining bool   `json:"draining"`
}

func TestBackendDrainLifecycleOverHTTP(t *testing.T) {
	backendA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("a")) }))
	defer backendA.Close()
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("b")) }))
	defer backendB.Close()

	raw := `{"routes":[{"name":"api","path_prefix":"/api","rate_limit":{"requests_per_second":10000,"burst":10000},"backends":[{"url":"` + backendA.URL + `"},{"url":"` + backendB.URL + `"}]}]}`
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw, err := gateway.New("", cfg, "secret", "admin", nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gw)
	defer server.Close()

	adminRequest := func(method, path string) *http.Response {
		req, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Admin-Key", "admin")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	aHost := strings.TrimPrefix(backendA.URL, "http://")
	bHost := strings.TrimPrefix(backendB.URL, "http://")
	resp := adminRequest(http.MethodPost, "/admin/backends/drain?route=api&backend="+aHost)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("drain A status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	for i := 0; i < 20; i++ {
		resp, err = server.Client().Get(server.URL + "/api/work")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "b" || resp.Header.Get("X-GateGo-Backend") != bHost {
			t.Fatalf("request %d reached wrong backend: status=%d backend=%q body=%q", i, resp.StatusCode, resp.Header.Get("X-GateGo-Backend"), body)
		}
	}

	resp = adminRequest(http.MethodGet, "/admin/backends")
	var states []backendState
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(states) != 2 || !states[0].Draining {
		t.Fatalf("unexpected backend states: %+v", states)
	}

	resp = adminRequest(http.MethodPost, "/admin/backends/drain?route=api&backend="+bHost)
	_ = resp.Body.Close()
	resp, err = server.Client().Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("all-draining readiness = %d, want 503", resp.StatusCode)
	}

	resp = adminRequest(http.MethodDelete, "/admin/backends/drain?route=api&backend="+aHost)
	_ = resp.Body.Close()
	resp, err = server.Client().Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reactivated readiness = %d, want 200", resp.StatusCode)
	}

	metricsRR := httptest.NewRecorder()
	m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRR.Body.String(), `gatego_backend_draining{route="api",backend="`+aHost+`"} 0`) || !strings.Contains(metricsRR.Body.String(), `gatego_backend_draining{route="api",backend="`+bHost+`"} 1`) {
		t.Fatalf("drain metrics do not reflect lifecycle:\n%s", metricsRR.Body.String())
	}
}
