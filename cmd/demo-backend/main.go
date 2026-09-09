package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

type faultState struct {
	status  atomic.Int64
	delayNS atomic.Int64
	healthy atomic.Bool
}

func main() {
	name := flag.String("name", env("BACKEND_NAME", "backend"), "backend name")
	port := flag.String("port", env("PORT", "9001"), "listen port")
	failRate := flag.Float64("fail-rate", envFloat("FAIL_RATE", 0), "probability [0,1] of returning 503")
	delay := flag.Duration("delay", envDuration("DELAY", 0), "artificial response delay")
	flag.Parse()
	faults := &faultState{}
	faults.delayNS.Store(int64(*delay))
	faults.healthy.Store(true)
	adminKey := env("DEMO_ADMIN_KEY", "demo-admin-key")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if !faults.healthy.Load() {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "backend": *name})
	})
	mux.HandleFunc("/admin/fault", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Admin-Key") != adminKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			faults.status.Store(0)
			faults.delayNS.Store(0)
			faults.healthy.Store(true)
		} else if r.Method == http.MethodPost {
			status, err := strconv.Atoi(r.URL.Query().Get("status"))
			if err != nil && r.URL.Query().Get("status") != "" {
				http.Error(w, "invalid status", http.StatusBadRequest)
				return
			}
			d, err := time.ParseDuration(valueOr(r.URL.Query().Get("delay"), "0s"))
			if err != nil {
				http.Error(w, "invalid delay", http.StatusBadRequest)
				return
			}
			faults.status.Store(int64(status))
			faults.delayNS.Store(int64(d))
			faults.healthy.Store(r.URL.Query().Get("healthy") != "false")
		} else {
			http.Error(w, "POST or DELETE required", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": faults.status.Load(), "delay": time.Duration(faults.delayNS.Load()).String(), "healthy": faults.healthy.Load()})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		currentDelay := time.Duration(faults.delayNS.Load())
		if currentDelay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(currentDelay):
			}
		}
		if status := int(faults.status.Load()); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "forced failure", "backend": *name})
			return
		}
		if *failRate > 0 && rand.Float64() < *failRate {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "simulated failure", "backend": *name})
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"backend":     *name,
			"method":      r.Method,
			"path":        r.URL.Path,
			"query":       r.URL.RawQuery,
			"subject":     r.Header.Get("X-Authenticated-Subject"),
			"request_id":  r.Header.Get("X-Request-ID"),
			"traceparent": r.Header.Get("traceparent"),
			"body":        string(body),
			"time":        time.Now().UTC().Format(time.RFC3339Nano),
		})
	})

	srv := &http.Server{Addr: ":" + *port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("demo backend %s listening on %s", *name, srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func envFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}
func envDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
