# GateGo

GateGo is a resilient Go API gateway that keeps traffic flowing when an upstream fails. It combines weighted load balancing, active health checks, bounded retries, circuit breakers, JWT and API-key authentication, shared Redis rate limiting, graceful draining, metrics, traces, and atomic configuration reloads.

```mermaid
flowchart LR
    C[Client] --> G1[GateGo :8080]
    C --> G2[GateGo :8081]
    G1 --> R[(Redis shared rate limits)]
    G2 --> R
    G1 --> A[Backend A weight 3]
    G1 --> B[Backend B weight 1]
    G1 --> D[Backend C weight 1]
    G2 --> A
    G2 --> B
    G2 --> D
    G1 --> O[OpenTelemetry Collector]
    G2 --> O
    P[Prometheus] --> G1
    P --> G2
    F[Grafana] --> P
```

## See it work

Prerequisites: Go, Docker with Compose, `curl`, and `make`.

```bash
make demo
```

The demo builds the complete two-gateway topology, waits for readiness, calls public and JWT-protected routes, prints a metrics sample, forces backend B to return `503`, and shows B's circuit opening while requests continue through A and C. The stack remains available for inspection; stop it with `make compose-down`.

Generate a repeatable failure matrix and Markdown evidence report with:

```bash
make chaos-report
```

This tests backend latency, persistent 503 responses, a stopped backend process, Redis interruption, and full recovery. See [the chaos testing guide](docs/CHAOS_TESTING.md) for report contents and parameters.

On Windows without `make`, run:

```powershell
./scripts/smoke.ps1
```

## Why these choices

| Choice | Why | Trade-off |
|---|---|---|
| Go and mostly its standard library | Small binaries, explicit concurrency, and a network stack suitable for a gateway hot path | More infrastructure code is implemented directly |
| Two gateway replicas with Redis | Demonstrates horizontal scaling while enforcing one global rate-limit bucket | Redis failure falls back to locally consistent limits |
| Weighted round robin | Predictable traffic allocation that is easy to inspect and test | It does not automatically account for backend latency |
| Optional least-connections routing | Sends work to the currently least-loaded eligible backend for uneven request durations | Backend weights apply only to weighted round robin |
| Optional consistent hashing | Preserves request affinity and cache locality using rendezvous hashing with minimal remapping | Clients must supply a stable configured key header |
| Health checks plus circuit breakers | Health checks detect readiness proactively; circuits react immediately to real request failures | Two independent state signals must be understood operationally |
| Retry budgets and backend bulkheads | Prevent one failed replica from amplifying load into healthy replicas | Overload is rejected deliberately instead of queued indefinitely |
| Atomic JSON reloads | Invalid configuration never replaces the last-known-good route set | This is not a distributed control plane |
| Prometheus and OpenTelemetry | Metrics explain aggregate behavior while traces explain individual requests | Full observability adds three demo containers |
| Structured access logs | Every request emits machine-readable status, latency, byte count, request ID, and trace ID for incident correlation | High-volume deployments should sample or aggregate logs |
| Multi-stage container builds | The compiler stays out of the runtime image, reducing size and attack surface | Builds have an additional stage |
| Header and body size limits | Reject oversized input before it consumes disproportionate gateway or backend memory | Limits must be raised deliberately for unusual APIs |

Retries are contained by per-route concurrency budgets and each backend has an independent bulkhead. Circuit thresholds, cooldowns, bulkhead capacity, and retry capacity are configurable per route through the `resilience` object.

Retry classification is configurable through `resilience.retry_statuses`; the default is `500`, `502`, `503`, and `504`. Only error status codes can be configured, and non-idempotent requests still require an `Idempotency-Key` regardless of status policy.

Routes can set `resilience.total_timeout` to cap the complete request across every attempt and jittered backoff. This prevents a per-attempt timeout from multiplying into an unexpectedly long client wait; exhausting the total budget returns `504` and remains distinct from client cancellation.

Routes may also define `access.allow_cidrs` and `access.deny_cidrs`. Deny rules take precedence, and forwarded client addresses are only considered when `GATEGO_TRUST_FORWARDED_HEADERS=true`; otherwise GateGo uses the direct peer address to prevent header spoofing.

Authentication can use JWT or API keys per route. API-key configuration names an environment variable rather than containing the secret itself; GateGo compares keys in constant time and removes the credential header before proxying. The existing `auth_required` setting remains a backward-compatible shorthand for JWT mode.

Browser-facing routes can define a `cors` policy with allowed origins, methods, headers, exposed headers, credential handling, and preflight cache duration. Unsafe wildcard-origin plus credential configurations are rejected before activation.

Per-route `headers` policies can set or remove request and response headers at the proxy boundary. GateGo rejects transformations of hop-by-hop headers and trusted identity headers so routing configuration cannot bypass authentication or transport rules.

Routes may set `strip_path_prefix` when the external API namespace should not be forwarded upstream. Query strings are preserved, and the same rewrite is applied to normal HTTP and WebSocket requests.

Candidate releases can be evaluated with bounded shadow traffic:

```json
"shadow": {
  "url": "http://candidate:9004",
  "sample_percent": 10,
  "timeout": "750ms",
  "max_concurrent": 4
}
```

Only retry-safe requests are copied. Shadow calls run asynchronously behind their own concurrency limit and cannot change the primary response, consume retry capacity, or affect backend circuits.

## Run locally

Start three demo backends, then GateGo with the local configuration:

```powershell
go run ./cmd/demo-backend -name backend-a -port 9001
go run ./cmd/demo-backend -name backend-b -port 9002
go run ./cmd/demo-backend -name backend-c -port 9003
go run ./cmd/gatego -config configs/gatego.local.json
```

Or run the horizontally scaled topology with Docker Compose:

```powershell
docker compose up -d --build
```

GateGo #1 listens on port 8080 and GateGo #2 on port 8081. Both use the same Redis instance, so `/global-limit` has one global token bucket rather than a separate allowance per gateway.

Rate-limited routes return `RateLimit-Limit`, `RateLimit-Remaining`, and—when exhausted—`RateLimit-Reset` plus a matching `Retry-After`. Local and Redis token buckets calculate these values from the same decision model. Prometheus exposes the active limiter backend and counts Redis-to-local fallback activations.

Prometheus and Grafana are available with the observability profile. The OpenTelemetry Collector receives OTLP/HTTP traces and prints a basic export summary:

```powershell
docker compose --profile observability up -d --build
docker compose logs -f otel-collector
```

Prometheus is exposed on port 9090 and Grafana on port 3000. Outside Compose, set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable trace export; W3C trace-context propagation remains active when export is disabled.

The containment dashboard includes live `gatego_inflight_requests` and `gatego_inflight_retries` gauges alongside retry, rejection, circuit, and load-shedding counters. This makes concurrency saturation visible during failure tests rather than only after requests are rejected.

Configuration operations expose `gatego_config_reloads_total{result="success|failure"}` and `gatego_config_info{revision="..."}`. These support alerts for rejected reloads and Prometheus queries that detect revision drift across replicas.

`GET /version` returns the running binary's version, commit, build timestamp, dirty-tree flag, and Go runtime version. Container builds accept `VERSION`, `COMMIT`, and `BUILD_DATE` build arguments, allowing deployments and incident reports to identify an exact artifact.

## Configuration and service discovery

Backends are addressed by DNS names in `configs/gatego.json`; Docker, Kubernetes, or another service registry supplies the current endpoint behind each name. GateGo also polls its mounted configuration file every `server.config_reload_interval` and atomically activates valid changes. A malformed update leaves the last known-good routes active.

Reloads can also be triggered with `SIGHUP` or:

```powershell
Invoke-WebRequest -Method Post -Headers @{"X-Admin-Key"="dev-admin-key"} http://localhost:8080/admin/reload
```

A candidate configuration can be validated without applying it:

```powershell
Invoke-WebRequest -Method Post -Headers @{"X-Admin-Key"="dev-admin-key"} -ContentType "application/json" -InFile configs/gatego.json http://localhost:8080/admin/config/validate
```

The effective configuration, including defaults applied by GateGo, is available to authenticated operators at `GET /admin/config`. Secrets are supplied through environment variables and are not part of this response. The response includes an `ETag` and `X-GateGo-Config-Revision`; the same revision appears in `/admin/readiness`, making configuration drift between replicas easy to detect.

Authenticated `GET /admin/backends` reports health and circuit state for every route/backend pair.

Operators can manually clear circuit state after an emergency repair. The route is required; `backend` may be either the backend URL or host and can be omitted to reset every backend on the route:

```powershell
Invoke-WebRequest -Method Post -Headers @{"X-Admin-Key"="dev-admin-key"} "http://localhost:8080/admin/circuits/reset?route=public-api&backend=backend-b:9002"
```

`GET /readyz` is circuit-aware: it returns unavailable when any configured route has no healthy, circuit-eligible backend.

Authenticated `GET /admin/readiness` adds per-route counts for total, healthy, eligible, open-circuit, and draining backends. This keeps the Kubernetes probe response small while giving operators a concrete reason for an unhealthy gateway.

Authenticated `GET /admin/events` returns a newest-first operational journal with monotonic sequence IDs. The fixed-size journal records configuration reloads, circuit transitions, maintenance changes, backend drain operations, circuit resets, and Redis rate-limit fallbacks; `type`, `route`, `limit`, and `after` query filters support incident investigation and cursor-based polling without unbounded memory growth.

Backends can be drained before maintenance without interrupting requests already in flight. `POST` begins draining and `DELETE` makes the backend eligible for new traffic again:

```powershell
Invoke-WebRequest -Method Post -Headers @{"X-Admin-Key"="dev-admin-key"} "http://localhost:8080/admin/backends/drain?route=public-api&backend=backend-b:9002"
Invoke-WebRequest -Method Delete -Headers @{"X-Admin-Key"="dev-admin-key"} "http://localhost:8080/admin/backends/drain?route=public-api&backend=backend-b:9002"
```

An entire gateway replica can be withdrawn for deployment with `POST /admin/maintenance`. While enabled, `/readyz` returns `503` and new data-plane requests receive `503` plus `Retry-After`, but `/healthz` and authenticated administration remain available. `GET` reports state and `DELETE` resumes traffic.

## Verification

```powershell
go test ./... -count=1
go test ./internal/... -count=1
go test ./integration -count=1
./scripts/resilience.ps1
./scripts/benchmark.ps1
```

Tests under `internal/` are focused unit and component tests. Tests under `integration/` start real HTTP listeners for GateGo and its backends and exercise administrative operations, routing, readiness, and metrics through the network boundary. The complete Go suite also includes deterministic retry, failure, slow-upstream, circuit-breaker, graceful-shutdown, distributed Redis limit, and config-reload coverage. The resilience script repeats failure scenarios against real containers; it requires Docker Desktop or another running Docker daemon.

The demo backend exposes an authenticated fault controller for testing:

```text
POST   /admin/fault?status=503&delay=2s&healthy=false
DELETE /admin/fault
X-Admin-Key: demo-admin-key
```

Measured throughput and latency results, the saturation point, topology, and methodology are in [BENCHMARKS.md](BENCHMARKS.md).
