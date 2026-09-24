package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	shadowSlots  chan struct{}
	apiKey       []byte
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
	g.metrics.SetConfigRevision(set.Revision)
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
		lb, err := balancer.NewWithStrategy(rc.Backends, rc.Resilience, rc.LoadBalancing)
		if err != nil {
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		var apiKey []byte
		if rc.Auth.Mode == "api_key" {
			secret := os.Getenv(rc.Auth.APIKeyEnv)
			if secret == "" {
				return nil, fmt.Errorf("route %s: environment variable %s is required for API-key auth", rc.Name, rc.Auth.APIKeyEnv)
			}
			apiKey = []byte(secret)
		}
		set.Routes = append(set.Routes, &Route{
			Config: rc, Balancer: lb,
			retrySlots: make(chan struct{}, rc.Resilience.MaxConcurrentRetries), requestSlots: make(chan struct{}, rc.Resilience.MaxConcurrentRequests),
			allowCIDRs: parseCIDRs(rc.Access.AllowCIDRs), denyCIDRs: parseCIDRs(rc.Access.DenyCIDRs),
			shadowSlots: makeShadowSlots(rc.Shadow),
			apiKey:      apiKey,
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
		g.metrics.IncConfigReload("failure")
		return err
	}
	set, err := buildRouteSet(cfg)
	if err != nil {
		g.metrics.IncConfigReload("failure")
		return err
	}
	preserveDrainState(g.routes.Load(), set)
	g.routes.Store(set)
	g.metrics.SetConfigRevision(set.Revision)
	g.metrics.IncConfigReload("success")
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
	switch route.Config.Auth.Mode {
	case "jwt":
		claims, err := g.jwt.ValidateAuthorization(r.Header.Get("Authorization"))
		if err != nil {
			g.record(route.Config.Name, "", http.StatusUnauthorized, started)
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "request_id": requestID})
			return
		}
		identity = claims.Subject
		r.Header.Set("X-Authenticated-Subject", claims.Subject)
		r.Header.Set("X-Authenticated-Role", claims.Role)
	case "api_key":
		provided := []byte(r.Header.Get(route.Config.Auth.Header))
		if len(provided) != len(route.apiKey) || subtle.ConstantTimeCompare(provided, route.apiKey) != 1 {
			g.record(route.Config.Name, "", http.StatusUnauthorized, started)
			g.respondJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "request_id": requestID})
			return
		}
		digest := sha256.Sum256(route.apiKey)
		identity = "api-key:" + hex.EncodeToString(digest[:8])
		r.Header.Del(route.Config.Auth.Header)
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
	g.maybeShadow(r, route, body)

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
		upstreamReq.Header.Set("X-GateGo-Route", route.Config.Name)
		upstreamReq.Header.Set("X-GateGo-Attempt", strconv.Itoa(attempt+1))
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

		if retryableStatus(route.Config.Resilience.RetryStatuses, resp.StatusCode) && attempt+1 < maxAttempts {
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
		req.Header.Set("X-GateGo-Route", route.Config.Name)
		req.Header.Set("X-GateGo-Attempt", "1")
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

func makeShadowSlots(policy config.ShadowConfig) chan struct{} {
	if policy.URL == "" {
		return nil
	}
	return make(chan struct{}, policy.MaxConcurrent)
}

func (g *Gateway) maybeShadow(original *http.Request, route *Route, body []byte) {
	policy := route.Config.Shadow
	if policy.URL == "" || !retrySafe(original) || mathrand.Float64()*100 >= policy.SamplePercent {
		return
	}
	select {
	case route.shadowSlots <- struct{}{}:
	default:
		g.metrics.IncShadowRequest(route.Config.Name, "skipped")
		return
	}
	bodyCopy := append([]byte(nil), body...)
	go func() {
		defer func() { <-route.shadowSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), policy.Timeout.Std())
		defer cancel()
		req, err := buildUpstreamRequest(ctx, original, policy.URL, bodyCopy, route.Config.PathPrefix, route.Config.StripPathPrefix)
		if err != nil {
			g.metrics.IncShadowRequest(route.Config.Name, "failure")
			return
		}
		applyRequestHeaderPolicy(req.Header, route.Config.Headers)
		req.Header.Set("X-GateGo-Shadow", "true")
		req.Header.Set("X-GateGo-Route", route.Config.Name)
		req.Header.Set("X-GateGo-Attempt", "1")
		resp, err := g.client.Do(req)
		if err != nil {
			g.metrics.IncShadowRequest(route.Config.Name, "failure")
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			g.metrics.IncShadowRequest(route.Config.Name, "failure")
			return
		}
		g.metrics.IncShadowRequest(route.Config.Name, "success")
	}()
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

func retryableStatus(statuses []int, code int) bool {
	for _, status := range statuses {
		if status == code {
			return true
		}
	}
	return false
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
	for _, k := range []string{"X-Authenticated-Subject", "X-Authenticated-Role", "X-GateGo-Proxy", "X-GateGo-Shadow", "X-GateGo-Route", "X-GateGo-Attempt"} {
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
