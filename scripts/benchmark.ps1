param(
  [string]$Target = "http://localhost:8080/public/echo",
  [int]$DurationSeconds = 15,
  [int[]]$Concurrency = @(100, 250, 500),
  [string]$OutputDirectory = "benchmark-results"
)
$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$results = @()
foreach ($clients in $Concurrency) {
  $path = Join-Path $OutputDirectory "c$clients.json"
  go run ./cmd/loadgen -url $Target -c $clients -d "$($DurationSeconds)s" -json -output $path | Out-Host
  if ($LASTEXITCODE -ne 0) { throw "load generator failed at concurrency $clients" }
  $results += Get-Content $path -Raw | ConvertFrom-Json
}
$saturation = "not reached"
for ($i = 1; $i -lt $results.Count; $i++) {
  $growth = ($results[$i].requests_per_second - $results[$i-1].requests_per_second) / [Math]::Max(1, $results[$i-1].requests_per_second)
  $errorRate = $results[$i].errors / [Math]::Max(1, $results[$i].requests)
  if ($growth -lt 0.10 -or $errorRate -gt 0.01) { $saturation = "$($results[$i].concurrency) concurrent clients"; break }
}
Write-Host "Saturation point: $saturation"
