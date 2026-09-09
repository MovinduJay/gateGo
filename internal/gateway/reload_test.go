package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/metrics"
)

func TestAutomaticConfigReloadUsesLastKnownGoodConfig(t *testing.T) {
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "one") }))
	defer one.Close()
	two := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "two") }))
	defer two.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "gatego.json")
	write := func(backend string) {
		t.Helper()
		raw := fmt.Sprintf(`{"server":{"config_reload_interval":"10ms"},"routes":[{"name":"api","path_prefix":"/api","rate_limit":{"requests_per_second":100,"burst":100},"backends":[{"url":%q}]}]}`, backend)
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		now := time.Now().Add(time.Second)
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
	}
	write(one.URL)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New(path, cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gw.WatchConfig(ctx, 10*time.Millisecond)
	requestBody := func() string {
		rr := httptest.NewRecorder()
		gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api", nil))
		return rr.Body.String()
	}
	if got := requestBody(); got != "one" {
		t.Fatalf("initial backend=%q", got)
	}
	write(two.URL)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && requestBody() != "two" {
		time.Sleep(10 * time.Millisecond)
	}
	if got := requestBody(); got != "two" {
		t.Fatalf("config did not reload, body=%q", got)
	}
	if err := os.WriteFile(path, []byte(`{"invalid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(path, now, now)
	time.Sleep(50 * time.Millisecond)
	if got := requestBody(); got != "two" {
		t.Fatalf("invalid config replaced last known-good routes, body=%q", got)
	}
}
