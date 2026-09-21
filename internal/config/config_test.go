package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDurationMarshalsAsHumanReadableString(t *testing.T) {
	raw, err := json.Marshal(Duration(1500 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"1.5s"` {
		t.Fatalf("duration JSON = %s, want %q", raw, `"1.5s"`)
	}
}

func TestParseRejectsAmbiguousRouteConfiguration(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate path", `{"routes":[{"name":"one","path_prefix":"/api","backends":[{"url":"http://a"}]},{"name":"two","path_prefix":"/api/","backends":[{"url":"http://b"}]}]}`, "duplicate path_prefix"},
		{"duplicate backend", `{"routes":[{"name":"api","backends":[{"url":"http://a"},{"url":"http://a/"}]}]}`, "duplicate backend"},
		{"duplicate method", `{"routes":[{"name":"api","methods":["get","GET"],"backends":[{"url":"http://a"}]}]}`, "duplicate method"},
		{"blank method", `{"routes":[{"name":"api","methods":["  "],"backends":[{"url":"http://a"}]}]}`, "blank method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseNormalizesMethods(t *testing.T) {
	cfg, err := Parse([]byte(`{"routes":[{"name":"api","methods":[" get ","post"],"backends":[{"url":"http://a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Routes[0].Methods, ","); got != "GET,POST" {
		t.Fatalf("normalized methods = %q", got)
	}
}

func TestServerHeaderLimitDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.MaxRequestHeaderBytes != 1<<20 {
		t.Fatalf("default max header bytes = %d", cfg.Server.MaxRequestHeaderBytes)
	}
	_, err = Parse([]byte(`{"server":{"max_request_header_bytes":-1},"routes":[{"name":"api","backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "max_request_header_bytes must be positive") {
		t.Fatalf("negative header limit error = %v", err)
	}
}

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

func TestRejectNegativeRouteBodyLimit(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","max_request_body_bytes":-1,"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "max_request_body_bytes cannot be negative") {
		t.Fatalf("expected body-limit validation error, got %v", err)
	}
}
