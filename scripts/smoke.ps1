$ErrorActionPreference = "Stop"
Write-Host "Starting GateGo stack..."
docker compose up --build -d
for ($attempt = 0; $attempt -lt 60; $attempt++) {
  try {
    if ((Invoke-WebRequest -UseBasicParsing "http://localhost:8080/readyz" -TimeoutSec 1).StatusCode -eq 200) { break }
  } catch {}
  Start-Sleep -Seconds 1
}
if ($attempt -eq 60) { throw "GateGo did not become ready in time" }

$token = (go run ./cmd/token -sub demo-user -role developer).Trim()
Write-Host "`nPublic route"
Invoke-RestMethod -Uri "http://localhost:8080/public/hello" | ConvertTo-Json

Write-Host "`nProtected route"
$headers = @{ Authorization = "Bearer $token" }
Invoke-RestMethod -Uri "http://localhost:8080/api/hello" -Headers $headers | ConvertTo-Json

Write-Host "`nReadiness"
Invoke-RestMethod -Uri "http://localhost:8080/readyz" | ConvertTo-Json

Write-Host "`nRun a benchmark with:"
Write-Host "go run ./cmd/loadgen -url http://localhost:8080/public/echo -c 50 -d 10s"

Write-Host "`nFailure isolation (forcing backend B to return 503)"
$admin = @{ "X-Admin-Key" = "demo-admin-key" }
try {
  Invoke-WebRequest -UseBasicParsing -Method Post -Headers $admin "http://localhost:9002/admin/fault?status=503" | Out-Null
  1..120 | ForEach-Object { Invoke-WebRequest -UseBasicParsing "http://localhost:8080/public/failure-demo-$($_)" | Out-Null }
  Invoke-RestMethod -Headers @{ "X-Admin-Key" = "dev-admin-key" } "http://localhost:8080/admin/backends" | ConvertTo-Json -Depth 4
} finally {
  Invoke-WebRequest -UseBasicParsing -Method Delete -Headers $admin "http://localhost:9002/admin/fault" | Out-Null
}
Write-Host "`nDemo complete. Run 'make observe' to enable Grafana at http://localhost:3000"
