package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/gatego/internal/config"
	"github.com/example/gatego/internal/metrics"
	"github.com/example/gatego/internal/ratelimit"
)

func testGateway(t *testing.T, backendJSON string, retries int, timeout string, redis *ratelimit.RedisLimiter, m *metrics.Metrics) *Gateway {
	t.Helper()
	raw := fmt.Sprintf(`{"routes":[{"name":"api","path_prefix":"/api","retries":%d,"timeout":"%s","rate_limit":{"requests_per_second":10000,"burst":10000},"backends":%s}]}`, retries, timeout, backendJSON)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", redis, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return gw
}

func TestRetryMatrixAndCircuitFailover(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var failures atomic.Int64
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { failures.Add(1); w.WriteHeader(status) }))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "healthy") }))
			defer good.Close()
			m := metrics.New()
			gw := testGateway(t, fmt.Sprintf(`[{"url":%q,"weight":1},{"url":%q,"weight":1}]`, bad.URL, good.URL), 1, "1s", nil, m)
			for i := 0; i < 45; i++ {
				rr := httptest.NewRecorder()
				gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("request %d: got %d", i, rr.Code)
				}
			}
			if failures.Load() != 20 {
				t.Fatalf("breaker should stop calls after threshold: calls=%d", failures.Load())
			}
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/backends", nil)
			req.Header.Set("X-Admin-Key", "admin")
			gw.ServeHTTP(rr, req)
			if !strings.Contains(rr.Body.String(), `"circuit":"open"`) {
				t.Fatalf("expected open circuit: %s", rr.Body.String())
			}
			metricsRR := httptest.NewRecorder()
			m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "http://gateway/metrics", nil))
			if !strings.Contains(metricsRR.Body.String(), `gatego_retries_total{route="api"} 20`) {
				t.Fatalf("retry metric missing: %s", metricsRR.Body.String())
			}
			if !strings.Contains(metricsRR.Body.String(), `gatego_backend_circuit_state{route="api",backend="`+strings.TrimPrefix(bad.URL, "http://")+`",state="open"} 1`) {
				t.Fatalf("open circuit metric missing: %s", metricsRR.Body.String())
			}
		})
	}
}

func TestConfiguredRetryStatusCanFailOver(t *testing.T) {
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer limited.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer healthy.Close()
	raw := fmt.Sprintf(`{"routes":[{"name":"api","path_prefix":"/api","retries":1,"resilience":{"retry_statuses":[429]},"backends":[{"url":%q},{"url":%q}]}]}`, limited.URL, healthy.URL)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	gw, err := New("", cfg, "secret", "admin", nil, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("custom retry status failed over with status %d", rr.Code)
	}
}

func TestConcurrentFailureIsContainedToBadBackend(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	goodA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer goodA.Close()
	goodC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer goodC.Close()

	gw := testGateway(t, fmt.Sprintf(`[{"url":%q,"weight":1},{"url":%q,"weight":3},{"url":%q,"weight":1}]`, bad.URL, goodA.URL, goodC.URL), 2, "1s", nil, metrics.New())
	var failures atomic.Int64
	var wg sync.WaitGroup
	for worker := 0; worker < 30; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for request := 0; request < 100; request++ {
				rr := httptest.NewRecorder()
				gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/load", nil))
				if rr.Code != http.StatusOK {
					failures.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() > 30 {
		t.Fatalf("failure escaped isolation budget: %d of 3000 requests failed", failures.Load())
	}
	backends := gw.routes.Load().Routes[0].Balancer.Backends()
	if backends[0].Breaker.State().String() != "open" {
		t.Fatalf("bad backend circuit = %s, want open", backends[0].Breaker.State())
	}
	for _, backend := range backends[1:] {
		if backend.Breaker.State().String() != "closed" {
			t.Fatalf("healthy backend %s circuit = %s, want closed", backend.URL, backend.Breaker.State())
		}
	}
}

func TestShadowTrafficIsAsynchronousAndBounded(t *testing.T) {
	shadowStarted := make(chan struct{}, 2)
	releaseShadow := make(chan struct{})
	shadow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-GateGo-Shadow") != "true" || r.URL.RequestURI() != "/test?candidate=true" {
			http.Error(w, "invalid shadow request", http.StatusBadRequest)
			return
		}
		shadowStarted <- struct{}{}
		<-releaseShadow
		w.WriteHeader(http.StatusOK)
	}))
	defer shadow.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer primary.Close()
	raw := fmt.Sprintf(`{"routes":[{"name":"api","path_prefix":"/api","strip_path_prefix":true,"shadow":{"url":%q,"sample_percent":100,"timeout":"2s","max_concurrent":1},"backends":[{"url":%q}]}]}`, shadow.URL, primary.URL)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	gw, err := New("", cfg, "secret", "admin", nil, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	first := httptest.NewRecorder()
	gw.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "http://gateway/api/test?candidate=true", nil))
	if first.Code != http.StatusOK || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("primary response was affected by shadow: status=%d duration=%s", first.Code, time.Since(started))
	}
	select {
	case <-shadowStarted:
	case <-time.After(time.Second):
		t.Fatal("shadow request was not sent")
	}
	second := httptest.NewRecorder()
	gw.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "http://gateway/api/test?candidate=true", nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second primary status = %d", second.Code)
	}
	close(releaseShadow)
	deadline := time.Now().Add(time.Second)
	for {
		metricsRR := httptest.NewRecorder()
		m.ServeHTTP(metricsRR, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body := metricsRR.Body.String()
		if strings.Contains(body, `gatego_shadow_requests_total{route="api",result="skipped"} 1`) && strings.Contains(body, `gatego_shadow_requests_total{route="api",result="success"} 1`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shadow metrics missing: %s", body)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeadAndSlowBackendsFailOver(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + listener.Addr().String()
	_ = listener.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer good.Close()
	for name, first := range map[string]string{"dead": deadURL, "slow": slow.URL} {
		t.Run(name, func(t *testing.T) {
			gw := testGateway(t, fmt.Sprintf(`[{"url":%q,"weight":1},{"url":%q,"weight":1}]`, first, good.URL), 1, "50ms", nil, metrics.New())
			rr := httptest.NewRecorder()
			gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/test", nil))
			if rr.Code != http.StatusOK || rr.Header().Get("X-GateGo-Backend") != strings.TrimPrefix(good.URL, "http://") {
				t.Fatalf("failover failed: status=%d backend=%s", rr.Code, rr.Header().Get("X-GateGo-Backend"))
			}
		})
	}
}

func TestExhaustedUpstreamTimeoutReturnsGatewayTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	gw := testGateway(t, fmt.Sprintf(`[{"url":%q,"weight":1}]`, slow.URL), 0, "20ms", nil, metrics.New())

	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/slow", nil))
	if rr.Code != http.StatusGatewayTimeout || !strings.Contains(rr.Body.String(), "upstream timeout") {
		t.Fatalf("timeout response = %d %s, want 504", rr.Code, rr.Body.String())
	}
}

func TestConnectionFailureReturnsBadGateway(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + listener.Addr().String()
	_ = listener.Close()
	gw := testGateway(t, fmt.Sprintf(`[{"url":%q,"weight":1}]`, deadURL), 0, "100ms", nil, metrics.New())

	rr := httptest.NewRecorder()
	gw.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "http://gateway/api/dead", nil))
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "upstream unavailable") {
		t.Fatalf("connection-failure response = %d %s, want 502", rr.Code, rr.Body.String())
	}
}

func TestGracefulShutdownDrainsInflightRequest(t *testing.T) {
	started := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, "finished")
	}))
	defer backend.Close()
	gw := testGateway(t, fmt.Sprintf(`[{"url":%q,"weight":1}]`, backend.URL), 0, "1s", nil, metrics.New())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: gw}
	go server.Serve(listener)
	response := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String() + "/api/slow")
		if err == nil {
			b, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				err = readErr
			} else if string(b) != "finished" {
				err = fmt.Errorf("body=%q", b)
			}
		}
		response <- err
	}()
	<-started
	shutdownStarted := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	if time.Since(shutdownStarted) < 100*time.Millisecond {
		t.Fatal("shutdown returned before the in-flight request completed")
	}
}

type fakeRedis struct {
	listener  net.Listener
	mu        sync.Mutex
	remaining map[string]int
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{listener: l, remaining: map[string]int{}}
	go f.serve()
	return f
}
func (f *fakeRedis) close() { _ = f.listener.Close() }
func (f *fakeRedis) serve() {
	for {
		c, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.connection(c)
	}
}
func (f *fakeRedis) connection(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if strings.EqualFold(args[0], "PING") {
			_, _ = io.WriteString(c, "+PONG\r\n")
			continue
		}
		if strings.EqualFold(args[0], "EVAL") {
			key := args[3]
			burst, _ := strconv.Atoi(args[5])
			f.mu.Lock()
			n, exists := f.remaining[key]
			if !exists {
				n = burst
			}
			allowed := n > 0
			if allowed {
				n--
			}
			f.remaining[key] = n
			f.mu.Unlock()
			if allowed {
				_, _ = io.WriteString(c, ":1\r\n")
			} else {
				_, _ = io.WriteString(c, ":0\r\n")
			}
			continue
		}
		_, _ = io.WriteString(c, "+OK\r\n")
	}
}
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, count)
	for i := range args {
		lengthLine, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lengthLine, "$")))
		if err != nil {
			return nil, err
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		args[i] = string(b[:n])
	}
	return args, nil
}

func TestDistributedRateLimitIsSharedAcrossGateways(t *testing.T) {
	fake := newFakeRedis(t)
	defer fake.close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer backend.Close()
	newGateway := func() (*Gateway, *ratelimit.RedisLimiter) {
		r := ratelimit.NewRedis(fake.listener.Addr().String(), "", 0)
		raw := fmt.Sprintf(`{"routes":[{"name":"limited","path_prefix":"/limited","rate_limit":{"requests_per_second":0.001,"burst":3},"backends":[{"url":%q}]}]}`, backend.URL)
		cfg, err := config.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		gw, err := New("", cfg, "secret", "admin", r, metrics.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		return gw, r
	}
	g1, r1 := newGateway()
	defer r1.Close()
	g2, r2 := newGateway()
	defer r2.Close()
	accepted, limited := 0, 0
	for i := 0; i < 6; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://gateway/limited/test", nil)
		if i%2 == 0 {
			g1.ServeHTTP(rr, req)
		} else {
			g2.ServeHTTP(rr, req)
		}
		if rr.Code == http.StatusOK {
			accepted++
		} else if rr.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if accepted != 3 || limited != 3 {
		t.Fatalf("expected one global burst of 3 across both gateways; accepted=%d limited=%d", accepted, limited)
	}
}
