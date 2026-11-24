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

  Language, keyboard and time zone are those of this PC, like the place
  the medium is mostly used: -Language (default: the display language of
  this PC, e.g. de-de; en-us when Windows PE has no language pack for it)
  adds the Windows PE language pack and sets the UI language, which the
  recovery program follows (14 languages, else English). -KeyboardLayout
  (default: the first keyboard of this PC, e.g. 0407:00000407 for German;
  with -Language, the keyboard of that language) sets the keyboard, so a
  server address, recovery code or passphrase types as on this PC.
  -TimeZone (default: the one of this PC, e.g. "W. Europe Standard Time";
  "" keeps Pacific time) is the time zone snapshot times are shown in.
  -Language "" keeps the language of the base image. Japanese, Chinese and
  Korean get their Windows PE font support. -Components adds further
  optional components; -PowerShell adds PowerShell with the storage and
  DISM cmdlets (larger ISO).

  The graphical recovery program (backupzit-recovery.exe and its
  .manifest, installed next to the agent) is copied to System32. At start
  the medium looks for \backupzit\recovery.json on every drive: with it,
  the recovery agent connects to the console as before (the restore is
  started there; nobody needs to type on site), otherwise the recovery
  program opens its restore wizard. The command prompt of the medium
  starts the other one ("backupzit-recovery" or "backupzit-agent
  recovery"; the program also has "Connect to the console"). -UiExe names
  another copy of the program, -NoUi leaves it out (console agent only).

  The license texts (LICENSE.txt, THIRD_PARTY_LICENSES.txt, next to the
  agent when it was installed with the MSI, or in the source tree) and a
  SOURCE-CODE.txt saying where the source code is are copied to
  \backupzit\ on the medium, as on the Linux recovery ISO.

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
    [switch]$NoUi,
    [string]$Language = (Get-UICulture).Name,
    [string]$KeyboardLayout = "",
    [string]$TimeZone = (Get-TimeZone).Id,
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

# The graphical program is installed next to the agent (MSI): use it unless
# -UiExe names another one or -NoUi asks for the console agent only.
if (-not $UiExe -and -not $NoUi -and (Test-Path $AgentExe)) {
    $next = Join-Path (Split-Path -Parent (Resolve-Path $AgentExe)) "backupzit-recovery.exe"
    if (Test-Path $next) { $UiExe = $next; Write-Host "graphical recovery program: $UiExe" }
}
if ($NoUi) { $UiExe = "" }

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
# Language and keyboard follow this PC unless they are given, like the time
# zone: the medium is mostly used where it is built, and a server address,
# recovery code or passphrase typed on a keyboard that Windows PE reads as
# another layout comes out wrong (a US keyboard read as German swaps y and
# z and moves ":" and "/"). A language named with -Language must exist;
# this PC's language falls back to en-us when Windows PE has none for it.
$languageGiven = $PSBoundParameters.ContainsKey("Language")
$Language = $Language.ToLower()
if ($Language -and -not (Test-Path (Join-Path $ocs $Language))) {
    if ($languageGiven) { throw "Windows PE language '$Language' not installed ($ocs\$Language)" }
    Write-Output "Windows PE has no language pack for this PC's language $Language; using en-us"
    $Language = "en-us"
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
if (-not $KeyboardLayout -and -not $languageGiven) {
    # This PC's first keyboard that is a plain layout ("0407:00000407"); an
    # input method such as the Japanese IME is left to the table above.
    try {
        $KeyboardLayout = @(Get-WinUserLanguageList | ForEach-Object { $_.InputMethodTips } |
                Where-Object { $_ -match '^[0-9A-Fa-f]{4}:[0-9A-Fa-f]{8}$' })[0]
    }
    catch { $KeyboardLayout = "" }
}
if (-not $KeyboardLayout -and $keyboards.ContainsKey($Language)) { $KeyboardLayout = $keyboards[$Language] }
if ($Language) { Write-Output "Windows PE language $Language, keyboard $(if ($KeyboardLayout) { $KeyboardLayout } else { 'of the language' })" }

# The license texts for \backupzit\ on the medium: next to the agent (the
# MSI installs LICENSE.txt and THIRD_PARTY_LICENSES.txt there), next to the
# recovery program, or at the root of the source tree. Looked up before the
# build so a missing file is reported at the start; a recovery medium is
# still built without them (it may be needed right now).
$licenseText = ""
$thirdPartyText = ""
$licenseDirs = @(Split-Path -Parent (Resolve-Path $AgentExe))
if ($UiExe) { $licenseDirs += Split-Path -Parent (Resolve-Path $UiExe) }
$licenseDirs += Join-Path $PSScriptRoot "..\.."
foreach ($d in $licenseDirs) {
    foreach ($n in @("LICENSE.txt", "LICENSE")) {
        $l = Join-Path $d $n
        $tp = Join-Path $d "THIRD_PARTY_LICENSES.txt"
        if (-not $licenseText -and (Test-Path $l) -and (Test-Path $tp)) { $licenseText = $l; $thirdPartyText = $tp }
    }
}
if (-not $licenseText) {
    Write-Warning ("LICENSE and THIRD_PARTY_LICENSES.txt were not found next to $AgentExe or in the source tree; " +
        "the medium gets only SOURCE-CODE.txt. Use the agent in its installation folder " +
        "(C:\Program Files\backupzit Agent\backupzit-agent.exe) to include them.")
}

# Optional components, in dependency order. WMI is needed by the storage
# and BitLocker components; SecureStartup brings manage-bde (unlocking
# BitLocker volumes).
$wanted = @("WinPE-WMI", "WinPE-Scripting", "WinPE-StorageWMI", "WinPE-SecureStartup", "WinPE-EnhancedStorage")
if ($PowerShell) { $wanted += @("WinPE-NetFx", "WinPE-PowerShell", "WinPE-DismCmdlets", "WinPE-SecureBootCmdlets") }
# Japanese, Chinese and Korean text needs the font support of Windows PE;
# without it the recovery program's texts show as boxes.
$fonts = @{ "ja-jp" = "WinPE-FontSupport-JA-JP"; "ko-kr" = "WinPE-FontSupport-KO-KR"; "zh-cn" = "WinPE-FontSupport-ZH-CN"
    "zh-tw" = "WinPE-FontSupport-ZH-TW"; "zh-hk" = "WinPE-FontSupport-ZH-HK" }
if ($fonts.ContainsKey($Language) -and $Components -notcontains $fonts[$Language]) { $wanted += $fonts[$Language] }
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
    if ($TimeZone) {
        # Windows PE runs in Pacific time otherwise: snapshot times would be
        # shown hours off, which makes choosing a backup by its time hard.
        Invoke-Dism @("/Image:$mount", "/Set-TimeZone:$TimeZone") "set time zone"
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
    # The recovery program takes its language from BACKUPZIT_LANG first, so
    # it follows -Language even where Windows PE reports another one.
    $uiLang = ""
    if ($Language) { $uiLang = "set BACKUPZIT_LANG=$Language" }
    if ($UiExe) {
        # recovery.json on the medium (put there by -RecoveryJson, or copied
        # onto the stick later) means: connect to the console, as without
        # the program, so someone on site only boots the stick and the admin
        # restores from the console. The drives are searched like the agent
        # does (C: to Z:, first match). The agent runs in a window of its
        # own, so this prompt stays free to start the program as well (e.g.
        # to load a network driver while the agent waits).
        $start = @"
set BZ_SETTINGS=
for %%d in (C D E F G H I J K L M N O P Q R S T U V W X Y Z) do if not defined BZ_SETTINGS if exist "%%d:\backupzit\recovery.json" set "BZ_SETTINGS=%%d:\backupzit\recovery.json"
if defined BZ_SETTINGS goto console
start "" backupzit-recovery.exe --open=restore
echo The BackupZit recovery program runs in its own window. Type "backupzit-recovery" to start it again,
echo "backupzit-agent recovery" for the console mode, or "wpeutil reboot" to restart the computer.
goto :eof
:console
echo Found %BZ_SETTINGS%: the recovery agent connects to the BackupZit console.
start "BackupZit recovery agent" cmd.exe /k backupzit-agent.exe recovery
echo The recovery agent runs in its own window; start the restore in the console.
echo Type "backupzit-recovery" for the recovery program (restore without the console, boot repair, drivers),
echo "backupzit-agent recovery" to start the agent again, or "wpeutil reboot" to restart the computer.
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
$uiLang
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
New-Item -ItemType Directory -Force $dst | Out-Null
if ($RecoveryJson) { Copy-Item $RecoveryJson (Join-Path $dst "recovery.json") }
if ($AccessPackage) { Copy-Item $AccessPackage (Join-Path $dst (Split-Path -Leaf $AccessPackage)) }
# The licenses go with the programs on the medium, as on the Linux recovery
# ISO. The release does not add them later: this ISO is built here, from
# the user's Windows PE, and is never part of a release.
if ($licenseText) {
    Copy-Item $licenseText (Join-Path $dst "LICENSE.txt")
    Copy-Item $thirdPartyText (Join-Path $dst "THIRD_PARTY_LICENSES.txt")
}
# Where the source code is. Written here rather than shipped as a file: the
# release publishes this script on its own. The version comes from the
# agent's file version (the MSI build sets it), e.g. 0.60.0.0 -> v0.60.0.
$agentVersion = (Get-Item $AgentExe).VersionInfo.ProductVersion
$tag = "v<version>"
if ($agentVersion -match '^(\d+\.\d+\.\d+)') { $tag = "v$($Matches[1])" }
$licenseRef = "LICENSE.txt"
$thirdPartyRef = "THIRD_PARTY_LICENSES.txt"
if (-not $licenseText) {
    $licenseRef = "LICENSE in the source code"
    $thirdPartyRef = "THIRD_PARTY_LICENSES.txt in the source code"
}
$sourceText = @"
BackupZit - source code of the software on this medium
======================================================

This Windows recovery medium was built with build-winpe.ps1 from
Windows PE and the BackupZit agent and recovery program.

BackupZit
---------
BackupZit is free software under the GNU Affero General Public License v3
($licenseRef). Its complete source code, for every release, is at

    https://github.com/Max-Zit/backupzit   (tag $tag)

The version is also shown in the title of the recovery program and by
"backupzit-agent version". The licenses of the libraries built into
BackupZit are listed in $thirdPartyRef.

Windows PE
----------
Windows PE is not part of BackupZit. It comes from the Windows Assessment
and Deployment Kit (ADK) of whoever built this medium and is licensed by
Microsoft under the ADK's license terms; drivers added to the medium are
licensed by their vendors.
"@
Set-Content -Path (Join-Path $dst "SOURCE-CODE.txt") -Value $sourceText -Encoding ASCII

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
