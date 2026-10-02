<#
  Clicks into a Hyper-V VM's console (test lab automation), using the same
  coordinates as a vmscreen.ps1 screenshot of the given size.

    .\tools\vmclick.ps1 -VMName bz-win11 -X 617 -Y 525 [-Right] [-ShotWidth 1024 -ShotHeight 768]
#>
param(
    [Parameter(Mandatory = $true)][string]$VMName,
    [Parameter(Mandatory = $true)][int]$X,
    [Parameter(Mandatory = $true)][int]$Y,
    [switch]$Right,
    [switch]$Double,
    [int]$ShotWidth = 1024,
    [int]$ShotHeight = 768
)
$ErrorActionPreference = "Stop"
$vm = Get-CimInstance -Namespace root\virtualization\v2 -ClassName Msvm_ComputerSystem -Filter "ElementName='$VMName'"
$ms = Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_SyntheticMouse
$vh = Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VideoHead | Select-Object -First 1
$w = [int]$vh.CurrentHorizontalResolution; $h = [int]$vh.CurrentVerticalResolution
if (-not $w) { $w = $ShotWidth; $h = $ShotHeight }
Invoke-CimMethod -InputObject $ms -MethodName SetAbsolutePosition -Arguments @{HorizontalPosition = [int]($X * $w / $ShotWidth); VerticalPosition = [int]($Y * $h / $ShotHeight) } | Out-Null
Start-Sleep -Milliseconds 300
$btn = if ($Right) { 2 } else { 1 }
Invoke-CimMethod -InputObject $ms -MethodName ClickButton -Arguments @{ButtonIndex = $btn } | Out-Null
if ($Double) { Start-Sleep -Milliseconds 120; Invoke-CimMethod -InputObject $ms -MethodName ClickButton -Arguments @{ButtonIndex = $btn } | Out-Null }
