package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseAndSortLongestPrefixFirst(t *testing.T) {
	raw := []byte(`{
	  "routes": [
	    {"name":"root","path_prefix":"/api","backends":[{"url":"http://a"}]},
	    {"name":"specific","path_prefix":"/api/payments","timeout":"2s","backends":[{"url":"http://b"}]}
	  ]
	}`)
	cfg, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routes[0].Name != "specific" {
		t.Fatalf("expected longest prefix first, got %s", cfg.Routes[0].Name)
	}
	if cfg.Routes[0].Timeout.Std().String() != "2s" {
		t.Fatalf("duration not parsed: %s", cfg.Routes[0].Timeout.Std())
	}
	resilience := cfg.Routes[0].Resilience
	if resilience.CircuitFailureThreshold != 20 || resilience.CircuitCooldown.Std() != 10*time.Second || resilience.MaxConcurrentPerBackend != 16 || resilience.MaxConcurrentRetries != 8 {
		t.Fatalf("unexpected resilience defaults: %+v", resilience)
	}
}

func TestRejectInvalidResilienceConfiguration(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","resilience":{"max_concurrent_retries":-1},"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "resilience values must be positive") {
		t.Fatalf("expected resilience validation error, got %v", err)
	}
}
