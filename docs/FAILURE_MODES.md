# Failure-mode matrix

| Failure | GateGo behavior | Mechanism |
|---|---|---|
| Backend returns 502/503/504 | Try another eligible backend when retry is safe | Retry + weighted balancer |
| Backend connection fails | Record failure and retry safe request | Circuit breaker + retry |
| Backend repeatedly fails | Temporarily remove it from request path | Circuit breaker |
| Health endpoint fails repeatedly | Mark backend unhealthy | Active health checker |
| Redis is unavailable | Fall back to local token bucket | Graceful degradation |
| Client exceeds quota | Return 429 with `Retry-After` | Token bucket |
| POST has no idempotency key | Do not retry | Side-effect safety |
| Request exceeds body limit | Return 413 | Bounded buffering |
| Gateway receives SIGTERM | Stop accepting new work and drain | `http.Server.Shutdown` |
| Bad config reload | Keep currently active route set | Atomic swap only after validation |

### Failure-amplification containment

Retries use jittered exponential backoff and an eight-request concurrent budget per route by default. Each backend has an independent 16-request bulkhead, so retry traffic aimed at one failing replica cannot consume all capacity reserved for healthy replicas. Passive circuits open after 20 failures; this still isolates a persistent failure quickly while avoiding circuit trips from small, concurrent bursts of tail-latency errors. After the ten-second cooldown, the circuit permits one half-open probe and closes only when that probe succeeds. All four values are configurable through each route's `resilience` object.

When Redis is unavailable, the gateway falls back locally and waits one second before trying Redis again. Warning logs use the same cooldown. This prevents an unavailable rate-limit dependency from adding a dial and synchronous log write to every request during an upstream incident.

## Failure demo

`backend-b` in Docker Compose has a 10% simulated failure rate. Generate traffic and inspect:

```bash
go run ./cmd/loadgen -url http://localhost:8080/public/echo -c 50 -d 20s
curl http://localhost:8080/metrics
```

Then stop a backend:

```bash
docker compose stop backend-a
```

After the health threshold, traffic should be served only by remaining healthy backends. Restart it and observe it become eligible again.
