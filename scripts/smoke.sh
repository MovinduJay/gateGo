#!/usr/bin/env sh
set -eu

echo "Starting GateGo stack..."
docker compose up --build -d

printf 'Waiting for GateGo readiness'
attempt=0
until curl -fsS http://localhost:8080/readyz >/dev/null; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 60 ]; then
    printf '\nGateGo did not become ready in time\n' >&2
    docker compose ps
    exit 1
  fi
  printf '.'
  sleep 1
done
printf ' ready\n'

TOKEN=$(go run ./cmd/token -sub demo-user -role developer)
echo "Public route:"
curl -fsS http://localhost:8080/public/hello | sed 's/^/  /'
printf '\nProtected route:\n'
curl -fsS -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/hello | sed 's/^/  /'
printf '\nHealth:\n'
curl -fsS http://localhost:8080/readyz
printf '\nMetrics sample:\n'
curl -fsS http://localhost:8080/metrics | head -20

printf '\n\nFailure isolation (forcing backend B to return 503):\n'
curl -fsS -X POST -H 'X-Admin-Key: demo-admin-key' 'http://localhost:9002/admin/fault?status=503' >/dev/null
trap 'curl -fsS -X DELETE -H "X-Admin-Key: demo-admin-key" http://localhost:9002/admin/fault >/dev/null 2>&1 || true' EXIT
i=0
while [ "$i" -lt 120 ]; do
  curl -fsS "http://localhost:8080/public/failure-demo-$i" >/dev/null
  i=$((i + 1))
done
curl -fsS -H 'X-Admin-Key: dev-admin-key' http://localhost:8080/admin/backends
curl -fsS -X DELETE -H 'X-Admin-Key: demo-admin-key' http://localhost:9002/admin/fault >/dev/null
trap - EXIT
printf '\n\nDemo complete. Grafana: http://localhost:3000 (run make observe to enable it)\n'
