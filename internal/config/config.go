package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("duration must be a string such as 3s")
	}
	*d = Duration(n)
	return nil
}
func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Server        ServerConfig        `json:"server"`
	Health        HealthConfig        `json:"health"`
	Observability ObservabilityConfig `json:"observability"`
	Routes        []RouteConfig       `json:"routes"`
}
type ServerConfig struct {
	Listen                string   `json:"listen"`
	ReadHeaderTimeout     Duration `json:"read_header_timeout"`
	IdleTimeout           Duration `json:"idle_timeout"`
	ShutdownTimeout       Duration `json:"shutdown_timeout"`
	ConfigReloadInterval  Duration `json:"config_reload_interval"`
	MaxRequestBodyBytes   int64    `json:"max_request_body_bytes"`
	MaxRequestHeaderBytes int      `json:"max_request_header_bytes"`
}
type HealthConfig struct {
	Interval         Duration `json:"interval"`
	Timeout          Duration `json:"timeout"`
	FailureThreshold int      `json:"failure_threshold"`
	SuccessThreshold int      `json:"success_threshold"`
	MaxConcurrent    int      `json:"max_concurrent"`
}
type ObservabilityConfig struct {
	ServiceName string `json:"service_name"`
}
type RouteConfig struct {
	Name                string           `json:"name"`
	PathPrefix          string           `json:"path_prefix"`
	StripPathPrefix     bool             `json:"strip_path_prefix"`
	LoadBalancing       string           `json:"load_balancing"`
	Methods             []string         `json:"methods"`
	AuthRequired        bool             `json:"auth_required"`
	Auth                AuthConfig       `json:"auth"`
	RateLimit           RateLimitConfig  `json:"rate_limit"`
	Resilience          ResilienceConfig `json:"resilience"`
	Timeout             Duration         `json:"timeout"`
	Retries             int              `json:"retries"`
	MaxRequestBodyBytes int64            `json:"max_request_body_bytes"`
	Access              AccessConfig     `json:"access"`
	CORS                CORSConfig       `json:"cors"`
	Headers             HeaderConfig     `json:"headers"`
	Shadow              ShadowConfig     `json:"shadow"`
	Backends            []BackendConfig  `json:"backends"`
}

type AuthConfig struct {
	Mode      string `json:"mode"`
	APIKeyEnv string `json:"api_key_env"`
	Header    string `json:"header"`
}

type AccessConfig struct {
	AllowCIDRs []string `json:"allow_cidrs"`
	DenyCIDRs  []string `json:"deny_cidrs"`
}

type CORSConfig struct {
	Enabled          bool     `json:"enabled"`
	AllowedOrigins   []string `json:"allowed_origins"`
	AllowedMethods   []string `json:"allowed_methods"`
	AllowedHeaders   []string `json:"allowed_headers"`
	ExposedHeaders   []string `json:"exposed_headers"`
	AllowCredentials bool     `json:"allow_credentials"`
	MaxAgeSeconds    int      `json:"max_age_seconds"`
}

type HeaderConfig struct {
	RequestSet     map[string]string `json:"request_set"`
	RequestRemove  []string          `json:"request_remove"`
	ResponseSet    map[string]string `json:"response_set"`
	ResponseRemove []string          `json:"response_remove"`
}

type ShadowConfig struct {
	URL           string   `json:"url"`
	SamplePercent float64  `json:"sample_percent"`
	Timeout       Duration `json:"timeout"`
	MaxConcurrent int      `json:"max_concurrent"`
}

type ResilienceConfig struct {
	CircuitFailureThreshold int      `json:"circuit_failure_threshold"`
	CircuitCooldown         Duration `json:"circuit_cooldown"`
	MaxConcurrentPerBackend int      `json:"max_concurrent_per_backend"`
	MaxConcurrentRetries    int      `json:"max_concurrent_retries"`
	MaxConcurrentRequests   int      `json:"max_concurrent_requests"`
	RetryStatuses           []int    `json:"retry_statuses"`
}
type RateLimitConfig struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int     `json:"burst"`
}
type BackendConfig struct {
	URL        string `json:"url"`
	Weight     int    `json:"weight"`
	HealthPath string `json:"health_path"`
}

func Load(path string) (*Config, error) {
	if raw := strings.TrimSpace(os.Getenv("GATEGO_CONFIG_JSON")); raw != "" {
		return Parse([]byte(raw))
	}
	if path == "" {
		path = "configs/gatego.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}
func Parse(b []byte) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	sort.SliceStable(cfg.Routes, func(i, j int) bool { return len(cfg.Routes[i].PathPrefix) > len(cfg.Routes[j].PathPrefix) })
	return &cfg, nil
}
func applyDefaults(cfg *Config) {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}
	if cfg.Server.ReadHeaderTimeout == 0 {
		cfg.Server.ReadHeaderTimeout = Duration(5 * time.Second)
	}
	if cfg.Server.IdleTimeout == 0 {
		cfg.Server.IdleTimeout = Duration(60 * time.Second)
	}
	if cfg.Server.ShutdownTimeout == 0 {
		cfg.Server.ShutdownTimeout = Duration(10 * time.Second)
	}
	if cfg.Server.ConfigReloadInterval == 0 {
		cfg.Server.ConfigReloadInterval = Duration(2 * time.Second)
	}
	if cfg.Server.MaxRequestBodyBytes == 0 {
		cfg.Server.MaxRequestBodyBytes = 2 << 20
	}
	if cfg.Server.MaxRequestHeaderBytes == 0 {
		cfg.Server.MaxRequestHeaderBytes = 1 << 20
	}
	if cfg.Health.Interval == 0 {
		cfg.Health.Interval = Duration(5 * time.Second)
	}
	if cfg.Health.Timeout == 0 {
		cfg.Health.Timeout = Duration(2 * time.Second)
	}
	if cfg.Health.FailureThreshold == 0 {
		cfg.Health.FailureThreshold = 2
	}
	if cfg.Health.SuccessThreshold == 0 {
		cfg.Health.SuccessThreshold = 1
	}
	if cfg.Health.MaxConcurrent == 0 {
		cfg.Health.MaxConcurrent = 16
	}
	if cfg.Observability.ServiceName == "" {
		cfg.Observability.ServiceName = "gatego"
	}
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.Auth.Mode == "" {
			if r.AuthRequired {
				r.Auth.Mode = "jwt"
			} else {
				r.Auth.Mode = "none"
			}
		}
		r.Auth.Mode = strings.ToLower(strings.TrimSpace(r.Auth.Mode))
		if r.Auth.Mode == "api_key" && r.Auth.Header == "" {
			r.Auth.Header = "X-API-Key"
		}
		for j := range r.Methods {
			r.Methods[j] = strings.ToUpper(strings.TrimSpace(r.Methods[j]))
		}
		for j := range r.CORS.AllowedMethods {
			r.CORS.AllowedMethods[j] = strings.ToUpper(strings.TrimSpace(r.CORS.AllowedMethods[j]))
		}
		if r.PathPrefix == "" {
			r.PathPrefix = "/"
		}
		if r.LoadBalancing == "" {
			r.LoadBalancing = "weighted_round_robin"
		}
		r.LoadBalancing = strings.ToLower(strings.TrimSpace(r.LoadBalancing))
		if r.Timeout == 0 {
			r.Timeout = Duration(3 * time.Second)
		}
		if r.RateLimit.RequestsPerSecond <= 0 {
			r.RateLimit.RequestsPerSecond = 50
		}
		if r.RateLimit.Burst <= 0 {
			r.RateLimit.Burst = 100
		}
		if r.Resilience.CircuitFailureThreshold == 0 {
			r.Resilience.CircuitFailureThreshold = 20
		}
		if r.Resilience.CircuitCooldown == 0 {
			r.Resilience.CircuitCooldown = Duration(10 * time.Second)
		}
		if r.Resilience.MaxConcurrentPerBackend == 0 {
			r.Resilience.MaxConcurrentPerBackend = 16
		}
		if r.Resilience.MaxConcurrentRetries == 0 {
			r.Resilience.MaxConcurrentRetries = 8
		}
		if r.Resilience.MaxConcurrentRequests == 0 {
			r.Resilience.MaxConcurrentRequests = 256
		}
		if len(r.Resilience.RetryStatuses) == 0 {
			r.Resilience.RetryStatuses = []int{500, 502, 503, 504}
		}
		if r.Shadow.URL != "" {
			if r.Shadow.SamplePercent == 0 {
				r.Shadow.SamplePercent = 100
			}
			if r.Shadow.Timeout == 0 {
				r.Shadow.Timeout = Duration(time.Second)
			}
			if r.Shadow.MaxConcurrent == 0 {
				r.Shadow.MaxConcurrent = 4
			}
		}
		for j := range r.Backends {
			if r.Backends[j].Weight <= 0 {
				r.Backends[j].Weight = 1
			}
			if r.Backends[j].HealthPath == "" {
				r.Backends[j].HealthPath = "/healthz"
			}
		}
	}
}
func validate(cfg *Config) error {
	if cfg.Server.MaxRequestHeaderBytes < 1 {
		return errors.New("server max_request_header_bytes must be positive")
	}
	if cfg.Health.MaxConcurrent < 1 {
		return errors.New("health max_concurrent must be positive")
	}
	if len(cfg.Routes) == 0 {
		return errors.New("config must define at least one route")
	}
	seen := map[string]struct{}{}
	seenPaths := map[string]string{}
	for _, r := range cfg.Routes {
		if strings.TrimSpace(r.Name) == "" {
			return errors.New("every route needs a name")
		}
		if _, ok := seen[r.Name]; ok {
			return fmt.Errorf("duplicate route %q", r.Name)
		}
		seen[r.Name] = struct{}{}
		if !strings.HasPrefix(r.PathPrefix, "/") {
			return fmt.Errorf("route %q path_prefix must start with /", r.Name)
		}
		if r.LoadBalancing != "weighted_round_robin" && r.LoadBalancing != "least_connections" {
			return fmt.Errorf("route %q has unsupported load_balancing strategy %q", r.Name, r.LoadBalancing)
		}
		path := strings.TrimSuffix(r.PathPrefix, "/")
		if path == "" {
			path = "/"
		}
		if previous, ok := seenPaths[path]; ok {
			return fmt.Errorf("routes %q and %q have duplicate path_prefix %q", previous, r.Name, path)
		}
		seenPaths[path] = r.Name
		methods := map[string]struct{}{}
		for _, method := range r.Methods {
			if method == "" {
				return fmt.Errorf("route %q contains a blank method", r.Name)
			}
			if _, ok := methods[method]; ok {
				return fmt.Errorf("route %q contains duplicate method %q", r.Name, method)
			}
			methods[method] = struct{}{}
		}
		if len(r.Backends) == 0 {
			return fmt.Errorf("route %q needs a backend", r.Name)
		}
		if r.MaxRequestBodyBytes < 0 {
			return fmt.Errorf("route %q max_request_body_bytes cannot be negative", r.Name)
		}
		switch r.Auth.Mode {
		case "none", "jwt":
		case "api_key":
			if strings.TrimSpace(r.Auth.APIKeyEnv) == "" {
				return fmt.Errorf("route %q API-key auth requires api_key_env", r.Name)
			}
			if strings.TrimSpace(r.Auth.Header) == "" {
				return fmt.Errorf("route %q API-key auth requires a header", r.Name)
			}
		default:
			return fmt.Errorf("route %q has unsupported auth mode %q", r.Name, r.Auth.Mode)
		}
		for _, cidr := range append(append([]string{}, r.Access.AllowCIDRs...), r.Access.DenyCIDRs...) {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("route %q contains invalid access CIDR %q", r.Name, cidr)
			}
		}
		if r.CORS.Enabled {
			if len(r.CORS.AllowedOrigins) == 0 {
				return fmt.Errorf("route %q CORS requires at least one allowed origin", r.Name)
			}
			for _, origin := range r.CORS.AllowedOrigins {
				if origin == "*" && r.CORS.AllowCredentials {
					return fmt.Errorf("route %q CORS cannot combine wildcard origin with credentials", r.Name)
				}
			}
			if r.CORS.MaxAgeSeconds < 0 {
				return fmt.Errorf("route %q CORS max_age_seconds cannot be negative", r.Name)
			}
		}
		for _, header := range headerPolicyNames(r.Headers) {
			if isReservedHeader(header) {
				return fmt.Errorf("route %q header policy cannot modify reserved header %q", r.Name, header)
			}
			if r.Auth.Mode == "api_key" && strings.EqualFold(header, r.Auth.Header) {
				return fmt.Errorf("route %q header policy cannot modify its API-key header %q", r.Name, header)
			}
		}
		if r.Resilience.CircuitFailureThreshold < 1 || r.Resilience.CircuitCooldown.Std() <= 0 || r.Resilience.MaxConcurrentPerBackend < 1 || r.Resilience.MaxConcurrentRetries < 1 || r.Resilience.MaxConcurrentRequests < 1 {
			return fmt.Errorf("route %q resilience values must be positive", r.Name)
		}
		retryStatuses := map[int]struct{}{}
		for _, status := range r.Resilience.RetryStatuses {
			if status < 400 || status > 599 {
				return fmt.Errorf("route %q retry status %d must be between 400 and 599", r.Name, status)
			}
			if _, exists := retryStatuses[status]; exists {
				return fmt.Errorf("route %q contains duplicate retry status %d", r.Name, status)
			}
			retryStatuses[status] = struct{}{}
		}
		if r.Shadow.URL != "" {
			if err := validateUpstreamURL(r.Shadow.URL); err != nil {
				return fmt.Errorf("route %q shadow URL: %w", r.Name, err)
			}
			if r.Shadow.SamplePercent <= 0 || r.Shadow.SamplePercent > 100 {
				return fmt.Errorf("route %q shadow sample_percent must be greater than 0 and at most 100", r.Name)
			}
			if r.Shadow.Timeout.Std() <= 0 || r.Shadow.MaxConcurrent < 1 {
				return fmt.Errorf("route %q shadow timeout and max_concurrent must be positive", r.Name)
			}
		}
		backends := map[string]struct{}{}
		for _, b := range r.Backends {
			if err := validateUpstreamURL(b.URL); err != nil {
				return fmt.Errorf("route %q backend %q: %w", r.Name, b.URL, err)
			}
			if !strings.HasPrefix(b.HealthPath, "/") {
				return fmt.Errorf("route %q backend %q health_path must be absolute", r.Name, b.URL)
			}
			normalizedURL := strings.TrimSuffix(b.URL, "/")
			if _, ok := backends[normalizedURL]; ok {
				return fmt.Errorf("route %q contains duplicate backend %q", r.Name, normalizedURL)
			}
			backends[normalizedURL] = struct{}{}
		}
	}
	return nil
}

func validateUpstreamURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	if parsed.Host == "" {
		return errors.New("host is required")
	}
	if parsed.User != nil {
		return errors.New("embedded credentials are not allowed")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("query parameters and fragments are not allowed")
	}
	return nil
}

func headerPolicyNames(policy HeaderConfig) []string {
	result := append([]string{}, policy.RequestRemove...)
	result = append(result, policy.ResponseRemove...)
	for name := range policy.RequestSet {
		result = append(result, name)
	}
	for name := range policy.ResponseSet {
		result = append(result, name)
	}
	return result
}

func isReservedHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade", "x-authenticated-subject", "x-authenticated-role", "x-gatego-proxy", "x-gatego-shadow", "x-gatego-route", "x-gatego-attempt":
		return true
	default:
		return false
	}
}
func MethodAllowed(route RouteConfig, method string) bool {
	if len(route.Methods) == 0 {
		return true
	}
	for _, m := range route.Methods {
		if strings.EqualFold(m, method) {
			return true
		}
	}
	return false
}
