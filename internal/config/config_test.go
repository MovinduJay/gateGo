package config

import (
	"encoding/json"
	"fmt"
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

func TestParseValidatesAccessCIDRs(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","access":{"allow_cidrs":["not-a-cidr"]},"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "invalid access CIDR") {
		t.Fatalf("invalid CIDR error = %v", err)
	}
}

func TestParseValidatesCredentialedCORS(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","cors":{"enabled":true,"allowed_origins":["*"],"allow_credentials":true},"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "wildcard origin with credentials") {
		t.Fatalf("unsafe CORS error = %v", err)
	}
}

func TestParseRejectsReservedHeaderTransforms(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","headers":{"request_set":{"X-Authenticated-Subject":"admin"}},"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "reserved header") {
		t.Fatalf("reserved header error = %v", err)
	}
}

func TestShadowPolicyDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte(`{"routes":[{"name":"api","shadow":{"url":"http://candidate"},"backends":[{"url":"http://primary"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	shadow := cfg.Routes[0].Shadow
	if shadow.SamplePercent != 100 || shadow.Timeout.Std() != time.Second || shadow.MaxConcurrent != 4 {
		t.Fatalf("unexpected shadow defaults: %+v", shadow)
	}
	_, err = Parse([]byte(`{"routes":[{"name":"api","shadow":{"url":"ftp://candidate","sample_percent":101},"backends":[{"url":"http://primary"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "scheme must be http or https") {
		t.Fatalf("invalid shadow error = %v", err)
	}
}

func TestAuthModesAreDefaultedAndValidated(t *testing.T) {
	cfg, err := Parse([]byte(`{"routes":[{"name":"legacy","auth_required":true,"backends":[{"url":"http://a"}]},{"name":"key","path_prefix":"/key","auth":{"mode":"api_key","api_key_env":"PARTNER_KEY"},"backends":[{"url":"http://b"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]AuthConfig{}
	for _, route := range cfg.Routes {
		modes[route.Name] = route.Auth
	}
	if modes["legacy"].Mode != "jwt" || modes["key"].Mode != "api_key" || modes["key"].Header != "X-API-Key" {
		t.Fatalf("unexpected auth defaults: %+v", modes)
	}
	_, err = Parse([]byte(`{"routes":[{"name":"api","auth":{"mode":"basic"},"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported auth mode") {
		t.Fatalf("unsupported auth error = %v", err)
	}
}

func TestLoadBalancingStrategyValidation(t *testing.T) {
	cfg, err := Parse([]byte(`{"routes":[{"name":"api","load_balancing":"least_connections","backends":[{"url":"http://a"}]}]}`))
	if err != nil || cfg.Routes[0].LoadBalancing != "least_connections" {
		t.Fatalf("least-connections config = %+v error=%v", cfg, err)
	}
	_, err = Parse([]byte(`{"routes":[{"name":"api","load_balancing":"random","backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported load_balancing") {
		t.Fatalf("unsupported strategy error = %v", err)
	}
	_, err = Parse([]byte(`{"routes":[{"name":"api","load_balancing":"consistent_hash","backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "requires hash_key_header") {
		t.Fatalf("missing hash key error = %v", err)
	}
}

func TestRetryStatusDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://a"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(cfg.Routes[0].Resilience.RetryStatuses); got != "[500 502 503 504]" {
		t.Fatalf("default retry statuses = %s", got)
	}
	_, err = Parse([]byte(`{"routes":[{"name":"api","resilience":{"retry_statuses":[200]},"backends":[{"url":"http://a"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "must be between 400 and 599") {
		t.Fatalf("invalid retry status error = %v", err)
	}
}

func TestParseRejectsUnsafeUpstreamURLs(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"missing host", "http://", "host is required"},
		{"wrong scheme", "ftp://backend", "scheme must be http or https"},
		{"embedded credentials", "https://user:secret@backend", "embedded credentials"},
		{"query parameters", "https://backend?token=secret", "query parameters and fragments"},
		{"fragment", "https://backend/#fragment", "query parameters and fragments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"routes":[{"name":"api","backends":[{"url":%q}]}]}`, tt.url)
			_, err := Parse([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestParseRejectsRelativeHealthPath(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","backends":[{"url":"http://backend","health_path":"healthz"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "health_path must be absolute") {
		t.Fatalf("relative health path error = %v", err)
	}
}

func TestParseRejectsNegativeTotalTimeout(t *testing.T) {
	_, err := Parse([]byte(`{"routes":[{"name":"api","resilience":{"total_timeout":"-1s"},"backends":[{"url":"http://backend"}]}]}`))
	if err == nil || !strings.Contains(err.Error(), "total_timeout cannot be negative") {
		t.Fatalf("negative total timeout error = %v", err)
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
