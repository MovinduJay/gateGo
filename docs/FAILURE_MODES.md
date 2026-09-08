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
