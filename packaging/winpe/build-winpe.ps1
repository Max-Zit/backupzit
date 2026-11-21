<#
  Builds the BackupZit recovery ISO (Windows PE with the agent started in
  recovery mode). Run elevated on a Windows machine with the Windows ADK
  (Deployment Tools) and the Windows PE add-on installed.

    .\build-winpe.ps1 -AgentExe .\backupzit-agent.exe -Out .\backupzit-recovery.iso [-RecoveryJson .\recovery.json]

  With -RecoveryJson the file is placed in \backupzit\ on the ISO so the
  recovery environment connects to the server without typing.

  With -AccessPackage the access package (.bzaccess, see
  "backupzit-agent access-package create") is placed in \backupzit\ on the
  ISO, so the recovery environment opens the repositories in it with only
  its passphrase.

  With -DriverDir the drivers (INF folders) are added to Windows PE, so it
  sees disks and network cards that need them (e.g. VirtIO in Proxmox/KVM
  VMs), and are copied to X:\BackupZit\drivers, from where a restore "to
  different hardware" adds them to the restored Windows.

  -Language (default de-de) adds the Windows PE language pack and sets the
  UI language, -KeyboardLayout (default: the one of the language, e.g.
  0407:00000407 for German) the keyboard. -Components adds further optional
  components (e.g. WinPE-FontSupport-JA-JP); -PowerShell adds PowerShell
  with the storage and DISM cmdlets (larger ISO).

  With -UiExe the graphical recovery program is copied to System32 and
  started instead of the console recovery agent.
#>
param(
    [Parameter(Mandatory = $true)][string]$AgentExe,
    [Parameter(Mandatory = $true)][string]$Out,
    [string]$RecoveryJson = "",
    [string]$AccessPackage = "",
    [string]$DriverDir = "",
    [string]$UiExe = "",
    [string]$Language = "de-de",
    [string]$KeyboardLayout = "",
    [string[]]$Components = @(),
    [switch]$PowerShell,
    [int]$ScratchSpaceMB = 512,
    [string]$WorkDir = "$env:TEMP\backupzit-winpe"
)
$ErrorActionPreference = "Stop"

$adk = Join-Path ${env:ProgramFiles(x86)} "Windows Kits\10\Assessment and Deployment Kit"
$setEnv = Join-Path $adk "Deployment Tools\DandISetEnv.bat"
if (-not (Test-Path $setEnv)) { throw "Windows ADK Deployment Tools not found ($setEnv)" }
$peRoot = Join-Path $adk "Windows Preinstallation Environment"
if (-not (Test-Path $peRoot)) { throw "Windows PE add-on not installed" }
$ocs = Join-Path $peRoot "amd64\WinPE_OCs"

foreach ($f in @($AgentExe, $RecoveryJson, $AccessPackage, $UiExe, $DriverDir)) {
    if ($f -and -not (Test-Path $f)) { throw "not found: $f" }
}
$Language = $Language.ToLower()
if ($Language -and -not (Test-Path (Join-Path $ocs $Language))) {
    throw "Windows PE language '$Language' not installed ($ocs\$Language)"
}

# Keyboard layouts (input locale) of the common languages.
$keyboards = @{
    "de-de" = "0407:00000407"; "en-us" = "0409:00000409"; "en-gb" = "0809:00000809"; "fr-fr" = "040c:0000040c"
    "es-es" = "0c0a:0000040a"; "it-it" = "0410:00000410"; "nl-nl" = "0413:00020409"; "pl-pl" = "0415:00000415"
    "cs-cz" = "0405:00000405"; "pt-pt" = "0816:00000816"; "pt-br" = "0416:00000416"; "tr-tr" = "041f:0000041f"
    "ru-ru" = "0419:00000419"; "ja-jp" = "0411:{03B5835F-F03C-411B-9CE2-AA23E1171E36}{A76C93D9-5523-4E90-AAFA-4DB112F9AC76}"
    "zh-cn" = "0804:{81D4E9C9-1D3B-41BC-9E6C-4B40BF79E35E}{FA550B04-5AD7-411f-A5AC-CA038EC515D7}"; "de-ch" = "0807:00000807"
    "de-at" = "0c07:00000407"
}
if (-not $KeyboardLayout -and $keyboards.ContainsKey($Language)) { $KeyboardLayout = $keyboards[$Language] }

# Optional components, in dependency order. WMI is needed by the storage
# and BitLocker components; SecureStartup brings manage-bde (unlocking
# BitLocker volumes).
$wanted = @("WinPE-WMI", "WinPE-Scripting", "WinPE-StorageWMI", "WinPE-SecureStartup", "WinPE-EnhancedStorage")
if ($PowerShell) { $wanted += @("WinPE-NetFx", "WinPE-PowerShell", "WinPE-DismCmdlets", "WinPE-SecureBootCmdlets") }
$wanted += $Components

function Invoke-AdkCmd([string]$cmd) {
    & cmd.exe /c "`"$setEnv`" >nul && $cmd"
    if ($LASTEXITCODE -ne 0) { throw "failed ($LASTEXITCODE): $cmd" }
}

function Invoke-Dism([string[]]$dismArgs, [string]$what) {
    & dism.exe @dismArgs
    if ($LASTEXITCODE -ne 0) { throw "$what failed ($LASTEXITCODE)" }
}

if (Test-Path $WorkDir) {
    # Leftover mount from an interrupted build.
    & dism.exe /Unmount-Image /MountDir:"$WorkDir\mount" /Discard 2>$null | Out-Null
    Remove-Item -Recurse -Force $WorkDir
}
Invoke-AdkCmd "copype amd64 `"$WorkDir`""

$mount = Join-Path $WorkDir "mount"
Invoke-Dism @("/Mount-Image", "/ImageFile:$WorkDir\media\sources\boot.wim", "/Index:1", "/MountDir:$mount") "mount boot.wim"
try {
    # Language pack first, then every component with its language resources.
    if ($Language -and $Language -ne "en-us") {
        $lp = Join-Path $ocs "$Language\lp.cab"
        if (-not (Test-Path $lp)) { throw "language pack not found: $lp" }
        Write-Output "adding language pack $Language"
        Invoke-Dism @("/Image:$mount", "/Add-Package", "/PackagePath:$lp") "language pack $Language"
    }
    foreach ($c in $wanted) {
        $cab = Join-Path $ocs "$c.cab"
        if (-not (Test-Path $cab)) { throw "optional component not found: $cab" }
        Write-Output "adding $c"
        Invoke-Dism @("/Image:$mount", "/Add-Package", "/PackagePath:$cab") $c
        if ($Language) {
            $res = Join-Path $ocs "$Language\${c}_$Language.cab"
            if (Test-Path $res) {
                Invoke-Dism @("/Image:$mount", "/Add-Package", "/PackagePath:$res") "$c ($Language)"
            }
        }
    }
    if ($Language) {
        Invoke-Dism @("/Image:$mount", "/Set-AllIntl:$Language") "set language"
    }
    if ($KeyboardLayout) {
        Invoke-Dism @("/Image:$mount", "/Set-InputLocale:$KeyboardLayout") "set keyboard layout"
    }
    if ($ScratchSpaceMB -gt 0) {
        Invoke-Dism @("/Image:$mount", "/Set-ScratchSpace:$ScratchSpaceMB") "set scratch space"
    }

    Copy-Item $AgentExe (Join-Path $mount "Windows\System32\backupzit-agent.exe")
    if ($UiExe) {
        Copy-Item $UiExe (Join-Path $mount "Windows\System32\backupzit-recovery.exe")
    }
    if ($DriverDir) {
        Invoke-Dism @("/Image:$mount", "/Add-Driver", "/Driver:$DriverDir", "/Recurse") "adding drivers to Windows PE"
        $drv = Join-Path $mount "BackupZit\drivers"
        New-Item -ItemType Directory -Force $drv | Out-Null
        Copy-Item -Recurse -Force (Join-Path $DriverDir "*") $drv
    }

    $kb = ""
    if ($KeyboardLayout) { $kb = "wpeutil SetKeyboardLayout $KeyboardLayout >nul 2>&1" }
    if ($UiExe) {
        $start = @"
start "" backupzit-recovery.exe
echo The BackupZit recovery program runs in its own window. Type "backupzit-recovery" to start it again,
echo "backupzit-agent recovery" for the console mode, or "wpeutil reboot" to restart the computer.
"@
    }
    else {
        $start = @"
backupzit-agent.exe recovery
echo.
echo The recovery agent has stopped. Type "backupzit-agent recovery" to start it again,
echo or "wpeutil reboot" to restart the computer.
"@
    }
    $startnet = @"
@echo off
wpeinit
$kb
title BackupZit recovery
color 1F
echo Starting network...
wpeutil WaitForNetwork >nul 2>&1
$start
"@
    Set-Content -Path (Join-Path $mount "Windows\System32\startnet.cmd") -Value $startnet -Encoding ASCII
    Invoke-Dism @("/Unmount-Image", "/MountDir:$mount", "/Commit") "commit boot.wim"
}
catch {
    & dism.exe /Unmount-Image /MountDir:"$mount" /Discard | Out-Null
    throw
}

$dst = Join-Path $WorkDir "media\backupzit"
if ($RecoveryJson -or $AccessPackage) { New-Item -ItemType Directory -Force $dst | Out-Null }
if ($RecoveryJson) { Copy-Item $RecoveryJson (Join-Path $dst "recovery.json") }
if ($AccessPackage) { Copy-Item $AccessPackage (Join-Path $dst (Split-Path -Leaf $AccessPackage)) }

if (Test-Path $Out) { Remove-Item -Force $Out }
# Build the ISO directly with oscdimg so UEFI boots without the
# "Press any key to boot from CD or DVD" prompt (efisys_noprompt.bin);
# MakeWinPEMedia always uses the prompting loader.
$oscdimg = Join-Path $adk "Deployment Tools\amd64\Oscdimg"
$bootdata = "2#p0,e,b`"$oscdimg\etfsboot.com`"#pEF,e,b`"$oscdimg\efisys_noprompt.bin`""
& "$oscdimg\oscdimg.exe" -m -o -u2 -udfver102 "-bootdata:$bootdata" "$WorkDir\media" "$Out"
if ($LASTEXITCODE -ne 0) { throw "oscdimg failed ($LASTEXITCODE)" }
Write-Output "built $Out ($([math]::Round((Get-Item $Out).Length / 1MB)) MB)"
