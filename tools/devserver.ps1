<#
  (Re)starts the local development console with its embedded PostgreSQL.
    .\tools\devserver.ps1 [-Version 0.7.0]
  Console: http://127.0.0.1:8080 (admin/admin123), agents: https://<host>:8443
#>
param([string]$Version = "dev", [string]$PublicURL = "https://192.168.101.104:8443")
$ErrorActionPreference = "Stop"
$root = Resolve-Path (Join-Path $PSScriptRoot "..")
$data = Join-Path $root ".devdata"
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")

Get-Process backupzit-server -ErrorAction SilentlyContinue | Stop-Process -Force
# The embedded PostgreSQL survives a killed server; its path may be unreadable.
for ($i = 0; $i -lt 10; $i++) {
    $pg = Get-Process postgres -ErrorAction SilentlyContinue | Where-Object { -not $_.Path -or $_.Path -like "*backupzit*" }
    if (-not $pg) { break }
    $pg | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep 1
}
$pid_file = Join-Path $data "pgdata\postmaster.pid"
if (Test-Path $pid_file) { Remove-Item $pid_file -Force }

Push-Location $root
try {
    go build -ldflags "-X main.version=$Version" -o bin\ ./cmd/backupzit-server
    if ($LASTEXITCODE -ne 0) { throw "build failed" }
} finally { Pop-Location }

$env:BACKUPZIT_ADMIN_PASSWORD = "admin123"
Start-Process -FilePath (Join-Path $root "bin\backupzit-server.exe") -WorkingDirectory $root -WindowStyle Hidden `
    -RedirectStandardOutput (Join-Path $data "server.log") -RedirectStandardError (Join-Path $data "server.err") `
    -ArgumentList "--dev-embedded-db", "--data-dir", $data, "--listen", "0.0.0.0:8443", "--dev-http", "127.0.0.1:8080", "--public-url", $PublicURL
for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep 1
    try { if ((Invoke-WebRequest http://127.0.0.1:8080/login -UseBasicParsing -TimeoutSec 2).StatusCode -eq 200) { "console $Version is up"; exit 0 } } catch {}
}
Get-Content (Join-Path $data "server.err") -Tail 5
throw "console did not start"
