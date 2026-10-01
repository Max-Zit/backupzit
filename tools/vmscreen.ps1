<#
  Saves a screenshot of a Hyper-V VM's console as PNG (lab tool).
    .\vmscreen.ps1 -VMName bz-bare -Out C:\temp\screen.png
#>
param([Parameter(Mandatory = $true)][string]$VMName, [Parameter(Mandatory = $true)][string]$Out,
      [int]$Width = 800, [int]$Height = 600)
$ErrorActionPreference = "Stop"
Add-Type -AssemblyName System.Drawing
$ns = "root\virtualization\v2"
$vm = Get-CimInstance -Namespace $ns -ClassName Msvm_ComputerSystem -Filter "ElementName='$VMName'"
$vssd = Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VirtualSystemSettingData | Where-Object VirtualSystemType -eq "Microsoft:Hyper-V:System:Realized"
$svc = Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemManagementService
$r = Invoke-CimMethod -InputObject $svc -MethodName GetVirtualSystemThumbnailImage -Arguments @{ TargetSystem = $vssd; WidthPixels = [uint16]$Width; HeightPixels = [uint16]$Height }
if ($r.ReturnValue -ne 0) { throw "GetVirtualSystemThumbnailImage returned $($r.ReturnValue)" }
$data = $r.ImageData
$bmp = New-Object System.Drawing.Bitmap($Width, $Height, [System.Drawing.Imaging.PixelFormat]::Format16bppRgb565)
$rect = New-Object System.Drawing.Rectangle(0, 0, $Width, $Height)
$bd = $bmp.LockBits($rect, [System.Drawing.Imaging.ImageLockMode]::WriteOnly, $bmp.PixelFormat)
$bytes = [byte[]]$data; $n = [Math]::Min($bytes.Length, $bd.Stride * $Height)
[System.Runtime.InteropServices.Marshal]::Copy($bytes, 0, $bd.Scan0, $n)
$bmp.UnlockBits($bd)
$bmp.Save($Out, [System.Drawing.Imaging.ImageFormat]::Png)
"saved $Out"
