package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	DurationMS         int64          `json:"duration_ms"`
	Concurrency        int            `json:"concurrency"`
	Requests           int64          `json:"requests"`
	OK                 int64          `json:"ok"`
	Errors             int64          `json:"errors"`
	HTTPErrorResponses int64          `json:"http_error_responses"`
	TransportErrors    int64          `json:"transport_errors"`
	RPS                float64        `json:"requests_per_second"`
	P50MS              float64        `json:"p50_ms"`
	P95MS              float64        `json:"p95_ms"`
	P99MS              float64        `json:"p99_ms"`
	MaxMS              float64        `json:"max_ms"`
	Statuses           map[string]int `json:"statuses"`
	Backends           map[string]int `json:"backends"`
}

func main() {
	url := flag.String("url", "http://localhost:8080/public/echo", "target URL")
	concurrency := flag.Int("c", 50, "concurrent workers")
	duration := flag.Duration("d", 10*time.Second, "test duration")
	token := flag.String("token", "", "optional bearer token")
	jsonOutput := flag.Bool("json", false, "emit machine-readable JSON")
	output := flag.String("output", "", "also write JSON result to this file")
	flag.Parse()
	if *concurrency < 1 || *duration <= 0 {
		fmt.Fprintln(os.Stderr, "concurrency and duration must be positive")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	transport := &http.Transport{MaxIdleConns: *concurrency * 2, MaxIdleConnsPerHost: *concurrency, MaxConnsPerHost: *concurrency, IdleConnTimeout: 30 * time.Second}
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	defer transport.CloseIdleConnections()
	latencies := make(chan time.Duration, 1_000_000)
	observations := make(chan [2]string, 1_000_000)
	var okCount, errorCount, httpErrorCount, transportErrorCount atomic.Int64
	var wg sync.WaitGroup
	started := time.Now()

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
				if err != nil {
					errorCount.Add(1)
					return
				}
				if *token != "" {
					req.Header.Set("Authorization", "Bearer "+*token)
				}
				requestStarted := time.Now()
				resp, err := client.Do(req)
				latency := time.Since(requestStarted)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					transportErrorCount.Add(1)
					errorCount.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					okCount.Add(1)
				} else {
					httpErrorCount.Add(1)
					errorCount.Add(1)
				}
				select {
				case latencies <- latency:
				default:
				}
				select {
				case observations <- [2]string{strconv.Itoa(resp.StatusCode), resp.Header.Get("X-GateGo-Backend")}:
				default:
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	close(latencies)
	close(observations)

	var samples []time.Duration
	for d := range latencies {
		samples = append(samples, d)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	statuses, backends := map[string]int{}, map[string]int{}
	for observation := range observations {
		statuses[observation[0]]++
		if observation[1] != "" {
			backends[observation[1]]++
		}
	}
	total := okCount.Load() + errorCount.Load()
	r := result{DurationMS: elapsed.Milliseconds(), Concurrency: *concurrency, Requests: total, OK: okCount.Load(), Errors: errorCount.Load(), HTTPErrorResponses: httpErrorCount.Load(), TransportErrors: transportErrorCount.Load(), Statuses: statuses, Backends: backends}
	if elapsed > 0 {
		r.RPS = float64(total) / elapsed.Seconds()
	}
	if len(samples) > 0 {
		r.P50MS = milliseconds(percentile(samples, .50))
		r.P95MS = milliseconds(percentile(samples, .95))
		r.P99MS = milliseconds(percentile(samples, .99))
		r.MaxMS = milliseconds(samples[len(samples)-1])
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		panic(err)
	}
	if *output != "" {
		if err := os.WriteFile(*output, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *jsonOutput {
		fmt.Println(string(b))
		return
	}
	fmt.Printf("duration=%s concurrency=%d requests=%d ok=%d errors=%d rps=%.1f\n", elapsed.Round(time.Millisecond), r.Concurrency, r.Requests, r.OK, r.Errors, r.RPS)
	fmt.Printf("p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms statuses=%v backends=%v\n", r.P50MS, r.P95MS, r.P99MS, r.MaxMS, r.Statuses, r.Backends)
}

func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	return xs[int(float64(len(xs)-1)*p)]
}
