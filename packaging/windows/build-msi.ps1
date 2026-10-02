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
    [Parameter(Mandatory = $true)][string]$Version,
    # Build for Windows 7 / Server 2008 R2 / 2012 R2 with the go-legacy-win7
    # toolchain (Go dropped those systems after 1.20). Path from
    # BACKUPZIT_LEGACY_GO or tools-legacy next to the repository.
    [switch]$Legacy
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
    $go = "go"
    $suffix = ""
    if ($Legacy) {
        $go = $env:BACKUPZIT_LEGACY_GO
        if (-not $go) { $go = [IO.Path]::Combine($root, "..", "tools-legacy", "go-legacy-win7", "bin", "go.exe") }
        if (-not (Test-Path $go)) { throw "legacy Go toolchain not found: $go (https://github.com/thongtech/go-legacy-win7)" }
        $env:GOTOOLCHAIN = "local"
        $suffix = "-legacy"
    }
    # Icon, version and publisher in the exe (Windows resources).
    $icon = Join-Path $PSScriptRoot "backupzit.ico"
    $cmdDir = [IO.Path]::Combine($root, 'cmd', 'backupzit-agent'); Push-Location $cmdDir
    try {
        go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --manifest cli --icon $icon --product-name "BackupZit Agent" --file-description "BackupZit Agent" --copyright "MaxZit" --original-filename backupzit-agent.exe --product-version "$msiVersion.0" --file-version "$msiVersion.0"
        if ($LASTEXITCODE -ne 0) { throw "go-winres failed" }
    } finally { Pop-Location }
    try {
    & $go build -trimpath -ldflags "-s -w -X main.version=$Version$suffix" -o $exe ./cmd/backupzit-agent
    } finally { Remove-Item ([IO.Path]::Combine($cmdDir, 'rsrc_windows_amd64.syso')) -ErrorAction SilentlyContinue }
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }

    # Tray app (notification area), a GUI program.
    $tray = Join-Path $work "backupzit-tray.exe"
    $trayDir = [IO.Path]::Combine($root, 'cmd', 'backupzit-tray'); Push-Location $trayDir
    try {
        go run github.com/tc-hib/go-winres@v0.3.3 simply --arch amd64 --manifest gui --icon $icon --product-name "BackupZit Agent" --file-description "BackupZit Agent tray" --copyright "MaxZit" --original-filename backupzit-tray.exe --product-version "$msiVersion.0" --file-version "$msiVersion.0"
        if ($LASTEXITCODE -ne 0) { throw "go-winres failed" }
    } finally { Pop-Location }
    try {
    & $go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version$suffix" -o $tray ./cmd/backupzit-tray
    } finally { Remove-Item ([IO.Path]::Combine($trayDir, 'rsrc_windows_amd64.syso')) -ErrorAction SilentlyContinue }
    if ($LASTEXITCODE -ne 0) { throw "go build (tray) failed" }

    $wix = Join-Path $env:USERPROFILE ".dotnet\tools\wix.exe"
    if (-not (Test-Path $wix)) { $wix = "wix" }
    $msi = Join-Path $dist "backupzit-agent-$Version-x64$suffix.msi"
    & $wix build -ext WixToolset.Util.wixext -ext WixToolset.UI.wixext -arch x64 -d "Version=$msiVersion" -d "AgentExe=$exe" -d "TrayExe=$tray" -d "IconFile=$icon" -d "ArtDir=$PSScriptRoot" -o $msi (Join-Path $PSScriptRoot "agent.wxs")
    if ($LASTEXITCODE -ne 0) { throw "wix build failed" }
    Remove-Item (Join-Path $dist "*.wixpdb") -ErrorAction SilentlyContinue
    Write-Host "built $msi"
}
finally {
    Pop-Location
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED, Env:GOTOOLCHAIN -ErrorAction SilentlyContinue
}
