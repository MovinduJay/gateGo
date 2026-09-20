param(
  [int]$Concurrency = 30,
  [int]$DurationSeconds = 5,
  [string]$OutputDirectory = "benchmark-results/chaos"
)

$ErrorActionPreference = "Stop"
$adminHeaders = @{ "X-Admin-Key" = "dev-admin-key" }
$faultHeaders = @{ "X-Admin-Key" = "demo-admin-key" }
$rows = [System.Collections.Generic.List[object]]::new()

function Invoke-Compose([string[]]$Arguments) {
  & docker compose @Arguments | Out-Host
  if ($LASTEXITCODE -ne 0) { throw "docker compose $($Arguments -join ' ') failed" }
}

function Wait-Http([string]$Url, [int]$ExpectedStatus = 200, [int]$Seconds = 60) {
  $deadline = (Get-Date).AddSeconds($Seconds)
  do {
    try {
      $response = Invoke-WebRequest -UseBasicParsing -Uri $Url -TimeoutSec 2
      if ($response.StatusCode -eq $ExpectedStatus) { return }
    } catch {
      if ($_.Exception.Response -and [int]$_.Exception.Response.StatusCode -eq $ExpectedStatus) { return }
    }
    Start-Sleep -Milliseconds 500
  } while ((Get-Date) -lt $deadline)
  throw "$Url did not return HTTP $ExpectedStatus within $Seconds seconds"
}

function Wait-BackendBHealthy([int]$Seconds = 60) {
  $deadline = (Get-Date).AddSeconds($Seconds)
  do {
    try {
      $states = Invoke-RestMethod -Headers $adminHeaders http://localhost:8080/admin/backends
      $backend = $states | Where-Object { $_.route -eq "public-api" -and $_.url -match "backend-b" }
      if ($backend -and $backend.healthy -and -not $backend.draining) { return }
    } catch {}
    Start-Sleep -Milliseconds 500
  } while ((Get-Date) -lt $deadline)
  throw "backend B did not become healthy within $Seconds seconds"
}

function Clear-BackendFault {
  try { Invoke-WebRequest -UseBasicParsing -Method Delete -Headers $faultHeaders http://localhost:9002/admin/fault -TimeoutSec 3 | Out-Null } catch {}
}

function Reset-PublicCircuits {
  try { Invoke-WebRequest -UseBasicParsing -Method Post -Headers $adminHeaders "http://localhost:8080/admin/circuits/reset?route=public-api" -TimeoutSec 3 | Out-Null } catch {}
}

function Get-RetryCount {
  $metrics = (Invoke-WebRequest -UseBasicParsing http://localhost:8080/metrics -TimeoutSec 3).Content
  $match = [regex]::Match($metrics, 'gatego_retries_total\{route="public-api"\}\s+(\d+)')
  if ($match.Success) { return [int64]$match.Groups[1].Value }
  return 0
}

function Get-PublicState {
  $states = Invoke-RestMethod -Headers $adminHeaders http://localhost:8080/admin/backends
  $public = $states | Where-Object { $_.route -eq "public-api" }
  $parts = foreach ($state in $public) {
    $name = if ($state.url -match 'backend-([abc])') { $Matches[1].ToUpperInvariant() } else { $state.url }
    "$name=$($state.circuit)/$(if ($state.healthy) {'healthy'} else {'unhealthy'})"
  }
  return $parts -join ", "
}

function Run-Scenario([string]$Name, [string]$Slug) {
  $beforeRetries = Get-RetryCount
  $jsonPath = Join-Path $OutputDirectory "$Slug.json"
  & .\bin\loadgen.exe -url "http://localhost:8080/public/chaos-$Slug" -c $Concurrency -d "$($DurationSeconds)s" -json -output $jsonPath | Out-Null
  if ($LASTEXITCODE -ne 0) { throw "load generator failed for $Name" }
  $result = Get-Content $jsonPath -Raw | ConvertFrom-Json
  $afterRetries = Get-RetryCount
  $successRate = if ($result.requests -gt 0) { 100.0 * $result.ok / $result.requests } else { 0 }
  $statusText = ($result.statuses.PSObject.Properties | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join ", "
  $rows.Add([pscustomobject]@{
    Scenario = $Name
    Requests = $result.requests
    SuccessRate = $successRate
    RPS = $result.requests_per_second
    P99 = $result.p99_ms
    Statuses = $statusText
    Retries = $afterRetries - $beforeRetries
    Backends = Get-PublicState
  })
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw "Docker CLI is required" }
$dockerReady = $true
try {
  docker info *> $null
  if ($LASTEXITCODE -ne 0) { $dockerReady = $false }
} catch {
  $dockerReady = $false
}
if (-not $dockerReady) { throw "Docker daemon is not available; start Docker Desktop and rerun the command" }

New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
go build -o bin/loadgen.exe ./cmd/loadgen
if ($LASTEXITCODE -ne 0) { throw "load generator build failed" }

$startedAt = Get-Date
$commit = (git rev-parse --short HEAD).Trim()
$runError = $null

try {
  Invoke-Compose @("up", "-d", "--build", "backend-a", "backend-b", "backend-c", "redis", "otel-collector", "gatego-1", "gatego-2")
  Wait-Http http://localhost:8080/readyz
  Wait-Http http://localhost:8081/readyz

  Clear-BackendFault
  Reset-PublicCircuits
  Run-Scenario "Healthy baseline" "01-baseline"

  Invoke-WebRequest -UseBasicParsing -Method Post -Headers $faultHeaders "http://localhost:9002/admin/fault?delay=1500ms" | Out-Null
  Reset-PublicCircuits
  Run-Scenario "Backend B adds 1.5s latency" "02-backend-b-latency"
  Clear-BackendFault

  Invoke-WebRequest -UseBasicParsing -Method Post -Headers $faultHeaders "http://localhost:9002/admin/fault?status=503" | Out-Null
  Reset-PublicCircuits
  Run-Scenario "Backend B returns persistent 503" "03-backend-b-503"
  Clear-BackendFault

  Reset-PublicCircuits
  Invoke-Compose @("stop", "backend-b")
  Start-Sleep -Seconds 7
  Run-Scenario "Backend B process stopped" "04-backend-b-stopped"
  Invoke-Compose @("start", "backend-b")
  Wait-BackendBHealthy

  Reset-PublicCircuits
  Invoke-Compose @("stop", "redis")
  Start-Sleep -Seconds 1
  Run-Scenario "Redis unavailable; local fallback active" "05-redis-stopped"
  Invoke-Compose @("start", "redis")
  Start-Sleep -Seconds 2

  Clear-BackendFault
  Reset-PublicCircuits
  Wait-BackendBHealthy
  Run-Scenario "Everything restored" "06-recovered"
} catch {
  $runError = $_
} finally {
  try { Invoke-Compose @("start", "redis", "backend-b") } catch {}
  Clear-BackendFault
  Reset-PublicCircuits
}

$reportPath = Join-Path $OutputDirectory "chaos-report.md"
$lines = [System.Collections.Generic.List[string]]::new()
$lines.Add("# GateGo chaos report")
$lines.Add("")
$lines.Add("- Generated: $($startedAt.ToUniversalTime().ToString('yyyy-MM-dd HH:mm:ss')) UTC")
$lines.Add("- Commit: ``$commit``")
$lines.Add("- Load: concurrency $Concurrency for $DurationSeconds seconds per scenario")
$lines.Add("")
$lines.Add("| Scenario | Requests | Success | RPS | p99 ms | Statuses | Retries | Backend state |")
$lines.Add("|---|---:|---:|---:|---:|---|---:|---|")
foreach ($row in $rows) {
  $lines.Add("| $($row.Scenario) | $($row.Requests) | $($row.SuccessRate.ToString('F2'))% | $($row.RPS.ToString('F1')) | $($row.P99.ToString('F2')) | $($row.Statuses) | $($row.Retries) | $($row.Backends) |")
}
$lines.Add("")
$lines.Add("Raw load-generator results are stored beside this report.")
if ($runError) {
  $lines.Add("")
  $lines.Add("## Run failure")
  $lines.Add("")
  $lines.Add("The environment was restored after the runner failed: $($runError.Exception.Message)")
}
$resolvedOutputDirectory = (Resolve-Path $OutputDirectory).Path
[System.IO.File]::WriteAllLines((Join-Path $resolvedOutputDirectory "chaos-report.md"), $lines)

Write-Host "`nChaos report written to $reportPath"
$lines | ForEach-Object { Write-Host $_ }
if ($runError) { throw $runError }
