# Architecture

## Request path

```text
Client
  |
  v
+-------------------- GateGo --------------------+
| request ID / trace context                     |
| longest-prefix route match                     |
| optional JWT HS256 authentication              |
| local or Redis-distributed rate limit          |
| smooth weighted round-robin backend selection  |
| per-backend circuit breaker                    |
| timeout + correctness-aware retry              |
| Prometheus-compatible metrics                  |
+--------------------------+----------------------+
                           |
             +-------------+-------------+
             |             |             |
             v             v             v
         backend-a     backend-b     backend-c
```

## Why these mechanisms exist

### Smooth weighted round robin
A route can favor larger backends without permanently starving smaller ones. The balancer keeps a `currentWeight` per backend under a mutex and skips unhealthy/open-circuit targets.

### Health checks and circuit breakers are separate
Health checks are proactive and periodic. The circuit breaker reacts to real request failures immediately. A backend must be both healthy and circuit-eligible before it can receive traffic.

### Retry correctness
GateGo retries idempotent methods (`GET`, `HEAD`, `PUT`, `DELETE`, `OPTIONS`) automatically. `POST` and `PATCH` are retried only when the client supplies `Idempotency-Key`. This avoids turning a network retry into duplicate business side effects.

### Distributed rate limiting
With no Redis configured, each GateGo instance uses an in-memory token bucket. When `GATEGO_REDIS_ADDR` is configured, the same token-bucket state is updated atomically by a Redis Lua script, allowing multiple gateway replicas to enforce a shared quota. Redis failures degrade to the local limiter rather than taking the gateway down.

### Trusted proxy boundary
GateGo strips caller-supplied identity headers. Forwarded IP headers are ignored by default so clients cannot bypass per-IP quotas by spoofing `X-Forwarded-For`. They can be trusted explicitly only when a known ingress sanitizes them.

### Trace context
GateGo accepts or creates a W3C `traceparent` header and forwards it downstream. `X-Trace-ID` is returned to the caller for correlation. The project intentionally keeps tracing dependency-free; in a real deployment the next step would be exporting spans through an OpenTelemetry SDK/Collector.

## Control path

Configuration is loaded from `configs/gatego.json` or `GATEGO_CONFIG_JSON`. Routes are immutable after construction and swapped atomically during reload.

Reload options:

```bash
kill -HUP <pid>
```

or:

```bash
curl -X POST http://localhost:8080/admin/reload \
  -H 'X-Admin-Key: dev-admin-key'
```

## Known limitations

These are deliberate portfolio boundaries rather than hidden production claims:

- Configuration is file/environment driven rather than stored in a replicated control-plane database.
- Request bodies are buffered up to the configured limit so they can be replayed safely on retry.
- WebSocket upgrades are proxied, but WebSocket connections are not retried after establishment.
- TLS termination and certificate lifecycle are delegated to Cloud Run/Kubernetes ingress in deployment examples.
- No service discovery protocol is implemented; backend endpoints come from config.
- The minimal Redis RESP client is purpose-built for rate limiting, not a general Redis library.
