package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type Duration time.Duration

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
	Listen               string   `json:"listen"`
	ReadHeaderTimeout    Duration `json:"read_header_timeout"`
	IdleTimeout          Duration `json:"idle_timeout"`
	ShutdownTimeout      Duration `json:"shutdown_timeout"`
	ConfigReloadInterval Duration `json:"config_reload_interval"`
	MaxRequestBodyBytes  int64    `json:"max_request_body_bytes"`
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
	Methods             []string         `json:"methods"`
	AuthRequired        bool             `json:"auth_required"`
	RateLimit           RateLimitConfig  `json:"rate_limit"`
	Resilience          ResilienceConfig `json:"resilience"`
	Timeout             Duration         `json:"timeout"`
	Retries             int              `json:"retries"`
	MaxRequestBodyBytes int64            `json:"max_request_body_bytes"`
	Backends            []BackendConfig  `json:"backends"`
}

type ResilienceConfig struct {
	CircuitFailureThreshold int      `json:"circuit_failure_threshold"`
	CircuitCooldown         Duration `json:"circuit_cooldown"`
	MaxConcurrentPerBackend int      `json:"max_concurrent_per_backend"`
	MaxConcurrentRetries    int      `json:"max_concurrent_retries"`
	MaxConcurrentRequests   int      `json:"max_concurrent_requests"`
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
		if r.PathPrefix == "" {
			r.PathPrefix = "/"
		}
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
	if cfg.Health.MaxConcurrent < 1 {
		return errors.New("health max_concurrent must be positive")
	}
	if len(cfg.Routes) == 0 {
		return errors.New("config must define at least one route")
	}
	seen := map[string]struct{}{}
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
		if len(r.Backends) == 0 {
			return fmt.Errorf("route %q needs a backend", r.Name)
		}
		if r.MaxRequestBodyBytes < 0 {
			return fmt.Errorf("route %q max_request_body_bytes cannot be negative", r.Name)
		}
		if r.Resilience.CircuitFailureThreshold < 1 || r.Resilience.CircuitCooldown.Std() <= 0 || r.Resilience.MaxConcurrentPerBackend < 1 || r.Resilience.MaxConcurrentRetries < 1 || r.Resilience.MaxConcurrentRequests < 1 {
			return fmt.Errorf("route %q resilience values must be positive", r.Name)
		}
		for _, b := range r.Backends {
			if !strings.HasPrefix(b.URL, "http://") && !strings.HasPrefix(b.URL, "https://") {
				return fmt.Errorf("route %q backend must be http(s)", r.Name)
			}
		}
	}
	return nil
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
