param(
  [string]$RedisAddress = $env:GATEGO_REDIS_ADDR,
  [string]$ResultDir = "benchmark-results/resilience-c30"
)

$ErrorActionPreference = "Stop"
$resultDir = $ResultDir
New-Item -ItemType Directory -Force -Path $resultDir | Out-Null
go build -o bin/gatego.exe ./cmd/gatego
go build -o bin/demo-backend.exe ./cmd/demo-backend
go build -o bin/loadgen.exe ./cmd/loadgen
$processes = @()
$savedPort = $env:PORT
$savedRedis = $env:GATEGO_REDIS_ADDR
try {
  foreach ($i in 1..3) {
    $name = "backend-$([char](96 + $i))"
    $processes += Start-Process -FilePath (Resolve-Path "bin/demo-backend.exe") -ArgumentList @("-name", $name, "-port", "900$i") -RedirectStandardOutput "$resultDir/backend-$i.out.log" -RedirectStandardError "$resultDir/backend-$i.err.log" -WindowStyle Hidden -PassThru
  }
  $env:GATEGO_REDIS_ADDR = $RedisAddress
  foreach ($port in 8080, 8081) {
    $env:PORT = "$port"
    $processes += Start-Process -FilePath (Resolve-Path "bin/gatego.exe") -ArgumentList @("-config", "configs/gatego.local.json") -RedirectStandardOutput "$resultDir/gateway-$port.out.log" -RedirectStandardError "$resultDir/gateway-$port.err.log" -WindowStyle Hidden -PassThru
  }
  foreach ($attempt in 1..30) {
    try { if ((Invoke-WebRequest -UseBasicParsing http://localhost:8080/readyz -TimeoutSec 1).StatusCode -eq 200) { break } } catch {}
    Start-Sleep -Milliseconds 500
  }
  & bin/loadgen.exe -url http://localhost:8080/public/echo -c 30 -d 3s -json -output "$resultDir/baseline-c30.json"
  Invoke-WebRequest -UseBasicParsing -Method Post -Headers @{"X-Admin-Key"="demo-admin-key"} -Uri "http://localhost:9002/admin/fault?status=503" | Out-Null
  & bin/loadgen.exe -url http://localhost:8080/public/failure-load -c 30 -d 5s -json -output "$resultDir/backend-b-503-c30.json"
  Invoke-RestMethod -Headers @{"X-Admin-Key"="dev-admin-key"} http://localhost:8080/admin/backends | ConvertTo-Json -Depth 4 | Set-Content "$resultDir/circuit-state.json"
  (Invoke-WebRequest -UseBasicParsing http://localhost:8080/metrics).Content | Set-Content "$resultDir/metrics.txt"
  Invoke-WebRequest -UseBasicParsing -Method Delete -Headers @{"X-Admin-Key"="demo-admin-key"} -Uri http://localhost:9002/admin/fault | Out-Null
  Start-Sleep -Seconds 11
  1..100 | ForEach-Object { Invoke-WebRequest -UseBasicParsing "http://localhost:8080/public/recovery-$($_)" | Out-Null }
  Invoke-RestMethod -Headers @{"X-Admin-Key"="dev-admin-key"} http://localhost:8080/admin/backends | ConvertTo-Json -Depth 4 | Set-Content "$resultDir/circuit-state-recovered.json"
} finally {
  foreach ($process in $processes) { if (-not $process.HasExited) { Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue } }
  $env:PORT = $savedPort
  $env:GATEGO_REDIS_ADDR = $savedRedis
}
