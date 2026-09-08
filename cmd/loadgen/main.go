package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://localhost:8080/api/echo", "target URL")
	concurrency := flag.Int("c", 50, "concurrent workers")
	duration := flag.Duration("d", 10*time.Second, "test duration")
	token := flag.String("token", "", "optional bearer token")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *concurrency}}
	latencies := make(chan time.Duration, 1_000_000)
	var okCount atomic.Int64
	var errorCount atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
				if *token != "" {
					req.Header.Set("Authorization", "Bearer "+*token)
				}
				start := time.Now()
				resp, err := client.Do(req)
				lat := time.Since(start)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					errorCount.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 400 {
					okCount.Add(1)
				} else {
					errorCount.Add(1)
				}
				select {
				case latencies <- lat:
				default:
				}
			}
		}()
	}
	wg.Wait()
	close(latencies)

	var samples []time.Duration
	for d := range latencies {
		samples = append(samples, d)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	total := okCount.Load() + errorCount.Load()
	fmt.Printf("duration=%s concurrency=%d requests=%d ok=%d errors=%d rps=%.1f\n", *duration, *concurrency, total, okCount.Load(), errorCount.Load(), float64(total)/duration.Seconds())
	if len(samples) > 0 {
		fmt.Printf("p50=%s p95=%s p99=%s max=%s samples=%d\n", percentile(samples, .50), percentile(samples, .95), percentile(samples, .99), samples[len(samples)-1], len(samples))
	}
}

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	i := int(float64(len(xs)-1) * p)
	return xs[i]
}
