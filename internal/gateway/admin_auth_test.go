package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/metrics"
)

func TestEmptyAdminKeyNeverAuthenticates(t *testing.T) {
	cfg, err := config.Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://backend"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/config", nil)
	req.Header.Set("X-Admin-Key", "")
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("empty admin key status = %d, want 401", rr.Code)
	}
}
