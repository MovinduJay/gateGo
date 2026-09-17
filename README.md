# GateGo

GateGo is a cloud-native Go API gateway with weighted load balancing, active health checks, retries, circuit breaking, JWT authentication, local or Redis-backed token-bucket rate limiting, metrics, graceful shutdown, and atomic configuration reload.

Retries are contained by per-route concurrency budgets and each backend has an independent bulkhead. Circuit thresholds, cooldowns, bulkhead capacity, and retry capacity are configurable per route through the `resilience` object.

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

Prometheus and Grafana are available with the observability profile. The OpenTelemetry Collector receives OTLP/HTTP traces and prints a basic export summary:

```powershell
docker compose --profile observability up -d --build
docker compose logs -f otel-collector
```

Prometheus is exposed on port 9090 and Grafana on port 3000. Outside Compose, set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable trace export; W3C trace-context propagation remains active when export is disabled.

## Configuration and service discovery

Backends are addressed by DNS names in `configs/gatego.json`; Docker, Kubernetes, or another service registry supplies the current endpoint behind each name. GateGo also polls its mounted configuration file every `server.config_reload_interval` and atomically activates valid changes. A malformed update leaves the last known-good routes active.

Reloads can also be triggered with `SIGHUP` or:

```powershell
Invoke-WebRequest -Method Post -Headers @{"X-Admin-Key"="dev-admin-key"} http://localhost:8080/admin/reload
```

Authenticated `GET /admin/backends` reports health and circuit state for every route/backend pair.

## Verification

```powershell
go test ./... -count=1
./scripts/resilience.ps1
./scripts/benchmark.ps1
```

The Go suite includes deterministic retry, failure, slow-upstream, circuit-breaker, graceful-shutdown, distributed Redis limit, and config-reload tests. The resilience script repeats them against real containers; it requires Docker Desktop or another running Docker daemon.

The demo backend exposes an authenticated fault controller for testing:

```text
POST   /admin/fault?status=503&delay=2s&healthy=false
DELETE /admin/fault
X-Admin-Key: demo-admin-key
```

Measured throughput and latency results, the saturation point, topology, and methodology are in [BENCHMARKS.md](BENCHMARKS.md).
