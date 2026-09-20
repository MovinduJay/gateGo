# Automated chaos testing

Run the complete failure matrix with one command:

```powershell
make chaos-report
```

On Windows without `make`:

```powershell
./scripts/chaos-report.ps1
```

The runner builds and starts two GateGo replicas, three demo backends, Redis, and the OpenTelemetry collector. It then measures a healthy baseline and injects these failures one at a time:

1. 1.5 seconds of latency on backend B.
2. Persistent HTTP 503 responses from backend B.
3. A complete backend B process stop.
4. A Redis outage, which exercises local rate-limit fallback.
5. Full restoration and recovery verification.

Every load phase uses the same concurrency and duration. The final Markdown table includes request count, success percentage, throughput, p99 latency, status distribution, retry delta, health, and circuit state. Raw JSON output is retained beside the report under `benchmark-results/chaos/`.

The script uses `finally` restoration: Redis and backend B are restarted, injected faults are cleared, and public-route circuits are reset even when a scenario fails. The Compose stack remains running afterward for inspection and can be removed with `make compose-down`.

Parameters can be overridden directly:

```powershell
./scripts/chaos-report.ps1 -Concurrency 50 -DurationSeconds 10 -OutputDirectory benchmark-results/chaos-50
```
