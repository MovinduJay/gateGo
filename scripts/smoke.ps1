$ErrorActionPreference = "Stop"
Write-Host "Starting GateGo stack..."
docker compose up --build -d
Start-Sleep -Seconds 4

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
