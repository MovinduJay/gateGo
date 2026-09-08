# Technical interview guide

## 1. Why build an API gateway instead of calling services directly?
Centralizes cross-cutting traffic concerns: authentication, quotas, routing, resilience, observability, rollout strategies, and policy enforcement. It also prevents every service from reimplementing the same edge logic.

## 2. Why Go?
The standard library has a strong HTTP/networking stack, goroutines make concurrent health checks and request handling straightforward, binaries are small and easy to containerize, and the language keeps the hot path easy to reason about.

## 3. Why not retry every failed request?
Retries can duplicate side effects. GateGo retries naturally idempotent methods and requires `Idempotency-Key` for POST/PATCH. A production system can also integrate a server-side idempotency store.

## 4. Health check vs circuit breaker?
A health check periodically samples backend readiness. A circuit breaker reacts to failures seen on the real traffic path. Using both gives proactive detection plus fast protection under live failures.

## 5. Why a mutex in the weighted balancer?
Smooth weighted round robin mutates shared current weights. Without synchronization, concurrent requests race and corrupt scheduling fairness. The critical section is tiny; contention can later be reduced with sharded balancers or different algorithms.

## 6. Why Redis for distributed rate limiting?
In-memory buckets are correct only per gateway process. With several replicas, Redis provides shared state and an atomic Lua script so the quota applies across the cluster.

## 7. What happens if Redis dies?
The current policy fails open to a local limiter. That preserves availability but weakens global quota accuracy. Security-sensitive routes may instead choose fail-closed behavior; this is a product/SLO trade-off.

## 8. What is the consistency guarantee of the rate limiter?
With Redis available, updates for one key are serialized by the Lua script. Without Redis, limits are only locally consistent per gateway replica.

## 9. How would you improve performance?
Profile first. Likely improvements include avoiding body buffering for non-retried streaming requests, reducing label-cardinality in metrics, reusing Redis connections more aggressively, adding connection-level telemetry, and considering power-of-two-choices load balancing under heterogeneous latency.

## 10. How would you make it production-grade?
Add mTLS/service identity, persistent dynamic configuration, service discovery, richer authorization policy, OpenTelemetry SDK export, distributed config rollout, admin audit logs, secret-manager integration, fuzzing, chaos tests, and SLO-driven autoscaling.

## Questions an interviewer may ask you to code

- Implement token bucket from scratch.
- Make weighted round robin thread-safe.
- Detect duplicate idempotency keys.
- Add consistent hashing for sticky routing.
- Add least-connections routing.
- Implement bounded worker concurrency for health checks.
- Prevent retry storms with retry budgets.
