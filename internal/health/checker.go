package health

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/example/gatego/internal/balancer"
	"github.com/example/gatego/internal/metrics"
)

type Target struct {
	Route   string
	Backend *balancer.Backend
}

type Provider func() []Target

type Checker struct {
	client           *http.Client
	provider         Provider
	interval         time.Duration
	failureThreshold int
	successThreshold int
	metrics          *metrics.Metrics
}

func New(provider Provider, interval, timeout time.Duration, failureThreshold, successThreshold int, m *metrics.Metrics) *Checker {
	return &Checker{
		client: &http.Client{Timeout: timeout}, provider: provider, interval: interval,
		failureThreshold: failureThreshold, successThreshold: successThreshold, metrics: m,
	}
}

func (c *Checker) Run(ctx context.Context) {
	c.check(ctx)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.check(ctx)
		}
	}
}

func (c *Checker) check(ctx context.Context) {
	for _, target := range c.provider() {
		go c.checkOne(ctx, target)
	}
}

func (c *Checker) checkOne(ctx context.Context, target Target) {
	b := target.Backend
	u := *b.URL
	hp, err := url.Parse(b.HealthPath)
	if err == nil {
		u = *b.URL.ResolveReference(hp)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	ok := false
	if err == nil {
		resp, reqErr := c.client.Do(req)
		if reqErr == nil {
			ok = resp.StatusCode >= 200 && resp.StatusCode < 400
			resp.Body.Close()
		}
	}
	b.MarkHealth(ok, c.failureThreshold, c.successThreshold)
	if c.metrics != nil {
		v := 0.0
		if b.Healthy.Load() {
			v = 1
		}
		c.metrics.SetHealthy(target.Route, b.URL.Host, v)
	}
}
