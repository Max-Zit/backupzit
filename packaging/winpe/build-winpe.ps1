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

  -DriverDir takes several folders. -IncludeHostDrivers adds the storage,
  disk controller and network drivers of the machine the ISO is built on
  (e.g. Intel RST/VMD on a notebook), so this medium sees its disks.
  Vendor drivers are not shipped with BackupZit; the folders come from
  you (e.g. the VirtIO ISO for Proxmox, VMware PVSCSI/VMXNET3).

  -BaseWim builds on another Windows PE image instead of the ADK's, e.g. a
  winre.wim (Windows Recovery Environment) that brings Wi-Fi and the
  drivers of its machine. The BackupZit program replaces its recovery
  menu.

  -UsbDrive F: also writes the medium to the USB stick with that drive
  letter (MakeWinPEMedia, FAT32; everything on it is erased). It needs
  -Force.
#>
param(
    [Parameter(Mandatory = $true)][string]$AgentExe,
    [Parameter(Mandatory = $true)][string]$Out,
    [string]$RecoveryJson = "",
    [string]$AccessPackage = "",
    [string[]]$DriverDir = @(),
    [switch]$IncludeHostDrivers,
    [string]$BaseWim = "",
    [string]$UsbDrive = "",
    [switch]$Force,
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

foreach ($f in @($AgentExe, $RecoveryJson, $AccessPackage, $UiExe, $BaseWim) + $DriverDir) {
    if ($f -and -not (Test-Path $f)) { throw "not found: $f" }
}
if ($UsbDrive) {
    if ($UsbDrive -notmatch '^[D-Zd-z]:$') { throw "-UsbDrive takes a drive letter such as F:" }
    if (-not $Force) { throw "-UsbDrive erases $UsbDrive completely; add -Force to continue" }
    $vol = Get-Volume -DriveLetter $UsbDrive[0] -ErrorAction Stop
    $disk = Get-Partition -DriveLetter $UsbDrive[0] | Get-Disk
    if ($disk.BusType -ne "USB") { throw "$UsbDrive is not on a USB disk (bus $($disk.BusType)); refusing to erase it" }
    if ($disk.IsBoot -or $disk.IsSystem) { throw "$UsbDrive is on the system disk" }
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

# Native programs write progress to stderr; with "Stop" and a redirected
# error stream Windows PowerShell 5.1 would treat that as an error. They
# are judged by their exit code only.
function Invoke-Native([string]$exe, [string[]]$argv) {
    $eap = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try { & $exe @argv 2>&1 | ForEach-Object { "$_" } }
    finally { $ErrorActionPreference = $eap }
}

function Invoke-AdkCmd([string]$cmd) {
    Invoke-Native cmd.exe @("/c", "`"$setEnv`" >nul && $cmd")
    if ($LASTEXITCODE -ne 0) { throw "failed ($LASTEXITCODE): $cmd" }
}

function Invoke-Dism([string[]]$dismArgs, [string]$what) {
    Invoke-Native dism.exe $dismArgs
    if ($LASTEXITCODE -ne 0) { throw "$what failed ($LASTEXITCODE)" }
}

if (Test-Path $WorkDir) {
    # Leftover mount from an interrupted build.
    & dism.exe /Unmount-Image /MountDir:"$WorkDir\mount" /Discard 2>$null | Out-Null
    Remove-Item -Recurse -Force $WorkDir
}
Invoke-AdkCmd "copype amd64 `"$WorkDir`""
if ($BaseWim) {
    Write-Output "using $BaseWim as the base image"
    Copy-Item -Force $BaseWim "$WorkDir\media\sources\boot.wim"
    Set-ItemProperty "$WorkDir\media\sources\boot.wim" -Name IsReadOnly -Value $false
}

# Drivers of this machine: storage, disk controllers and network.
$drivers = @($DriverDir)
if ($IncludeHostDrivers) {
    $hostDrv = Join-Path $WorkDir "host-drivers"
    New-Item -ItemType Directory -Force $hostDrv | Out-Null
    $classes = @("SCSIAdapter", "HDC", "Net", "System")
    $n = 0
    foreach ($d in Get-WindowsDriver -Online | Where-Object { $classes -contains $_.ClassName }) {
        $src = Split-Path -Parent $d.OriginalFileName
        # "System" holds chipset drivers too; keep only storage-related ones.
        if ($d.ClassName -eq "System" -and $d.ProviderName -notmatch "Intel" -and $src -notmatch "(?i)vmd|rst|iastor") { continue }
        $dst = Join-Path $hostDrv ((Split-Path -Leaf $src))
        if (-not (Test-Path $dst)) { Copy-Item -Recurse $src $dst; $n++ }
    }
    Write-Output "exported $n driver packages of this machine"
    $drivers += $hostDrv
}

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
    # bootsect.exe writes the Windows MBR boot code ("boot-repair --mbr").
    # Current Windows PE images have it (write-protected); older ones get
    # the ADK's copy.
    $inImage = Join-Path $mount "Windows\System32\bootsect.exe"
    $bootsect = Join-Path $adk "Deployment Tools\amd64\BCDBoot\bootsect.exe"
    if (-not (Test-Path $inImage)) {
        if (Test-Path $bootsect) { Copy-Item $bootsect $inImage }
        else { Write-Warning "bootsect.exe is neither in Windows PE nor in the ADK; boot-repair --mbr will not work" }
    }
    if ($UiExe) {
        Copy-Item $UiExe (Join-Path $mount "Windows\System32\backupzit-recovery.exe")
        # The program needs common controls 6: its manifest lies next to it.
        $manifest = "$UiExe.manifest"
        if (-not (Test-Path $manifest)) { $manifest = Join-Path $PSScriptRoot "..\..\cmd\backupzit-recovery\backupzit-recovery.exe.manifest" }
        if (-not (Test-Path $manifest)) { throw "backupzit-recovery.exe.manifest not found (next to -UiExe or in cmd\backupzit-recovery)" }
        Copy-Item $manifest (Join-Path $mount "Windows\System32\backupzit-recovery.exe.manifest")
    }
    $i = 0
    foreach ($dir in $drivers) {
        # /ForceUnsigned is not used: unsigned drivers would not load anyway
        # with Secure Boot.
        Invoke-Dism @("/Image:$mount", "/Add-Driver", "/Driver:$dir", "/Recurse") "adding drivers from $dir to Windows PE"
        $drv = Join-Path $mount "BackupZit\drivers\$i"
        New-Item -ItemType Directory -Force $drv | Out-Null
        Copy-Item -Recurse -Force (Join-Path $dir "*") $drv
        $i++
    }
    if ($BaseWim) {
        # Windows RE starts its recovery menu through winpeshl.ini; start
        # the BackupZit environment instead.
        $ini = "[LaunchApps]`r`n%SYSTEMROOT%\System32\cmd.exe, /k %SYSTEMROOT%\System32\startnet.cmd`r`n"
        Set-Content -Path (Join-Path $mount "Windows\System32\winpeshl.ini") -Value $ini -Encoding ASCII
    }

    $kb = ""
    if ($KeyboardLayout) { $kb = "wpeutil SetKeyboardLayout $KeyboardLayout >nul 2>&1" }
    if ($UiExe) {
        $start = @"
start "" backupzit-recovery.exe --open=restore
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
Invoke-Native "$oscdimg\oscdimg.exe" @("-m", "-o", "-u2", "-udfver102", "-bootdata:$bootdata", "$WorkDir\media", "$Out") | Select-Object -Last 3
if ($LASTEXITCODE -ne 0) { throw "oscdimg failed ($LASTEXITCODE)" }
Write-Output "built $Out ($([math]::Round((Get-Item $Out).Length / 1MB)) MB)"

if ($UsbDrive) {
    Write-Output "writing the medium to $UsbDrive (erases it)"
    Invoke-AdkCmd "MakeWinPEMedia /UFD /F `"$WorkDir`" $UsbDrive"
    Write-Output "USB stick $UsbDrive ready (UEFI and BIOS)"
}
