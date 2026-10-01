<#
  Builds the BackupZit recovery ISO (Windows PE with the agent started in
  recovery mode). Run elevated on a Windows machine with the Windows ADK
  (Deployment Tools) and the Windows PE add-on installed.

    .\build-winpe.ps1 -AgentExe .\backupzit-agent.exe -Out .\backupzit-recovery.iso [-RecoveryJson .\recovery.json]

  With -RecoveryJson the file is placed in \backupzit\ on the ISO so the
  recovery environment connects to the server without typing.
#>
param(
    [Parameter(Mandatory = $true)][string]$AgentExe,
    [Parameter(Mandatory = $true)][string]$Out,
    [string]$RecoveryJson = "",
    [string]$WorkDir = "$env:TEMP\backupzit-winpe"
)
$ErrorActionPreference = "Stop"

$adk = Join-Path ${env:ProgramFiles(x86)} "Windows Kits\10\Assessment and Deployment Kit"
$setEnv = Join-Path $adk "Deployment Tools\DandISetEnv.bat"
if (-not (Test-Path $setEnv)) { throw "Windows ADK Deployment Tools not found ($setEnv)" }
if (-not (Test-Path (Join-Path $adk "Windows Preinstallation Environment"))) { throw "Windows PE add-on not installed" }

function Invoke-AdkCmd([string]$cmd) {
    & cmd.exe /c "`"$setEnv`" >nul && $cmd"
    if ($LASTEXITCODE -ne 0) { throw "failed ($LASTEXITCODE): $cmd" }
}

if (Test-Path $WorkDir) {
    # Leftover mount from an interrupted build.
    & dism.exe /Unmount-Image /MountDir:"$WorkDir\mount" /Discard 2>$null | Out-Null
    Remove-Item -Recurse -Force $WorkDir
}
Invoke-AdkCmd "copype amd64 `"$WorkDir`""

$mount = Join-Path $WorkDir "mount"
& dism.exe /Mount-Image /ImageFile:"$WorkDir\media\sources\boot.wim" /Index:1 /MountDir:"$mount"
if ($LASTEXITCODE -ne 0) { throw "mount boot.wim failed" }
try {
    Copy-Item $AgentExe (Join-Path $mount "Windows\System32\backupzit-agent.exe")
    $startnet = @"
@echo off
wpeinit
title BackupZit recovery
color 1F
echo Starting network...
wpeutil WaitForNetwork >nul 2>&1
backupzit-agent.exe recovery
echo.
echo The recovery agent has stopped. Type "backupzit-agent recovery" to start it again,
echo or "wpeutil reboot" to restart the computer.
"@
    Set-Content -Path (Join-Path $mount "Windows\System32\startnet.cmd") -Value $startnet -Encoding ASCII
    & dism.exe /Unmount-Image /MountDir:"$mount" /Commit
    if ($LASTEXITCODE -ne 0) { throw "commit boot.wim failed" }
}
catch {
    & dism.exe /Unmount-Image /MountDir:"$mount" /Discard | Out-Null
    throw
}

if ($RecoveryJson) {
    $dst = Join-Path $WorkDir "media\backupzit"
    New-Item -ItemType Directory -Force $dst | Out-Null
    Copy-Item $RecoveryJson (Join-Path $dst "recovery.json")
}
if (Test-Path $Out) { Remove-Item -Force $Out }
# Build the ISO directly with oscdimg so UEFI boots without the
# "Press any key to boot from CD or DVD" prompt (efisys_noprompt.bin);
# MakeWinPEMedia always uses the prompting loader.
$oscdimg = Join-Path $adk "Deployment Tools\amd64\Oscdimg"
$bootdata = "2#p0,e,b`"$oscdimg\etfsboot.com`"#pEF,e,b`"$oscdimg\efisys_noprompt.bin`""
& "$oscdimg\oscdimg.exe" -m -o -u2 -udfver102 "-bootdata:$bootdata" "$WorkDir\media" "$Out"
if ($LASTEXITCODE -ne 0) { throw "oscdimg failed ($LASTEXITCODE)" }
Write-Output "built $Out ($([math]::Round((Get-Item $Out).Length / 1MB)) MB)"
