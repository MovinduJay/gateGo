package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
)

type Route struct {
	Config   config.RouteConfig
	Balancer *balancer.WeightedRoundRobin
}

type routeSet struct {
	Config *config.Config
	Routes []*Route
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
	g.client = &http.Client{Transport: transport}
	set, err := buildRouteSet(cfg)
	if err != nil {
		return nil, err
	}
	g.routes.Store(set)
	return g, nil
}

func buildRouteSet(cfg *config.Config) (*routeSet, error) {
	set := &routeSet{Config: cfg}
	for _, rc := range cfg.Routes {
		lb, err := balancer.New(rc.Backends)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		set.Routes = append(set.Routes, &Route{Config: rc, Balancer: lb})
	}
	return set, nil
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
	g.routes.Store(set)
	if info, statErr := os.Stat(g.configPath); statErr == nil {
		g.configModTime = info.ModTime()
	}
	g.logger.Info("configuration reloaded", "routes", len(set.Routes))
	return nil
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
		set.Config.Health.FailureThreshold, set.Config.Health.SuccessThreshold, g.metrics)
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
	set := g.routes.Load()
	for _, route := range set.Routes {
		ok := false
		for _, b := range route.Balancer.Backends() {
			if b.Healthy.Load() {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
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
	traceparent, traceID := observability.EnsureTraceparent(r.Header.Get("traceparent"))
	r.Header.Set("traceparent", traceparent)
	w.Header().Set("X-Trace-ID", traceID)

	route := g.match(r.URL.Path)
	if route == nil {
		g.respondJSON(w, http.StatusNotFound, map[string]any{"error": "no route", "request_id": requestID})
		return
	}
	if !config.MethodAllowed(route.Config, r.Method) {
		w.Header().Set("Allow", strings.Join(route.Config.Methods, ", "))
		g.record(route.Config.Name, "", http.StatusMethodNotAllowed, started)
		g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "request_id": requestID})
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
			Route   string `json:"route"`
			URL     string `json:"url"`
			Circuit string `json:"circuit"`
			Healthy bool   `json:"healthy"`
		}
		states := []backendState{}
		for _, route := range g.routes.Load().Routes {
			for _, backend := range route.Balancer.Backends() {
				states = append(states, backendState{Route: route.Config.Name, URL: backend.URL.String(), Healthy: backend.Healthy.Load(), Circuit: backend.Breaker.State().String()})
			}
		}
		g.respondJSON(w, http.StatusOK, states)
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
		ok, err := g.redis.Allow(ctx, key, rl.RequestsPerSecond, rl.Burst)
		if err == nil {
			return ok
		}
		g.logger.Warn("redis rate limiter unavailable; using local fallback", "error", err)
	}
	return g.local.Allow(key, rl.RequestsPerSecond, rl.Burst)
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request, route *Route, started time.Time) {
	set := g.routes.Load()
	body, err := readBody(r, set.Config.Server.MaxRequestBodyBytes)
	if err != nil {
		g.record(route.Config.Name, "", http.StatusRequestEntityTooLarge, started)
		g.respondJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": err.Error()})
		return
	}

	maxAttempts := 1
	if retrySafe(r) {
		maxAttempts += route.Config.Retries
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		backend, pickErr := route.Balancer.Pick()
		if pickErr != nil {
			lastErr = pickErr
			break
		}
		backendName := backend.URL.Host
		ctx, cancel := context.WithTimeout(r.Context(), route.Config.Timeout.Std())
		upstreamReq, reqErr := buildUpstreamRequest(ctx, r, backend.URL.String(), body)
		if reqErr != nil {
			cancel()
			lastErr = reqErr
			break
		}
		resp, reqErr := g.client.Do(upstreamReq)
		if reqErr != nil {
			cancel()
			backend.Breaker.Failure()
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
			cancel()
			backend.Breaker.Failure()
			g.metrics.IncUpstreamError(route.Config.Name, backendName)
			g.metrics.IncRetry(route.Config.Name)
			lastErr = fmt.Errorf("upstream returned %d", resp.StatusCode)
			if !sleepBackoff(r.Context(), attempt) {
				return
			}
			continue
		}

		if resp.StatusCode >= 500 {
			backend.Breaker.Failure()
		} else {
			backend.Breaker.Success()
		}
		copyHeaders(w.Header(), resp.Header)
		removeHopHeaders(w.Header())
		w.Header().Set("X-GateGo-Backend", backendName)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		resp.Body.Close()
		cancel()
		g.record(route.Config.Name, backendName, resp.StatusCode, started)
		return
	}
	g.logger.Warn("upstream request failed", "route", route.Config.Name, "error", lastErr)
	g.record(route.Config.Name, "", http.StatusBadGateway, started)
	g.respondJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream unavailable"})
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
		req.Header.Set("X-GateGo-Proxy", "1")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		backend.Breaker.Success()
		g.record(route.Config.Name, backend.URL.Host, resp.StatusCode, started)
		return nil
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, e error) {
		backend.Breaker.Failure()
		g.metrics.IncUpstreamError(route.Config.Name, backend.URL.Host)
		g.record(route.Config.Name, backend.URL.Host, http.StatusBadGateway, started)
		http.Error(rw, "websocket upstream unavailable", http.StatusBadGateway)
	}
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

func buildUpstreamRequest(ctx context.Context, original *http.Request, base string, body []byte) (*http.Request, error) {
	target := strings.TrimRight(base, "/") + original.URL.RequestURI()
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
