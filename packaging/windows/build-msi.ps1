<#
  Builds the Windows agent and its MSI.

    .\packaging\windows\build-msi.ps1 -Version 0.2.0

  Output: dist\backupzit-agent-<version>-x64.msi
  Requires: Go, .NET SDK and WiX 5 (MS-RL licensed; newer WiX versions carry a
  maintenance-fee EULA):
    dotnet tool install --global wix --version 5.0.2
    wix extension add -g WixToolset.Util.wixext/5.0.2
#>
param(
    [Parameter(Mandatory = $true)][string]$Version
)
$ErrorActionPreference = "Stop"
$root = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$dist = Join-Path $root "dist"
$work = Join-Path $root "bin\msi"
New-Item -ItemType Directory -Force $dist, $work | Out-Null

# MSI versions must be numeric major.minor.build.
$msiVersion = ($Version -replace '[^0-9.].*$', '')
if ($msiVersion -notmatch '^\d+\.\d+\.\d+$') { throw "Version must start with major.minor.patch, got '$Version'" }

Push-Location $root
try {
    $env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
    $exe = Join-Path $work "backupzit-agent.exe"
    go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $exe ./cmd/backupzit-agent
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }

    $wix = Join-Path $env:USERPROFILE ".dotnet\tools\wix.exe"
    if (-not (Test-Path $wix)) { $wix = "wix" }
    $msi = Join-Path $dist "backupzit-agent-$Version-x64.msi"
    & $wix build -ext WixToolset.Util.wixext -arch x64 -d "Version=$msiVersion" -d "AgentExe=$exe" -o $msi (Join-Path $PSScriptRoot "agent.wxs")
    if ($LASTEXITCODE -ne 0) { throw "wix build failed" }
    Remove-Item (Join-Path $dist "*.wixpdb") -ErrorAction SilentlyContinue
    Write-Host "built $msi"
}
finally {
    Pop-Location
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
}
