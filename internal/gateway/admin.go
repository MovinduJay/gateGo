package gateway

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/eventlog"
)

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
		if !g.requireAdmin(w, r) {
			return true
		}
		set := g.routes.Load()
		routes := g.readinessSnapshot(set)
		ready := routesReady(routes, g.maintenance.Load())
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		g.respondJSON(w, status, map[string]any{"ready": ready, "config_revision": set.Revision, "routes": routes})
		return true
	case "/admin/events":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return true
		}
		if !g.requireAdmin(w, r) {
			return true
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 200 {
				g.respondJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be between 1 and 200"})
				return true
			}
			limit = parsed
		}
		var after uint64
		if raw := r.URL.Query().Get("after"); raw != "" {
			parsed, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				g.respondJSON(w, http.StatusBadRequest, map[string]any{"error": "after must be an unsigned event sequence"})
				return true
			}
			after = parsed
		}
		events := g.events.List(eventlog.Filter{Type: r.URL.Query().Get("type"), Route: r.URL.Query().Get("route"), Limit: limit, After: after})
		g.respondJSON(w, http.StatusOK, map[string]any{"events": events, "latest_sequence": g.events.LatestSequence()})
		return true
	case "/admin/maintenance":
		if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "GET, POST, DELETE")
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET, POST, or DELETE required"})
			return true
		}
		if !g.requireAdmin(w, r) {
			return true
		}
		if r.Method == http.MethodPost {
			g.maintenance.Store(true)
			g.events.Add(eventlog.Event{Type: "maintenance", Details: map[string]any{"enabled": true, "peer": r.RemoteAddr}})
			g.logger.Warn("maintenance mode enabled", "peer", r.RemoteAddr)
		} else if r.Method == http.MethodDelete {
			g.maintenance.Store(false)
			g.events.Add(eventlog.Event{Type: "maintenance", Details: map[string]any{"enabled": false, "peer": r.RemoteAddr}})
			g.logger.Info("maintenance mode disabled", "peer", r.RemoteAddr)
		}
		g.respondJSON(w, http.StatusOK, map[string]any{"maintenance": g.maintenance.Load()})
		return true
	case "/admin/reload":
		if r.Method != http.MethodPost {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return true
		}
		if !g.requireAdmin(w, r) {
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
		if !g.requireAdmin(w, r) {
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
		if !g.requireAdmin(w, r) {
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
		if !g.requireAdmin(w, r) {
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
	case "/admin/routes":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return true
		}
		if !g.requireAdmin(w, r) {
			return true
		}
		type routeState struct {
			Name            string  `json:"name"`
			PathPrefix      string  `json:"path_prefix"`
			LoadBalancing   string  `json:"load_balancing"`
			InFlight        int     `json:"in_flight"`
			RequestCapacity int     `json:"request_capacity"`
			RetriesInFlight int     `json:"retries_in_flight"`
			RetryCapacity   int     `json:"retry_capacity"`
			RatePerSecond   float64 `json:"rate_per_second"`
			RateBurst       int     `json:"rate_burst"`
			TotalTimeoutMS  int64   `json:"total_timeout_ms"`
			ShadowInFlight  int     `json:"shadow_in_flight"`
			ShadowCapacity  int     `json:"shadow_capacity"`
		}
		set := g.routes.Load()
		states := make([]routeState, 0, len(set.Routes))
		for _, route := range set.Routes {
			state := routeState{
				Name: route.Config.Name, PathPrefix: route.Config.PathPrefix, LoadBalancing: route.Config.LoadBalancing,
				InFlight: len(route.requestSlots), RequestCapacity: cap(route.requestSlots),
				RetriesInFlight: len(route.retrySlots), RetryCapacity: cap(route.retrySlots),
				RatePerSecond: route.Config.RateLimit.RequestsPerSecond, RateBurst: route.Config.RateLimit.Burst,
				TotalTimeoutMS: route.Config.Resilience.TotalTimeout.Std().Milliseconds(),
			}
			if route.shadowSlots != nil {
				state.ShadowInFlight = len(route.shadowSlots)
				state.ShadowCapacity = cap(route.shadowSlots)
			}
			states = append(states, state)
		}
		g.respondJSON(w, http.StatusOK, states)
		return true
	case "/admin/backends/drain":
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST or DELETE required"})
			return true
		}
		if !g.requireAdmin(w, r) {
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
		g.events.Add(eventlog.Event{Type: "backend_state", Route: routeName, Backend: backendName, Details: map[string]any{"status": status, "peer": r.RemoteAddr}})
		g.respondJSON(w, http.StatusOK, map[string]any{"status": status})
		return true
	case "/admin/circuits/reset":
		if r.Method != http.MethodPost {
			g.respondJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return true
		}
		if !g.requireAdmin(w, r) {
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
		g.events.Add(eventlog.Event{Type: "circuit_reset", Route: routeName, Backend: backendName, Details: map[string]any{"count": reset, "peer": r.RemoteAddr}})
		g.respondJSON(w, http.StatusOK, map[string]any{"status": "reset", "count": reset})
		return true
	}
	return false
}
