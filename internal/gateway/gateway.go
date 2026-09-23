package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example/gatego/internal/auth"
	"github.com/example/gatego/internal/balancer"
	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/health"
	"github.com/example/gatego/internal/metrics"
	"github.com/example/gatego/internal/observability"
	"github.com/example/gatego/internal/ratelimit"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Route struct {
	Config       config.RouteConfig
	Balancer     *balancer.WeightedRoundRobin
	retrySlots   chan struct{}
	requestSlots chan struct{}
	allowCIDRs   []*net.IPNet
	denyCIDRs    []*net.IPNet
}

type routeSet struct {
	Config   *config.Config
	Routes   []*Route
	Revision string
}

type Gateway struct {
	configPath     string
	routes         atomic.Pointer[routeSet]
	jwt            *auth.Validator
	local          *ratelimit.LocalLimiter
	redis          *ratelimit.RedisLimiter
	metrics        *metrics.Metrics
	client         *http.Client
	logger         *slog.Logger
	adminKey       string
	trustForwarded bool
	reloadMu       sync.Mutex
	configModTime  time.Time
	redisRetryAt   atomic.Int64
	redisWarnAt    atomic.Int64
	maintenance    atomic.Bool
}

func New(configPath string, cfg *config.Config, jwtSecret, adminKey string, redisLimiter *ratelimit.RedisLimiter, m *metrics.Metrics, logger *slog.Logger) (*Gateway, error) {
	g := &Gateway{
		configPath:     configPath,
		jwt:            auth.NewValidator(jwtSecret),
		local:          ratelimit.NewLocal(),
		redis:          redisLimiter,
		metrics:        m,
		logger:         logger,
		adminKey:       adminKey,
		trustForwarded: strings.EqualFold(os.Getenv("GATEGO_TRUST_FORWARDED_HEADERS"), "true"),
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 64
	transport.IdleConnTimeout = 90 * time.Second
	g.client = &http.Client{Transport: otelhttp.NewTransport(transport)}
	set, err := buildRouteSet(cfg)
	if err != nil {
		return nil, err
	}
	g.routes.Store(set)
	g.initializeCircuitMetrics(set)
	return g, nil
}

func (g *Gateway) initializeCircuitMetrics(set *routeSet) {
	for _, route := range set.Routes {
		for _, backend := range route.Balancer.Backends() {
			g.recordCircuitState(route.Config.Name, backend)
			g.metrics.SetDraining(route.Config.Name, backend.URL.Host, backend.Draining.Load())
		}
	}
}

func (g *Gateway) recordCircuitState(route string, backend *balancer.Backend) {
	g.metrics.SetCircuitState(route, backend.URL.Host, backend.Breaker.State().String())
}

func buildRouteSet(cfg *config.Config) (*routeSet, error) {
	set := &routeSet{Config: cfg, Revision: configRevision(cfg)}
	for _, rc := range cfg.Routes {
		lb, err := balancer.New(rc.Backends, rc.Resilience)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		set.Routes = append(set.Routes, &Route{
			Config: rc, Balancer: lb,
			retrySlots: make(chan struct{}, rc.Resilience.MaxConcurrentRetries), requestSlots: make(chan struct{}, rc.Resilience.MaxConcurrentRequests),
			allowCIDRs: parseCIDRs(rc.Access.AllowCIDRs), denyCIDRs: parseCIDRs(rc.Access.DenyCIDRs),
		})
	}
	return set, nil
}

func configRevision(cfg *config.Config) string {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

func (g *Gateway) Reload() error {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return err
	}
	set, err := buildRouteSet(cfg)
	if err != nil {
		return err
	}
	preserveDrainState(g.routes.Load(), set)
	g.routes.Store(set)
	g.initializeCircuitMetrics(set)
	if info, statErr := os.Stat(g.configPath); statErr == nil {
		g.configModTime = info.ModTime()
	}
	g.logger.Info("configuration reloaded", "routes", len(set.Routes), "revision", set.Revision)
	return nil
}

func preserveDrainState(previous, next *routeSet) {
	if previous == nil || next == nil {
		return
	}
	draining := make(map[string]struct{})
	for _, route := range previous.Routes {
		for _, backend := range route.Balancer.Backends() {
			if backend.Draining.Load() {
				draining[route.Config.Name+"\x00"+backend.URL.String()] = struct{}{}
			}
		}
	}
	for _, route := range next.Routes {
		for _, backend := range route.Balancer.Backends() {
			_, shouldDrain := draining[route.Config.Name+"\x00"+backend.URL.String()]
			backend.Draining.Store(shouldDrain)
		}
	}
}

// WatchConfig reloads an on-disk configuration atomically whenever it changes.
// A failed reload leaves the last known-good route set active.
func (g *Gateway) WatchConfig(ctx context.Context, interval time.Duration) {
	if g.configPath == "" || strings.TrimSpace(os.Getenv("GATEGO_CONFIG_JSON")) != "" || interval <= 0 {
		return
	}
	if info, err := os.Stat(g.configPath); err == nil {
		g.reloadMu.Lock()
		g.configModTime = info.ModTime()
		g.reloadMu.Unlock()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			info, err := os.Stat(g.configPath)
			if err != nil {
				continue
			}
			g.reloadMu.Lock()
			changed := info.ModTime().After(g.configModTime)
			g.reloadMu.Unlock()
			if !changed {
				continue
			}
			if err := g.Reload(); err != nil {
				g.logger.Error("automatic config reload failed; keeping last known-good config", "error", err)
			}
		}
	}
}

func (g *Gateway) HealthChecker() *health.Checker {
	set := g.routes.Load()
	return health.New(g.healthTargets, set.Config.Health.Interval.Std(), set.Config.Health.Timeout.Std(),
		set.Config.Health.FailureThreshold, set.Config.Health.SuccessThreshold, set.Config.Health.MaxConcurrent, g.metrics)
}

func (g *Gateway) healthTargets() []health.Target {
	set := g.routes.Load()
	var out []health.Target
	for _, route := range set.Routes {
		for _, b := range route.Balancer.Backends() {
			out = append(out, health.Target{Route: route.Config.Name, Backend: b})
		}
	}
	return out
}

func (g *Gateway) Ready() bool {
	if g.maintenance.Load() {
		return false
	}
	for _, route := range g.readinessSnapshot() {
		if !route.Ready {
			return false
		}
	}
	return true
}

type routeReadiness struct {
	Route    string `json:"route"`
	Ready    bool   `json:"ready"`
	Backends int    `json:"backends"`
	Healthy  int    `json:"healthy"`
	Eligible int    `json:"eligible"`
	Open     int    `json:"open_circuits"`
	Draining int    `json:"draining"`
}

func (g *Gateway) readinessSnapshot() []routeReadiness {
	set := g.routes.Load()
	result := make([]routeReadiness, 0, len(set.Routes))
	for _, route := range set.Routes {
		state := routeReadiness{Route: route.Config.Name}
		for _, backend := range route.Balancer.Backends() {
			state.Backends++
			if backend.Healthy.Load() {
				state.Healthy++
			}
			if backend.Draining.Load() {
				state.Draining++
			}
			if backend.Breaker.State().String() == "open" {
				state.Open++
			}
			if !backend.Draining.Load() && backend.Healthy.Load() && backend.Breaker.Available() {
				state.Eligible++
			}
		}
		state.Ready = state.Eligible > 0
		result = append(result, state)
	}
	return result
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.handleControl(w, r) {
		return
	}
	started := time.Now()
	sanitizeInboundHeaders(r.Header, g.trustForwarded)
	requestID := requestID(r)
	w.Header().Set("X-Request-ID", requestID)
	r.Header.Set("X-Request-ID", requestID)
	traceparent, traceID := observability.RequestTrace(r.Context(), r.Header.Get("traceparent"))
	r.Header.Set("traceparent", traceparent)
	w.Header().Set("X-Trace-ID", traceID)
	if g.maintenance.Load() {
		w.Header().Set("Retry-After", "1")
		g.respondJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "gateway is in maintenance mode", "request_id": requestID})
		return
	}

	route := g.match(r.URL.Path)
	if route == nil {
		g.respondJSON(w, http.StatusNotFound, map[string]any{"error": "no route", "request_id": requestID})
		return
	}
	if !route.allows(clientIdentityIP(r, g.trustForwarded)) {
		g.record(route.Config.Name, "", http.StatusForbidden, started)
		g.respondJSON(w, http.StatusForbidden, map[string]any{"error": "source address is not allowed", "request_id": requestID})
		return
	}
	if g.handleCORS(w, r, route, started) {
		return
	}
	if !config.MethodAllowed(route.Config, r.Method) {
		w.Header().Set("Allow", strings.Join(route.Config.Methods, ", "))
		g.record(route.Config.Name, "", http.StatusMethodNotAllowed, started)
		g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
		return
	}
	select {
	case route.requestSlots <- struct{}{}:
		g.metrics.AddInflight(route.Config.Name, 1)
		defer func() {
			<-route.requestSlots
			g.metrics.AddInflight(route.Config.Name, -1)
		}()
	default:
		g.metrics.IncLoadShed(route.Config.Name)
		g.record(route.Config.Name, "", http.StatusServiceUnavailable, started)
		w.Header().Set("Retry-After", "1")
		g.respondJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "route capacity exhausted", "request_id": requestID})
		return
	}

	identity := clientIdentityIP(r, g.trustForwarded)
	if route.Config.AuthRequired {
		claims, err := g.jwt.ValidateAuthorization(r.Header.Get("Authorization"))
		if err != nil {
			g.record(route.Config.Name, "", http.StatusUnauthorized, started)
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "request_id": requestID})
			return
		}
		identity = claims.Subject
		r.Header.Set("X-Authenticated-Subject", claims.Subject)
		r.Header.Set("X-Authenticated-Role", claims.Role)
	}

	allowed := g.allowRate(r.Context(), route, identity)
	if !allowed {
		g.metrics.IncRateLimited(route.Config.Name)
		g.record(route.Config.Name, "", http.StatusTooManyRequests, started)
		w.Header().Set("Retry-After", "1")
		g.respondJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limit exceeded", "request_id": requestID})
		return
	}

	if isWebSocket(r) {
		g.serveWebSocket(w, r, route, started)
		return
	}
	g.serveHTTP(w, r, route, started)
}

func (g *Gateway) handleControl(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/healthz":
		g.respondJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return true
	case "/readyz":
		if g.Ready() {
			g.respondJSON(w, http.StatusOK, map[string]any{"status": "ready"})
		} else {
			g.respondJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
		}
		return true
	case "/admin/readiness":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		status := http.StatusOK
		if !g.Ready() {
			status = http.StatusServiceUnavailable
		}
		set := g.routes.Load()
		g.respondJSON(w, status, map[string]any{"ready": status == http.StatusOK, "config_revision": set.Revision, "routes": g.readinessSnapshot()})
		return true
	case "/admin/maintenance":
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "GET, POST, DELETE")
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET, POST, or DELETE required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		if r.Method == http.MethodPost {
			g.maintenance.Store(true)
			g.logger.Warn("maintenance mode enabled", "peer", r.RemoteAddr)
		} else if r.Method == http.MethodDelete {
			g.maintenance.Store(false)
			g.logger.Info("maintenance mode disabled", "peer", r.RemoteAddr)
		}
		g.respondJSON(w, http.StatusOK, map[string]any{"maintenance": g.maintenance.Load()})
		return true
	case "/admin/reload":
		if r.Method != http.MethodPost {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		if err := g.Reload(); err != nil {
			g.logger.Error("reload failed", "error", err)
			g.respondJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return true
		}
		g.respondJSON(w, http.StatusOK, map[string]any{"status": "reloaded"})
		return true
	case "/admin/config/validate":
		if r.Method != http.MethodPost {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		limit := g.routes.Load().Config.Server.MaxRequestBodyBytes
		raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			g.respondJSON(w, http.StatusBadRequest, map[string]any{"error": "read configuration: " + err.Error()})
			return true
		}
		if int64(len(raw)) > limit {
			g.respondJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "configuration exceeds size limit", "limit_bytes": limit})
			return true
		}
		candidate, err := config.Parse(raw)
		if err != nil {
			g.respondJSON(w, http.StatusBadRequest, map[string]any{"status": "invalid", "error": err.Error()})
			return true
		}
		g.respondJSON(w, http.StatusOK, map[string]any{"status": "valid", "routes": len(candidate.Routes)})
		return true
	case "/admin/config":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		set := g.routes.Load()
		etag := `"` + set.Revision + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("X-GateGo-Config-Revision", set.Revision)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		g.respondJSON(w, http.StatusOK, set.Config)
		return true
	case "/admin/backends":
		if r.Method != http.MethodGet {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		type backendState struct {
			Route            string `json:"route"`
			URL              string `json:"url"`
			Circuit          string `json:"circuit"`
			Failures         int    `json:"failures"`
			FailureThreshold int    `json:"failure_threshold"`
			RetryAfterMS     int64  `json:"retry_after_ms"`
			Healthy          bool   `json:"healthy"`
			Draining         bool   `json:"draining"`
			InFlight         int    `json:"in_flight"`
			Capacity         int    `json:"capacity"`
		}
		states := []backendState{}
		for _, route := range g.routes.Load().Routes {
			for _, backend := range route.Balancer.Backends() {
				snapshot := backend.Breaker.Snapshot()
				states = append(states, backendState{Route: route.Config.Name, URL: backend.URL.String(), Healthy: backend.Healthy.Load(), Draining: backend.Draining.Load(), Circuit: snapshot.State.String(), Failures: snapshot.Failures, FailureThreshold: snapshot.FailureThreshold, RetryAfterMS: snapshot.RetryAfter.Milliseconds(), InFlight: backend.InFlight(), Capacity: backend.Capacity()})
			}
		}
		g.respondJSON(w, http.StatusOK, states)
		return true
	case "/admin/backends/drain":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST or DELETE required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		routeName := strings.TrimSpace(r.URL.Query().Get("route"))
		backendName := strings.TrimSpace(r.URL.Query().Get("backend"))
		if routeName == "" || backendName == "" {
			g.respondJSON(w, http.StatusBadRequest, map[string]any{"error": "route and backend query parameters are required"})
			return true
		}
		draining := r.Method == http.MethodPost
		updated := false
		g.reloadMu.Lock()
		for _, route := range g.routes.Load().Routes {
			if route.Config.Name != routeName {
				continue
			}
			for _, backend := range route.Balancer.Backends() {
				if backendName != backend.URL.String() && backendName != backend.URL.Host {
					continue
				}
				backend.Draining.Store(draining)
				g.metrics.SetDraining(route.Config.Name, backend.URL.Host, draining)
				updated = true
			}
		}
		g.reloadMu.Unlock()
		if !updated {
			g.respondJSON(w, http.StatusNotFound, map[string]any{"error": "route or backend not found"})
			return true
		}
		status := "active"
		if draining {
			status = "draining"
		}
		g.logger.Info("backend administrative state changed", "route", routeName, "backend", backendName, "status", status)
		g.respondJSON(w, http.StatusOK, map[string]any{"status": status})
		return true
	case "/admin/circuits/reset":
		if r.Method != http.MethodPost {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return true
		}
		if g.adminKey == "" || r.Header.Get("X-Admin-Key") != g.adminKey {
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return true
		}
		routeName := strings.TrimSpace(r.URL.Query().Get("route"))
		backendName := strings.TrimSpace(r.URL.Query().Get("backend"))
		if routeName == "" {
			g.respondJSON(w, http.StatusBadRequest, map[string]any{"error": "route query parameter is required"})
			return true
		}
		reset := 0
		for _, route := range g.routes.Load().Routes {
			if route.Config.Name != routeName {
				continue
			}
			for _, backend := range route.Balancer.Backends() {
				if backendName != "" && backendName != backend.URL.String() && backendName != backend.URL.Host {
					continue
				}
				backend.Breaker.Reset()
				g.recordCircuitState(route.Config.Name, backend)
				reset++
			}
		}
		if reset == 0 {
			g.respondJSON(w, http.StatusNotFound, map[string]any{"error": "route or backend not found"})
			return true
		}
		g.logger.Info("circuit breakers manually reset", "route", routeName, "backend", backendName, "count", reset)
		g.respondJSON(w, http.StatusOK, map[string]any{"status": "reset", "count": reset})
		return true
	}
	return false
}

func (g *Gateway) match(path string) *Route {
	for _, route := range g.routes.Load().Routes {
		if pathMatches(path, route.Config.PathPrefix) {
			return route
		}
	}
	return nil
}

func (g *Gateway) allowRate(ctx context.Context, route *Route, identity string) bool {
	key := route.Config.Name + ":" + identity
	rl := route.Config.RateLimit
	if g.redis != nil {
		now := time.Now().UnixNano()
		if now >= g.redisRetryAt.Load() {
			ok, err := g.redis.Allow(ctx, key, rl.RequestsPerSecond, rl.Burst)
			if err == nil {
				g.redisRetryAt.Store(0)
				return ok
			}
			g.redisRetryAt.Store(time.Now().Add(time.Second).UnixNano())
			if now >= g.redisWarnAt.Load() {
				g.redisWarnAt.Store(time.Now().Add(time.Second).UnixNano())
				g.logger.Warn("redis rate limiter unavailable; using local fallback", "error", err)
			}
		}
	}
	return g.local.Allow(key, rl.RequestsPerSecond, rl.Burst)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request, route *Route, started time.Time) {
	set := g.routes.Load()
	maxBodyBytes := route.Config.MaxRequestBodyBytes
	if maxBodyBytes == 0 {
		maxBodyBytes = set.Config.Server.MaxRequestBodyBytes
	}
	body, err := readBody(r, maxBodyBytes)
	if err != nil {
		g.record(route.Config.Name, "", http.StatusRequestEntityTooLarge, started)
		g.respondJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": err.Error(), "limit_bytes": maxBodyBytes})
		return
	}

	maxAttempts := 1
	if retrySafe(r) {
		maxAttempts += route.Config.Retries
	}
	var lastErr error
attempts:
	for attempt := 0; attempt < maxAttempts; attempt++ {
		retryAdmitted := false
		releaseRetry := func() {}
		if attempt > 0 {
			select {
			case route.retrySlots <- struct{}{}:
				retryAdmitted = true
				g.metrics.AddRetryInflight(route.Config.Name, 1)
				releaseRetry = func() {
					if retryAdmitted {
						<-route.retrySlots
						g.metrics.AddRetryInflight(route.Config.Name, -1)
						retryAdmitted = false
					}
				}
			default:
				lastErr = fmt.Errorf("retry budget exhausted")
				g.metrics.IncRetryRejected(route.Config.Name)
				break attempts
			}
		}
		backend, pickErr := route.Balancer.Pick()
		if pickErr != nil {
			releaseRetry()
			lastErr = pickErr
			if errors.Is(pickErr, balancer.ErrAllBackendsBusy) {
				g.metrics.IncBulkheadRejected(route.Config.Name)
			}
			break
		}
		backendName := backend.URL.Host
		ctx, cancel := context.WithTimeout(r.Context(), route.Config.Timeout.Std())
		upstreamReq, reqErr := buildUpstreamRequest(ctx, r, backend.URL.String(), body, route.Config.PathPrefix, route.Config.StripPathPrefix)
		if reqErr != nil {
			backend.Release()
			releaseRetry()
			cancel()
			lastErr = reqErr
			break
		}
		applyRequestHeaderPolicy(upstreamReq.Header, route.Config.Headers)
		resp, reqErr := g.client.Do(upstreamReq)
		if reqErr != nil {
			backend.Release()
			releaseRetry()
			cancel()
			if r.Context().Err() != nil {
				g.metrics.IncClientCanceled(route.Config.Name, backendName)
				return
			}
			if backend.Breaker.Failure() {
				g.recordCircuitState(route.Config.Name, backend)
			}
			g.metrics.IncUpstreamError(route.Config.Name, backendName)
			lastErr = reqErr
			if attempt+1 < maxAttempts {
				g.metrics.IncRetry(route.Config.Name)
				if !sleepBackoff(r.Context(), attempt) {
					return
				}
				continue
			}
			break
		}

		if retryableStatus(resp.StatusCode) && attempt+1 < maxAttempts {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			backend.Release()
			releaseRetry()
			cancel()
			if backend.Breaker.Failure() {
				g.recordCircuitState(route.Config.Name, backend)
			}
			g.metrics.IncUpstreamError(route.Config.Name, backendName)
			g.metrics.IncRetry(route.Config.Name)
			lastErr = fmt.Errorf("upstream returned %d", resp.StatusCode)
			if !sleepBackoff(r.Context(), attempt) {
				return
			}
			continue
		}

		if resp.StatusCode >= 500 {
			if backend.Breaker.Failure() {
				g.recordCircuitState(route.Config.Name, backend)
			}
		} else {
			if backend.Breaker.Success() {
				g.recordCircuitState(route.Config.Name, backend)
			}
		}
		copyHeaders(w.Header(), resp.Header)
		removeHopHeaders(w.Header())
		applyResponseHeaderPolicy(w.Header(), route.Config.Headers)
		applyCORSHeaders(w.Header(), route.Config.CORS, r.Header.Get("Origin"))
		w.Header().Set("X-GateGo-Backend", backendName)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		backend.Release()
		releaseRetry()
		cancel()
		g.record(route.Config.Name, backendName, resp.StatusCode, started)
		return
	}
	g.logger.Warn("upstream request failed", "route", route.Config.Name, "error", lastErr)
	status, message := upstreamFailureResponse(lastErr)
	g.record(route.Config.Name, "", status, started)
	g.respondJSON(w, status, map[string]any{"error": message})
}

func (g *Gateway) serveWebSocket(w http.ResponseWriter, r *http.Request, route *Route, started time.Time) {
	backend, err := route.Balancer.Pick()
	if err != nil {
		g.record(route.Config.Name, "", http.StatusBadGateway, started)
		g.respondJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream unavailable"})
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(backend.URL)
	proxy.Transport = g.client.Transport
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = backend.URL.Host
		if route.Config.StripPathPrefix {
			req.URL.Path = stripPathPrefix(req.URL.Path, route.Config.PathPrefix)
			req.URL.RawPath = ""
		}
		req.Header.Set("X-GateGo-Proxy", "1")
		applyRequestHeaderPolicy(req.Header, route.Config.Headers)
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		applyResponseHeaderPolicy(resp.Header, route.Config.Headers)
		if backend.Breaker.Success() {
			g.recordCircuitState(route.Config.Name, backend)
		}
		g.record(route.Config.Name, backend.URL.Host, resp.StatusCode, started)
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, e error) {
		if req.Context().Err() != nil {
			g.metrics.IncClientCanceled(route.Config.Name, backend.URL.Host)
			return
		}
		if backend.Breaker.Failure() {
			g.recordCircuitState(route.Config.Name, backend)
		}
		g.metrics.IncUpstreamError(route.Config.Name, backend.URL.Host)
		g.record(route.Config.Name, backend.URL.Host, http.StatusBadGateway, started)
		http.Error(rw, "websocket upstream unavailable", http.StatusBadGateway)
	}
	defer backend.Release()
	w.Header().Set("X-GateGo-Backend", backend.URL.Host)
	proxy.ServeHTTP(w, r)
}

func readBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("request body exceeds %d bytes", max)
	}
	return b, nil
}

func buildUpstreamRequest(ctx context.Context, original *http.Request, base string, body []byte, routePrefix string, stripPrefix bool) (*http.Request, error) {
	path := original.URL.EscapedPath()
	if stripPrefix {
		path = stripPathPrefix(original.URL.Path, routePrefix)
	}
	target := strings.TrimRight(base, "/") + path
	if original.URL.RawQuery != "" {
		target += "?" + original.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, original.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, original.Header)
	removeHopHeaders(req.Header)
	req.Host = req.URL.Host
	if ip := peerIP(original); ip != "" {
		prior := req.Header.Get("X-Forwarded-For")
		if prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+ip)
		} else {
			req.Header.Set("X-Forwarded-For", ip)
		}
	}
	req.Header.Set("X-GateGo-Proxy", "1")
	return req, nil
}

func stripPathPrefix(path, prefix string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	stripped := strings.TrimPrefix(path, prefix)
	if stripped == "" {
		return "/"
	}
	if !strings.HasPrefix(stripped, "/") {
		return "/" + stripped
	}
	return stripped
}

func applyRequestHeaderPolicy(header http.Header, policy config.HeaderConfig) {
	for _, name := range policy.RequestRemove {
		header.Del(name)
	}
	for name, value := range policy.RequestSet {
		header.Set(name, value)
	}
}

func applyResponseHeaderPolicy(header http.Header, policy config.HeaderConfig) {
	for _, name := range policy.ResponseRemove {
		header.Del(name)
	}
	for name, value := range policy.ResponseSet {
		header.Set(name, value)
	}
}

func retrySafe(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	case http.MethodPost, http.MethodPatch:
		return r.Header.Get("Idempotency-Key") != ""
	default:
		return false
	}
}

func retryableStatus(code int) bool {
	return code == http.StatusInternalServerError || code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func upstreamFailureResponse(err error) (int, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "upstream timeout"
	}
	return http.StatusBadGateway, "upstream unavailable"
}

func sleepBackoff(ctx context.Context, attempt int) bool {
	base := 50 * time.Millisecond * time.Duration(1<<min(attempt, 3))
	jitter := time.Duration(mathrand.Int63n(int64(base/2) + 1))
	t := time.NewTimer(base + jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func requestID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("X-Request-ID")); id != "" {
		return id
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func clientIdentityIP(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	return peerIP(r)
}

func parseCIDRs(values []string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, _ := net.ParseCIDR(value)
		result = append(result, network)
	}
	return result
}

func (r *Route) allows(address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return len(r.allowCIDRs) == 0
	}
	for _, network := range r.denyCIDRs {
		if network.Contains(ip) {
			return false
		}
	}
	if len(r.allowCIDRs) == 0 {
		return true
	}
	for _, network := range r.allowCIDRs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (g *Gateway) handleCORS(w http.ResponseWriter, r *http.Request, route *Route, started time.Time) bool {
	policy := route.Config.CORS
	origin := r.Header.Get("Origin")
	if !policy.Enabled || origin == "" {
		return false
	}
	if !corsOriginAllowed(policy, origin) {
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			g.record(route.Config.Name, "", http.StatusForbidden, started)
			g.respondJSON(w, http.StatusForbidden, map[string]any{"error": "CORS origin is not allowed"})
			return true
		}
		return false
	}
	applyCORSHeaders(w.Header(), policy, origin)
	requestedMethod := strings.ToUpper(strings.TrimSpace(r.Header.Get("Access-Control-Request-Method")))
	if r.Method != http.MethodOptions || requestedMethod == "" {
		return false
	}
	if !containsFold(policy.AllowedMethods, requestedMethod) {
		g.record(route.Config.Name, "", http.StatusForbidden, started)
		g.respondJSON(w, http.StatusForbidden, map[string]any{"error": "CORS method is not allowed"})
		return true
	}
	w.WriteHeader(http.StatusNoContent)
	g.record(route.Config.Name, "", http.StatusNoContent, started)
	return true
}

func applyCORSHeaders(header http.Header, policy config.CORSConfig, origin string) {
	if !policy.Enabled || origin == "" || !corsOriginAllowed(policy, origin) {
		return
	}
	allowedOrigin := origin
	if containsFold(policy.AllowedOrigins, "*") {
		allowedOrigin = "*"
	} else {
		header.Add("Vary", "Origin")
	}
	header.Set("Access-Control-Allow-Origin", allowedOrigin)
	if len(policy.AllowedMethods) > 0 {
		header.Set("Access-Control-Allow-Methods", strings.Join(policy.AllowedMethods, ", "))
	}
	if len(policy.AllowedHeaders) > 0 {
		header.Set("Access-Control-Allow-Headers", strings.Join(policy.AllowedHeaders, ", "))
	}
	if len(policy.ExposedHeaders) > 0 {
		header.Set("Access-Control-Expose-Headers", strings.Join(policy.ExposedHeaders, ", "))
	}
	if policy.AllowCredentials {
		header.Set("Access-Control-Allow-Credentials", "true")
	}
	if policy.MaxAgeSeconds > 0 {
		header.Set("Access-Control-Max-Age", strconv.Itoa(policy.MaxAgeSeconds))
	}
}

func corsOriginAllowed(policy config.CORSConfig, origin string) bool {
	return containsFold(policy.AllowedOrigins, "*") || containsFold(policy.AllowedOrigins, origin)
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(value, wanted) {
			return true
		}
	}
	return false
}

func sanitizeInboundHeaders(h http.Header, trustForwarded bool) {
	for _, k := range []string{"X-Authenticated-Subject", "X-Authenticated-Role", "X-GateGo-Proxy"} {
		h.Del(k)
	}
	if !trustForwarded {
		h.Del("X-Forwarded-For")
		h.Del("X-Real-IP")
		h.Del("Forwarded")
	}
}

func pathMatches(path, prefix string) bool {
	if prefix == "/" {
		return true
	}
	prefix = strings.TrimSuffix(prefix, "/")
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func copyHeaders(dst, src http.Header) {
	for k, values := range src {
		dst.Del(k)
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

func removeHopHeaders(h http.Header) {
	if connection := h.Get("Connection"); connection != "" {
		for _, name := range strings.Split(connection, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}

func (g *Gateway) record(route, backend string, status int, started time.Time) {
	if g.metrics == nil {
		return
	}
	g.metrics.IncRequest(route, strconv.Itoa(status))
	g.metrics.Observe(route, backend, time.Since(started))
}

func (g *Gateway) respondJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (g *Gateway) Close() error {
	if g.redis != nil {
		return g.redis.Close()
	}
	return nil
}

func RedisFromEnv(ctx context.Context, logger *slog.Logger) *ratelimit.RedisLimiter {
	addr := strings.TrimSpace(os.Getenv("GATEGO_REDIS_ADDR"))
	if addr == "" {
		return nil
	}
	db, _ := strconv.Atoi(os.Getenv("GATEGO_REDIS_DB"))
	r := ratelimit.NewRedis(addr, os.Getenv("GATEGO_REDIS_PASSWORD"), db)
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := r.Ping(pingCtx); err != nil {
		logger.Warn("redis unavailable at startup; limiter will fall back locally", "error", err)
	}
	return r
}
