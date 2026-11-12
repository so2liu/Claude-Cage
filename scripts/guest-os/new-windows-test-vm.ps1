#requires -Version 5.1
<#
.SYNOPSIS
Create an offline Windows 11 x64 test VM on a local Hyper-V host.
.DESCRIPTION
Supply a Windows ISO that you are licensed to use and a new directory under
an existing local folder. -ExpectedIsoSha256 can check the digest published
by your media provider. The new VM has no network adapter. Its configuration,
disk and guest TPM state stay together after shutdown. Use the returned VMId
as -ExpectedVMId for claude-desktop-windows.ps1. -Start boots only this VM.
Windows Setup installs PowerShell 7 for all guest users. Supply -PowerShellZip
to use an existing copy of the pinned Microsoft archive on an offline host.
Manual ISO setup prompts for the guest's initial local administrator. Supply
-GuestAdministrator to provide it directly. Windows Setup uses the protected
answer DVD to create that account without a network connection.
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string]$Name,
    [Parameter(Mandatory)][string]$IsoPath,
    [Parameter(Mandatory)][string]$InstanceDirectory,
    [ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$ExpectedIsoSha256,
    [string]$PowerShellZip,
    [string]$ImageEdition,
    [string]$ImageLanguage,
    [string]$ImageBuild,
    [pscredential]$GuestAdministrator,
    [ValidateRange(2, 2048)][int]$CpuCount = 2,
    [ValidateRange(4, 65536)][int]$MemoryGiB = 4,
    [ValidateRange(64, 65536)][int]$DiskGiB = 64,
    [switch]$Start
)
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'windows-guest-media.ps1')
. (Join-Path $PSScriptRoot 'windows-host-image.ps1')
$fromHost = ![string]::IsNullOrEmpty($ImageEdition)

function Set-TestVMResources($VM, [int]$Processors, [long]$MemoryBytes) {
    # New-VM can inherit Dynamic Memory with a 1 TiB maximum on a client host.
    Set-VM -VM $VM -ProcessorCount $Processors -StaticMemory -MemoryStartupBytes $MemoryBytes -AutomaticStartAction Nothing -AutomaticCheckpointsEnabled $false
    $memory = Get-VMMemory -VM $VM
    if ($memory.DynamicMemoryEnabled -or $memory.Startup -ne $MemoryBytes) {
        throw 'The VM memory allocation does not match the requested fixed budget.'
    }
}

if ([Environment]::OSVersion.Platform -ne 'Win32NT' -or ![Environment]::Is64BitProcess) {
    throw 'Run this script in 64-bit Windows PowerShell on the Hyper-V host.'
}
if ($Name.Trim() -ne $Name -or $Name -match '[\x00-\x1f*?\[\]\\/:|"<>]') { throw 'Invalid VM name.' }
$system = Get-CimInstance Win32_ComputerSystem
$os = Get-CimInstance Win32_OperatingSystem
$nativeCpu = @(Get-CimInstance Win32_Processor | Where-Object Architecture -ne 9)
if ($nativeCpu.Count -gt 0 -or $os.ProductType -ne 1 -or [int]$os.BuildNumber -lt 22000) {
    throw 'This prototype requires a native x64 Windows 11 client host.'
}
if (!$system.HypervisorPresent -or (Get-Service vmms).Status -ne 'Running' -or !(Get-VMHost)) {
    throw 'Hyper-V must be running and accessible to the current user.'
}
if ($CpuCount -gt $system.NumberOfLogicalProcessors -or ($MemoryGiB * 1GB) -gt ($os.FreePhysicalMemory * 1KB)) {
    throw 'The requested CPU count or startup memory exceeds the available host resources.'
}
if (@(Get-VM | Where-Object Name -eq $Name).Count) { throw "A VM named '$Name' already exists." }

$iso = Get-Item -LiteralPath $IsoPath
if ($iso -isnot [IO.FileInfo] -or ($iso.Extension -ne '.iso' -and !($fromHost -and $iso.Extension -eq '.esd')) -or !$iso.Length -or
    $iso.FullName.StartsWith('\\') -or [IO.DriveInfo]::new($iso.Directory.Root.FullName).DriveType -eq 'Network') {
    throw 'Supply a local Windows 11 x64 ISO.'
}
if ($ExpectedIsoSha256 -and (Get-FileHash -LiteralPath $iso.FullName -Algorithm SHA256).Hash -ne $ExpectedIsoSha256) {
    throw 'The installation ISO does not match the expected SHA-256.'
}
$directory = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($InstanceDirectory)
$parent = Get-Item -LiteralPath ([IO.Path]::GetDirectoryName($directory))
if ($parent -isnot [IO.DirectoryInfo] -or $parent.FullName.StartsWith('\\') -or
    [IO.DriveInfo]::new($parent.Root.FullName).DriveType -ne 'Fixed') {
    throw 'Choose a new VM directory on a local fixed disk.'
}
if (Test-Path -LiteralPath $directory) { throw "The instance directory already exists: $directory" }
$requiredFree = if ($fromHost) { 32GB } else { 64GB }
if ([IO.DriveInfo]::new($parent.Root.FullName).AvailableFreeSpace -lt $requiredFree) {
    throw "Windows creation needs $($requiredFree / 1GB) GiB free on the destination volume. Its virtual disk grows with use."
}
if (!$PSCmdlet.ShouldProcess($directory, "Create offline Hyper-V VM '$Name' (start: $([bool]$Start))")) { return }

if ($fromHost) {
    $answer = New-CageCloneAnswer $GuestAdministrator $ImageLanguage
} else {
    if (!$GuestAdministrator) {
        $GuestAdministrator = Get-Credential -UserName 'CageAdmin' -Message '设置虚拟 Windows 的初始本地管理员和密码。安装后用此账户登录客机，并在 Cage 继续安装时再次输入。'
    }
    $answer = New-CageISOAnswer $GuestAdministrator
}

# Create with a protected ACL, then claim the requested name using a same-volume
# rename that fails atomically if another creation has already won.
$acl = [Security.AccessControl.DirectorySecurity]::new()
$acl.SetAccessRuleProtection($true, $false)
$owner = [Security.Principal.WindowsIdentity]::GetCurrent().User
$acl.SetOwner($owner)
foreach ($sid in @($owner, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'),
                            [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))) {
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new(
        $sid, 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow'))
}
$staging = Join-Path $parent.FullName ([Guid]::NewGuid().ToString('N'))
$staged = $false
$claimed = $false
$vm = $null
$diskPath = Join-Path $directory 'system.vhdx'
$toolsISO = Join-Path $directory 'windows-guest-tools.iso'
$ownsToolsISO = $false
try {
    if ($PSVersionTable.PSEdition -eq 'Desktop') {
        [void][IO.Directory]::CreateDirectory($staging, $acl)
    } else {
        [IO.FileSystemAclExtensions]::Create([IO.DirectoryInfo]::new($staging), $acl)
    }
    $staged = $true
    [IO.Directory]::Move($staging, $directory)
    $staged = $false
    $claimed = $true
    # The protected second DVD supplies the chosen local account and tools.
    # Windows Setup still asks for edition, disk, license and regional settings.
    New-WindowsGuestAnswerISO $toolsISO $answer $PowerShellZip
    $ownsToolsISO = $true
    $vm = New-VM -Name $Name -Generation 2 -MemoryStartupBytes ($MemoryGiB * 1GB) -NoVHD -Path $directory
    Set-TestVMResources $vm $CpuCount ($MemoryGiB * 1GB)
    Get-VMNetworkAdapter -VM $vm | Remove-VMNetworkAdapter -Confirm:$false
    $null = New-VHD -Path $diskPath -SizeBytes ($DiskGiB * 1GB) -Dynamic
    if ($fromHost) { Install-CageWindowsImage $iso.FullName $diskPath $ImageEdition $ImageLanguage $answer $ImageBuild }
    Add-VMHardDiskDrive -VM $vm -Path $diskPath
    $bootDevice = $vm | Get-VMHardDiskDrive
    if (!$fromHost) { $bootDevice = Add-VMDvdDrive -VM $vm -Path $iso.FullName -Passthru }
    Grant-VMAnswerReadAccess $toolsISO $vm.Id
    Add-VMDvdDrive -VM $vm -Path $toolsISO
    Set-VMFirmware -VM $vm -EnableSecureBoot On -SecureBootTemplate MicrosoftWindows -FirstBootDevice $bootDevice
    Set-VMKeyProtector -VM $vm -NewLocalKeyProtector
    Enable-VMTPM -VM $vm
    $vm = Get-VM -Id $vm.Id
    if (@(Get-VM -Name $Name).Count -ne 1) { throw "Another VM acquired the same name '$Name'." }
    if ($vm.AutomaticStartAction -ne 'Nothing' -or @(Get-VMNetworkAdapter -VM $vm).Count -or
        (Get-VMFirmware -VM $vm).SecureBoot -ne 'On' -or !(Get-VMSecurity -VM $vm).TpmEnabled) {
        throw 'Offline, secure boot or TPM configuration did not pass the post-creation checks.'
    }
} catch {
    $creationError = $_
    if ($claimed) {
        # New-VM can fail after registering. Match the new private configuration
        # directory as well as the name before removing any partial registration.
        $prefix = $directory.TrimEnd('\') + '\'
        $owned = @(Get-VM | Where-Object {
            $_.Name -eq $Name -and ($_.Path -eq $directory -or $_.Path.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase))
        })
        foreach ($registered in $owned) {
            if ($registered.State -ne 'Off') { throw "VM is running. Preserved '$directory': $creationError" }
            Remove-VM -VM $registered -Force
        }
        # Remove-VM deals with its configuration and key protector. Delete only
        # our disk and empty directories. Unexpected files stay available for recovery.
        $root = Get-Item -LiteralPath $directory
        if ($root.Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw "Instance directory changed; preserved '$directory': $creationError"
        }
        [IO.File]::Delete($diskPath)
        if ($ownsToolsISO) { [IO.File]::Delete($toolsISO) }
        foreach ($child in $root.EnumerateDirectories()) {
            if (!($child.Attributes -band [IO.FileAttributes]::ReparsePoint) -and
                !@($child.EnumerateFileSystemInfos()).Count) { $child.Delete() }
        }
        if (!@($root.EnumerateFileSystemInfos()).Count) { $root.Delete() }
        else { Write-Warning "Unexpected files retained at '$directory'." }
    }
    throw $creationError
} finally {
    if ($staged) { [IO.Directory]::Delete($staging, $false) }
}
$result = [pscustomobject]@{ VMId = $vm.Id; Name = $vm.Name; InstanceDirectory = $directory; Disk = $diskPath }
if ($Start) {
    try { Start-VM -VM $vm } catch { throw "Created VM $($vm.Id) at '$directory'; starting it failed: $_" }
}
$result
