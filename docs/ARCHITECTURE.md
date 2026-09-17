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

Each route also has a concurrent retry budget, and each backend has an independent bulkhead. These bounds prevent a failed replica from consuming the capacity needed by healthy replicas. The defaults can be overridden per route:

```json
"resilience": {
  "circuit_failure_threshold": 20,
  "circuit_cooldown": "10s",
  "max_concurrent_per_backend": 16,
  "max_concurrent_retries": 8
}
```

Containment activations are exported as `gatego_retry_rejected_total` and `gatego_bulkhead_rejected_total`. Current circuit states are exported as one-hot gauges through `gatego_backend_circuit_state`.

### Distributed rate limiting
With no Redis configured, each GateGo instance uses an in-memory token bucket. When `GATEGO_REDIS_ADDR` is configured, the same token-bucket state is updated atomically by a Redis Lua script, allowing multiple gateway replicas to enforce a shared quota. Redis failures degrade to the local limiter rather than taking the gateway down.

### Trusted proxy boundary
GateGo strips caller-supplied identity headers. Forwarded IP headers are ignored by default so clients cannot bypass per-IP quotas by spoofing `X-Forwarded-For`. They can be trusted explicitly only when a known ingress sanitizes them.

### Trace context
GateGo accepts W3C trace context, creates server and upstream HTTP spans, and forwards trace context downstream. `X-Trace-ID` is returned to the caller for correlation. When `OTEL_EXPORTER_OTLP_ENDPOINT` is configured, spans are batch-exported over OTLP/HTTP; otherwise propagation remains active without an exporter.

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

### Circuit operations

`GET /admin/backends` reports active health, circuit state, accumulated failures, the configured failure threshold, recovery wait, and bulkhead occupancy. `POST /admin/circuits/reset?route=<name>&backend=<url-or-host>` clears circuit state after an operator has independently confirmed recovery. Omitting `backend` resets all circuits on the named route. Both endpoints require `X-Admin-Key`.

Manual reset bypasses the normal cooldown and should be used only for deliberate incident recovery. It does not mark an actively unhealthy backend healthy; active health state remains an independent admission check.

`POST /admin/backends/drain?route=<name>&backend=<url-or-host>` removes a backend from new load-balancer selections while allowing its in-flight requests to complete. `DELETE` against the same endpoint activates it again. Drain state is independent from health and circuit state, is included in `/admin/backends`, and is exported through `gatego_backend_draining`.

## Known limitations

These are deliberate portfolio boundaries rather than hidden production claims:

- Configuration is file/environment driven rather than stored in a replicated control-plane database.
- Request bodies are buffered up to the configured limit so they can be replayed safely on retry.
- WebSocket upgrades are proxied, but WebSocket connections are not retried after establishment.
- TLS termination and certificate lifecycle are delegated to Cloud Run/Kubernetes ingress in deployment examples.
- No service discovery protocol is implemented; backend endpoints come from config.
- The minimal Redis RESP client is purpose-built for rate limiting, not a general Redis library.
