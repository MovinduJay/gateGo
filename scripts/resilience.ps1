$ErrorActionPreference = "Stop"
$adminHeaders = @{ "X-Admin-Key" = "demo-admin-key" }
function Assert([bool]$Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
function Request([string]$Url) { try { Invoke-WebRequest -UseBasicParsing -Uri $Url -TimeoutSec 8 } catch { $_.Exception.Response } }

docker compose up -d --build backend-a backend-b backend-c redis gatego-1 gatego-2
if ($LASTEXITCODE -ne 0) { throw "docker compose up failed" }
try {
  for ($i=0; $i -lt 60; $i++) { if ((Request "http://localhost:8080/readyz").StatusCode -eq 200) { break }; Start-Sleep -Seconds 1 }

  Invoke-WebRequest -UseBasicParsing -Method Post -Headers $adminHeaders -Uri "http://localhost:9002/admin/fault?status=503" | Out-Null
  1..30 | ForEach-Object { Assert ((Request "http://localhost:8080/public/failure-$($_)").StatusCode -eq 200) "503 retry did not fail over" }
  $metrics = (Invoke-WebRequest -UseBasicParsing http://localhost:8080/metrics).Content
  Assert ($metrics -match 'gatego_retries_total\{route="public-api"\} [1-9]') "retry metric did not increase"
  $states = Invoke-RestMethod -Headers @{"X-Admin-Key"="dev-admin-key"} http://localhost:8080/admin/backends
  Assert (($states | Where-Object {$_.url -match 'backend-b' -and $_.circuit -eq 'open'}).Count -eq 1) "backend-b circuit did not open"
  Invoke-WebRequest -UseBasicParsing -Method Delete -Headers $adminHeaders -Uri http://localhost:9002/admin/fault | Out-Null

  Invoke-WebRequest -UseBasicParsing -Method Post -Headers $adminHeaders -Uri "http://localhost:9003/admin/fault?delay=3s" | Out-Null
  1..10 | ForEach-Object { Assert ((Request "http://localhost:8080/public/slow-$($_)").StatusCode -eq 200) "slow backend did not time out and fail over" }
  Invoke-WebRequest -UseBasicParsing -Method Delete -Headers $adminHeaders -Uri http://localhost:9003/admin/fault | Out-Null

  docker compose stop backend-a | Out-Host
  Start-Sleep -Seconds 8
  1..20 | ForEach-Object { $r=Request "http://localhost:8080/public/after-kill-$($_)"; Assert ($r.StatusCode -eq 200) "traffic did not move to healthy instances"; Assert ($r.Headers['X-GateGo-Backend'] -notmatch 'backend-a') "traffic reached stopped backend-a" }
  docker compose start backend-a | Out-Host
  Start-Sleep -Seconds 4

  $jobs = 1..20 | ForEach-Object { $port = if ($_ % 2) {8080} else {8081}; Start-Job -ScriptBlock { param($p) try {(Invoke-WebRequest -UseBasicParsing "http://localhost:$p/global-limit/test").StatusCode} catch {$_.Exception.Response.StatusCode.value__} } -ArgumentList $port }
  $codes = $jobs | Wait-Job | Receive-Job; $jobs | Remove-Job
  Assert (($codes | Where-Object {$_ -eq 200}).Count -eq 10) "shared Redis burst was not global: $($codes -join ',')"
  Assert (($codes | Where-Object {$_ -eq 429}).Count -eq 10) "expected ten globally rate-limited requests: $($codes -join ',')"

  Invoke-WebRequest -UseBasicParsing -Method Post -Headers $adminHeaders -Uri "http://localhost:9001/admin/fault?delay=1s" | Out-Null
  $request = Start-Job { (Invoke-WebRequest -UseBasicParsing http://localhost:8080/limited/drain).Content }
  Start-Sleep -Milliseconds 200
  docker compose stop -t 10 gatego-1 | Out-Host
  $body = $request | Wait-Job | Receive-Job; $request | Remove-Job
  Assert ($body -match 'backend-a') "in-flight request did not finish during shutdown"
  Write-Host "PASS: retries, circuit opening, healthy failover, graceful drain, and global Redis limiting"
} finally {
  docker compose down | Out-Host
}
