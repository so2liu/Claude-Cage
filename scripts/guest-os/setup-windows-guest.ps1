#requires -Version 5.1
[CmdletBinding()]
param(
  [Parameter(Mandatory)][string]$Profile,
  [Parameter(Mandatory)][string]$State,
  [Parameter(Mandatory)][guid]$InstallationID,
  [Parameter(Mandatory)][string]$IsoPath,
  [Parameter(Mandatory)][ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$ExpectedIsoSha256,
  [string]$ManifestPath,
  [string]$ManifestSHA256,
  [string]$HostUserSID = ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value),
  [object]$HostEnrollment
)
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false)
$identity=[Security.Principal.WindowsIdentity]::GetCurrent()
$principal=[Security.Principal.WindowsPrincipal]::new($identity)
. (Join-Path $PSScriptRoot 'windows-host-image.ps1')

function Wait-CageWindowsInstaller($Process, [string]$ProgressFile) {
    $lastStage = ''
    do {
        $exited = $Process.WaitForExit(250)
        $stage = ''
        $reader = $null
        try {
            $reader = [IO.StreamReader]::new([IO.File]::Open($ProgressFile, 'Open', 'Read', 'ReadWrite'))
            $stage = $reader.ReadToEnd()
        } catch [IO.IOException] { }
        finally { if ($reader) { $reader.Dispose() } }
        if ($stage -cmatch '^CAGE_GUEST_STAGE windows-11 (account|oobe|credentials|media|tools|services)$' -and $stage -cne $lastStage) {
            Write-Output $stage
            $lastStage = $stage
        }
    } while (!$exited)
}

if(!$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)){
    if($identity.User.Value -cne $HostUserSID){throw 'Continue setup with the already authorized host user.'}
    if($HostEnrollment){throw 'Original user enrollment must be created by this setup process.'}
    $worker=$null;$context=$null
    $receipt=Join-Path $Profile 'windows-install-vm.txt'
    if((Test-Path -LiteralPath $receipt -PathType Leaf) -and !(Test-Path -LiteralPath (Join-Path $Profile 'windows-guest.json'))){
        . (Join-Path $PSScriptRoot 'windows-host-enrollment.ps1')
        $expectedVM=[guid][IO.File]::ReadAllText($receipt)
        $manager=Join-Path $PSScriptRoot 'payload/Cage.HyperVManager.exe'
        if($expectedVM -eq [guid]::Empty -or !(Test-Path -LiteralPath $manager -PathType Leaf)){throw 'The fixed VM enrollment payload is unavailable.'}
        $pipe='Cage.Enroll.'+[guid]::NewGuid().ToString('N')
        $arguments=@('user-enrollment','--vm-id',$expectedVM.ToString('D'),'--host-sid',$HostUserSID,'--profile',('"'+[IO.Path]::GetFullPath($Profile)+'"'),'--state',('"'+[IO.Path]::GetFullPath($State)+'"'),'--installation-id',$InstallationID.ToString('D'),'--pipe',$pipe,'--parent-pid',[string]$PID)
        $worker=Start-Process -FilePath $manager -ArgumentList $arguments -NoNewWindow -PassThru
        $context=@{Pipe=$pipe;WorkerPID=[uint32]$worker.Id;WorkerStarted=[Cage.HostEnrollment.Native]::Started([uint32]$worker.Id);WorkerExe=[IO.Path]::GetFullPath($manager);OwnerPID=[uint32]$PID;OwnerStarted=[Cage.HostEnrollment.Native]::Started([uint32]$PID);HostSID=$HostUserSID;VMID=$expectedVM.ToString('D');InstallationID=$InstallationID.ToString('D');Profile=[IO.Path]::GetFullPath($Profile);State=[IO.Path]::GetFullPath($State)}
    }
    # Preserve every selected installation argument across the one-time UAC
    # boundary. Encoded data cannot become executable PowerShell syntax.
    $progressFile=Join-Path ([IO.Path]::GetTempPath()) ('cage-windows-setup-'+[guid]::NewGuid().ToString('N')+'.stage')
    # Hold the new file without delete sharing across UAC. The elevated writer
    # opens this existing file once, so its path cannot be replaced mid-install.
    $progressHandle=[IO.File]::Open($progressFile,'CreateNew','ReadWrite','ReadWrite')
    $parameters=@{Profile=$Profile;State=$State;InstallationID=$InstallationID.ToString('D');IsoPath=$IsoPath;ExpectedIsoSha256=$ExpectedIsoSha256;ManifestPath=$ManifestPath;ManifestSHA256=$ManifestSHA256;HostUserSID=$HostUserSID;HostEnrollment=$context;ProgressFile=$progressFile}
    $payload=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(($parameters|ConvertTo-Json -Depth 6 -Compress)))
    $script=[Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($PSCommandPath))
    $command=@'
$ErrorActionPreference='Stop'
$p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__PARAMETERS__'))|ConvertFrom-Json
$a=@{}
$p.PSObject.Properties|ForEach-Object{$a[$_.Name]=$_.Value}
$progressFile=$a.ProgressFile
$a.Remove('ProgressFile')
$s=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('__SCRIPT__'))
$progressStream=$null
try {
    $progressStream=[IO.File]::Open($progressFile,'Open','Write','ReadWrite')
    & $s @a | ForEach-Object {
        $line=[string]$_
        if ($line -cmatch '^CAGE_GUEST_STAGE windows-11 (account|oobe|credentials|media|tools|services)$') {
            $bytes=[Text.Encoding]::UTF8.GetBytes($line)
            $progressStream.Position=0
            $progressStream.SetLength(0)
            $progressStream.Write($bytes,0,$bytes.Length)
            $progressStream.Flush()
        }
        Write-Output $_
    }
    exit 0
} catch { Write-Error $_; exit 1 }
finally { if ($progressStream) { $progressStream.Dispose() } }
'@
    $command=$command.Replace('__PARAMETERS__',$payload).Replace('__SCRIPT__',$script)
    $encoded=[Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
    try{
        if($context){Wait-CageHostEnrollment $context $worker}
        $process=Start-Process -FilePath (Join-Path ([Environment]::GetFolderPath('System')) 'WindowsPowerShell/v1.0/powershell.exe') -ArgumentList @('-NoLogo','-NoProfile','-ExecutionPolicy','RemoteSigned','-EncodedCommand',$encoded) -Verb RunAs -PassThru
        # Windows PowerShell 5.1 must retain a handle before waiting to read ExitCode.
        $null=$process.Handle
        if($context){Set-CageHostEnrollmentAdmin $context $process}
        Wait-CageWindowsInstaller $process $progressFile
        if($process.ExitCode -ne 0){throw 'Windows setup did not complete; existing VM and data were preserved.'}
    }finally{
        if($context){try{Stop-CageHostEnrollment $context}catch{}}
        if($worker){if(!$worker.WaitForExit(5000) -and [Cage.HostEnrollment.Native]::Started([uint32]$worker.Id) -eq [long]$context.WorkerStarted){$worker.Kill();$worker.WaitForExit()};$worker.Dispose()}
        $progressHandle.Dispose()
        try { [IO.File]::Delete($progressFile) } catch [IO.IOException] { }
    }
    return
}
$state=[IO.Path]::GetFullPath($State)
$stateAnchor=Get-Item -LiteralPath (Join-Path $Profile 'state') -Force -ErrorAction SilentlyContinue
if ($stateAnchor -and $stateAnchor.Attributes -band [IO.FileAttributes]::ReparsePoint) {
    $expectedState=@($stateAnchor.Target)
    if ($expectedState.Count -ne 1 -or ![IO.Path]::IsPathRooted($expectedState[0]) -or [IO.Path]::GetFullPath($expectedState[0]) -ne $state) { throw 'The selected storage path changed.' }
} elseif ([IO.Path]::GetFullPath((Join-Path $Profile 'state')) -ne $state) { throw 'The selected storage path changed.' }
$instance=Join-Path $state ('windows-'+$InstallationID.ToString('N'))
$name='Cage-'+$InstallationID.ToString('N')
$receipt=Join-Path $Profile 'windows-install-vm.txt'
$owner=Get-Content -LiteralPath (Join-Path $Profile 'guest-install.json') -Raw | ConvertFrom-Json
$preparedPath = Join-Path $Profile ('guest-install-media-'+$InstallationID.ToString('D')+'.json')
if ($owner.source.host.system -ceq 'windows-11') {
    $preparedFile = Get-Item -LiteralPath $preparedPath -Force
    if ($preparedFile -isnot [IO.FileInfo] -or $preparedFile.Length -gt 32768 -or $preparedFile.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Invalid verified installation media receipt.' }
    $prepared = Get-Content -LiteralPath $preparedPath -Raw | ConvertFrom-Json
    if ([guid]$prepared.id -ne $InstallationID -or $prepared.source.digest -cne $owner.source.digest -or
        $prepared.source.host.edition -cne $owner.source.host.edition -or $prepared.source.host.language -cne $owner.source.host.language -or
        $prepared.source.version -cne $owner.source.version -or $prepared.source.build -cne $owner.source.build -or $prepared.source.format -cne 'esd') {
        throw 'The verified Windows image no longer matches the selected host system.'
    }
    $owner | Add-Member -NotePropertyName media -NotePropertyValue $prepared.source.path -Force
    $owner | Add-Member -NotePropertyName sha256 -NotePropertyValue $prepared.source.digest.Substring(7) -Force
}
if ([guid]$owner.id -ne $InstallationID -or $owner.system -ne 'windows-11' -or $owner.media -ne $IsoPath -or $owner.sha256 -ne $ExpectedIsoSha256) {
    throw 'The selected Windows installation no longer matches this profile.'
}
$fromHost = $owner.source.host.system -ceq 'windows-11'
$setupCredential = $null
$published = Test-Path -LiteralPath (Join-Path $Profile 'windows-guest.json')
$credentialPath = Join-Path $Profile ('windows-setup-'+$InstallationID.ToString('N')+'.dpapi')
if ($fromHost -and (!$published -or (Test-Path -LiteralPath $credentialPath))) {
    if ($identity.User.Value -cne $HostUserSID) { throw 'Automatic system creation requires the same Windows host account after elevation.' }
    $setupCredential = Get-CageSetupCredential $Profile $InstallationID
}
$null=New-Item -ItemType Directory -Path $state -Force
function Assert-Instance($VM) {
    if ($null -eq $VM -or $VM.Name -cne $name -or $VM.Generation -ne 2 -or
        !($VM.ConfigurationLocation -eq $instance -or $VM.ConfigurationLocation.StartsWith($instance.TrimEnd('\')+'\',[StringComparison]::OrdinalIgnoreCase)) -or
        @(Hyper-V\Get-VMNetworkAdapter -VM $VM).Count) { throw 'The selected installation no longer owns this offline virtual machine.' }
}
$readyToProvision=Test-Path -LiteralPath $receipt -PathType Leaf
if ($readyToProvision) {
    $vm=Hyper-V\Get-VM -Id ([guid][IO.File]::ReadAllText($receipt))
    Assert-Instance $vm
} else {
    $vm=@(Hyper-V\Get-VM -Name $name -ErrorAction SilentlyContinue)
    if (!$vm.Count) {
        $imageOptions = @{}
        if ($fromHost) { $imageOptions = @{ ImageEdition=$owner.source.host.edition; ImageLanguage=$owner.source.host.language; ImageBuild=$owner.source.build; GuestAdministrator=$setupCredential } }
        else { Write-Output 'CAGE_GUEST_STAGE windows-11 account' }
        $created=& (Join-Path $PSScriptRoot 'new-windows-guest.ps1') -Name $name -IsoPath $IsoPath -ExpectedIsoSha256 $ExpectedIsoSha256 -InstanceDirectory $instance -Confirm:$false @imageOptions
        $vm=Hyper-V\Get-VM -Id $created.VMId
    } elseif ($vm.Count -eq 1) { $vm=$vm[0] }
    else { throw 'The installation has an ambiguous Hyper-V registration.' }
    Assert-Instance $vm
    if (@($vm | Hyper-V\Get-VMHardDiskDrive | Where-Object Path -eq (Join-Path $instance 'system.vhdx')).Count -ne 1 -or
        (!$fromHost -and @($vm | Hyper-V\Get-VMDvdDrive | Where-Object Path -eq $IsoPath).Count -ne 1)) {
        throw 'The retained Windows installation disk or setup media does not match.'
    }
    $next=$receipt+'.'+[guid]::NewGuid().ToString('N')
    try { [IO.File]::WriteAllText($next,$vm.Id.ToString('D')); [IO.File]::Move($next,$receipt) }
    finally { if ([IO.File]::Exists($next)) { [IO.File]::Delete($next) } }
}
if ($vm.State -eq 'Off') { Hyper-V\Start-VM -VM $vm; $vm=Hyper-V\Get-VM -Id $vm.Id }
if ($vm.State -ne 'Running') { throw 'The private Windows guest is not running yet. Retry after its current operation.' }
if (!$readyToProvision -and !$fromHost) {
    Start-Process -FilePath (Join-Path ([Environment]::GetFolderPath('System')) 'vmconnect.exe') -ArgumentList @('localhost',$name)
    Write-Output 'CAGE_GUEST_STAGE windows-11 oobe'
    Write-Output '在虚拟机连接窗口完成 Windows 安装，用安装前设置的本地管理员登录桌面，再回 Cage 点击「已进入桌面，继续安装」。本轮 PowerShell 创建窗口会自动关闭。'
    return
}
$componentsReady = (Test-Path -LiteralPath (Join-Path $instance 'cage-workbench/pair/workbench-components-ready')) -and
    (Test-Path -LiteralPath (Join-Path $instance 'cage-workbench/pair/windows-guest.json'))
if ($fromHost -and $setupCredential -and !$published -and !$componentsReady) { Wait-CageWindowsSetup $vm.Id $setupCredential }
& (Join-Path $PSScriptRoot 'complete-windows-guest.ps1') -VMId $vm.Id -InstanceDirectory $instance -HostProfileDirectory $Profile -HostUserSID $HostUserSID -StateDirectory $state -HostEnrollment $HostEnrollment -ManifestPath $ManifestPath -ManifestSHA256 $ManifestSHA256 -GuestAdministrator $setupCredential
Write-Output 'CAGE_GUEST_STAGE windows-11 services'
