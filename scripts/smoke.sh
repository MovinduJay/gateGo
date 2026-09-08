#!/usr/bin/env sh
set -eu

echo "Starting GateGo stack..."
docker compose up --build -d
sleep 4

TOKEN=$(go run ./cmd/token -sub demo-user -role developer)
echo "Public route:"
curl -fsS http://localhost:8080/public/hello | sed 's/^/  /'
printf '\nProtected route:\n'
curl -fsS -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/hello | sed 's/^/  /'
printf '\nHealth:\n'
curl -fsS http://localhost:8080/readyz
printf '\nMetrics sample:\n'
curl -fsS http://localhost:8080/metrics | head -20
