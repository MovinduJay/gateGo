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
	"time"
)

func main() {
	name := flag.String("name", env("BACKEND_NAME", "backend"), "backend name")
	port := flag.String("port", env("PORT", "9001"), "listen port")
	failRate := flag.Float64("fail-rate", envFloat("FAIL_RATE", 0), "probability [0,1] of returning 503")
	delay := flag.Duration("delay", envDuration("DELAY", 0), "artificial response delay")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "backend": *name})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if *delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(*delay):
			}
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
