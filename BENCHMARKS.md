# GateGo benchmarks

## Test setup

These results were measured on 2026-09-09, not estimated. GateGo and three demo backends ran as native Go 1.27 Windows processes on the same host. The route used weighted backends A/B/C at 3:1:1, a 50,000 req/s limit with a 100,000-request burst, two retries, and a two-second upstream timeout.

- Host: Windows 11 10.0.26200
- CPU: Intel Core i5-1135G7, 4 cores / 8 logical processors
- Memory: 8 GB
- Request: `GET /public/echo`
- Duration: 15 seconds per level
- Client and servers: same machine, loopback networking

Run the same matrix with:

```powershell
go run ./cmd/gatego -config configs/gatego.local.json
./scripts/benchmark.ps1
```

Raw JSON is written to the ignored `benchmark-results/` directory.

## Results

| Concurrent clients | Requests | Successful | Errors | Requests/sec | p50 | p95 | p99 | Max |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 277,253 | 277,253 | 0 | 18,476.7 | 4.19 ms | 13.11 ms | 21.42 ms | 69.40 ms |
| 250 | 141,713 | 141,713 | 0 | 8,674.9 | 6.57 ms | 45.99 ms | 332.78 ms | 2,966.57 ms |
| 500 | 15,507 | 5,824 | 9,683 | 1,028.3 | 299.03 ms | 730.95 ms | 3,095.39 ms | 4,992.89 ms |

At 500 clients, the errors comprise 8,852 HTTP 502 responses and 831 client transport/time-out errors. The raw files were produced before the load generator added explicit `http_error_responses` and `transport_errors` fields; those counts are derived from the status totals and are now reported directly on future runs.

## Saturation point

Saturation begins between 100 and 250 concurrent clients on this host. At 250 clients throughput fell by 53%, p99 increased from 21 ms to 333 ms, and maximum latency approached three seconds even though requests still completed successfully. At 500 clients the system was decisively overloaded: throughput fell to roughly 1,028 req/s and 62.4% of requests failed.

This is an end-to-end single-host limit, not a portable GateGo capacity claim. The client, gateway, and all backends compete for the same four physical CPU cores and loopback/network resources. Run the script on production-like separate hosts before setting capacity or autoscaling thresholds.

## Resilience and horizontal-scaling verification

`go test ./... -count=1` deterministically verifies:

- retries and failover for forced HTTP 500 and 503 responses;
- failover from refused connections and upstreams slower than the route timeout;
- circuit opening after five failures and subsequent traffic moving to a healthy backend;
- graceful shutdown waiting for an in-flight slow request to finish;
- two independent GateGo instances consuming one shared Redis token bucket;
- automatic file-based config reload, with invalid updates retaining the last known-good routes.

`scripts/resilience.ps1` repeats the scenarios against the full Docker Compose topology, including stopping a real backend container and alternating clients between ports 8080 and 8081. It requires a running Docker daemon.

## Horizontally scaled topology

```text
                         Redis
                           |
                    shared rate limit
                           |
                  +--------+--------+
                  |                 |
              GateGo #1         GateGo #2
               :8080             :8081
                  |                 |
                  +------ clients --+
                           |
                  +--------+--------+
                  |        |        |
              backend A backend B backend C
```

Compose service names provide DNS service discovery inside the deployment network. GateGo also watches its configuration file and atomically replaces routes after a valid change; invalid files never replace the active configuration. This lets an external discovery controller render backend membership without restarting the gateways. Manual `SIGHUP` and authenticated `POST /admin/reload` remain available.
