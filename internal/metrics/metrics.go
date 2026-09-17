package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type hist struct {
	Count   uint64
	Sum     float64
	Buckets [8]uint64
}
type Metrics struct {
	mu                                             sync.RWMutex
	requests, upstreamErrors, rateLimited, retries map[string]uint64
	retryRejected, bulkheadRejected                map[string]uint64
	healthy                                        map[string]float64
	durations                                      map[string]*hist
}

var bounds = []float64{.005, .01, .025, .05, .1, .25, .5, 1}

func New() *Metrics {
	return &Metrics{requests: map[string]uint64{}, upstreamErrors: map[string]uint64{}, rateLimited: map[string]uint64{}, retries: map[string]uint64{}, retryRejected: map[string]uint64{}, bulkheadRejected: map[string]uint64{}, healthy: map[string]float64{}, durations: map[string]*hist{}}
}
func key(parts ...string) string { return strings.Join(parts, "\x1f") }
func (m *Metrics) IncRequest(route, status string) {
	m.mu.Lock()
	m.requests[key(route, status)]++
	m.mu.Unlock()
}
func (m *Metrics) IncUpstreamError(route, backend string) {
	m.mu.Lock()
	m.upstreamErrors[key(route, backend)]++
	m.mu.Unlock()
}
func (m *Metrics) IncRateLimited(route string) { m.mu.Lock(); m.rateLimited[route]++; m.mu.Unlock() }
func (m *Metrics) IncRetry(route string)       { m.mu.Lock(); m.retries[route]++; m.mu.Unlock() }
func (m *Metrics) IncRetryRejected(route string) {
	m.mu.Lock()
	m.retryRejected[route]++
	m.mu.Unlock()
}
func (m *Metrics) IncBulkheadRejected(route string) {
	m.mu.Lock()
	m.bulkheadRejected[route]++
	m.mu.Unlock()
}
func (m *Metrics) SetHealthy(route, backend string, v float64) {
	m.mu.Lock()
	m.healthy[key(route, backend)] = v
	m.mu.Unlock()
}
func (m *Metrics) Observe(route, backend string, d time.Duration) {
	s := d.Seconds()
	m.mu.Lock()
	h := m.durations[key(route, backend)]
	if h == nil {
		h = &hist{}
		m.durations[key(route, backend)] = h
	}
	h.Count++
	h.Sum += s
	for i, b := range bounds {
		if s <= b {
			h.Buckets[i]++
		}
	}
	m.mu.Unlock()
}
func esc(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"").Replace(s)
}
func labels(k string, names ...string) string {
	p := strings.Split(k, "\x1f")
	parts := make([]string, 0, len(names))
	for i, n := range names {
		v := ""
		if i < len(p) {
			v = p[i]
		}
		parts = append(parts, fmt.Sprintf(`%s="%s"`, n, esc(v)))
	}
	return strings.Join(parts, ",")
}
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP gatego_requests_total Gateway requests by route and status.")
	fmt.Fprintln(w, "# TYPE gatego_requests_total counter")
	writeMap(w, "gatego_requests_total", m.requests, []string{"route", "status"})
	writeMap(w, "gatego_upstream_errors_total", m.upstreamErrors, []string{"route", "backend"})
	writeMap(w, "gatego_rate_limited_total", m.rateLimited, []string{"route"})
	writeMap(w, "gatego_retries_total", m.retries, []string{"route"})
	writeMap(w, "gatego_retry_rejected_total", m.retryRejected, []string{"route"})
	writeMap(w, "gatego_bulkhead_rejected_total", m.bulkheadRejected, []string{"route"})
	keys := sortedFloat(m.healthy)
	for _, k := range keys {
		fmt.Fprintf(w, "gatego_backend_healthy{%s} %g\n", labels(k, "route", "backend"), m.healthy[k])
	}
	for _, k := range sortedHist(m.durations) {
		h := m.durations[k]
		base := labels(k, "route", "backend")
		for i, b := range bounds {
			fmt.Fprintf(w, "gatego_request_duration_seconds_bucket{%s,le=\"%g\"} %d\n", base, b, h.Buckets[i])
		}
		fmt.Fprintf(w, "gatego_request_duration_seconds_bucket{%s,le=\"+Inf\"} %d\n", base, h.Count)
		fmt.Fprintf(w, "gatego_request_duration_seconds_sum{%s} %g\n", base, h.Sum)
		fmt.Fprintf(w, "gatego_request_duration_seconds_count{%s} %d\n", base, h.Count)
	}
}
func writeMap(w http.ResponseWriter, name string, m map[string]uint64, names []string) {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		fmt.Fprintf(w, "%s{%s} %d\n", name, labels(k, names...), m[k])
	}
}
func sortedFloat(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
func sortedHist(m map[string]*hist) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
