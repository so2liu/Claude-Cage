#requires -Version 5.1
#requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory)][guid]$VMId,
    [Parameter(Mandatory)][string]$InstanceDirectory,
    [Parameter(Mandatory)][string]$HostProfileDirectory,
    [pscredential]$GuestAdministrator,
    [string]$HostUserSID = ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value),
    [string]$StateDirectory,
    [string]$ManifestPath,
    [string]$ManifestSHA256,
    [object]$HostEnrollment
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$payload = Join-Path $PSScriptRoot 'payload'
$bridgeScripts = Join-Path $PSScriptRoot 'guest-clipboard-windows'
$componentManifest=$null
if ($ManifestPath) {
    $manifestFile=Get-Item -LiteralPath $ManifestPath -Force
    if ($ManifestSHA256 -notmatch '^[a-fA-F0-9]{64}$' -or $manifestFile -isnot [IO.FileInfo] -or $manifestFile.Length -gt 4MB -or
        $manifestFile.Attributes -band [IO.FileAttributes]::ReparsePoint -or (Get-FileHash -LiteralPath $ManifestPath).Hash -ne $ManifestSHA256) {
        throw '组件清单无法核验，请从 Cage 重新继续安装。'
    }
    $componentManifest=Get-Content -LiteralPath $ManifestPath -Raw | ConvertFrom-Json
    if ($componentManifest.protocol -ne 1 -or $componentManifest.system -cne 'windows-11' -or $componentManifest.revision -notmatch '^[a-f0-9]{64}$') { throw '原生组件清单无效。' }
    $kitRoot=[IO.Path]::GetFullPath($PSScriptRoot).TrimEnd('\')+'\'
    foreach ($entry in $componentManifest.files) {
        $source=[IO.Path]::GetFullPath((Join-Path $kitRoot $entry.source))
        if (!$source.StartsWith($kitRoot,[StringComparison]::OrdinalIgnoreCase) -or (Get-FileHash -LiteralPath $source).Hash -ne $entry.sha256) {
            throw '当前应用组件包已变化，请重新继续安装。'
        }
    }
}

function Get-LocalDirectory([string]$Path) {
    $dir = Get-Item -LiteralPath $Path -Force
    if ($dir -isnot [IO.DirectoryInfo] -or $dir.FullName.StartsWith('\\') -or
        [IO.DriveInfo]::new($dir.Root.FullName).DriveType -ne 'Fixed') { throw 'A local fixed-disk directory is required.' }
    for ($parent=$dir; $parent; $parent=$parent.Parent) {
        if ($parent.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'The installation path crosses a filesystem link.' }
    }
    return $dir.FullName
}

. (Join-Path $PSScriptRoot 'windows-workbench-packages.ps1')
. (Join-Path $PSScriptRoot 'windows-host-image.ps1')

function Read-CageBinding([string]$Path) {
    $item=Get-Item -LiteralPath $Path -Force
    if ($item -isnot [IO.FileInfo] -or $item.Attributes -band [IO.FileAttributes]::ReparsePoint -or $item.Length -gt 16384) { throw 'Invalid existing Windows guest binding.' }
    $data=Get-Content -LiteralPath $item.FullName -Raw | ConvertFrom-Json
    if ($data.version -ne 1 -or [guid]$data.vm_id -ne $VMId -or $data.instance_directory -ne $instance -or
        $data.relay_executable -ne (Join-Path $pairDirectory 'Cage.GuestClipboard.exe') -or
        $data.pairing_file -ne (Join-Path $pairDirectory 'pairing.dpapi') -or
        ($data.network_pairing_file -and $data.network_pairing_file -ne (Join-Path $pairDirectory 'packet.dpapi'))) {
        throw 'A different guest is already bound. Its disk and pairing were preserved.'
    }
    return $data
}

# The current package's own relay performs the proof, regardless of the path
# stored in the staged profile. A cold bridge may need a few seconds after its
# logon task restarts. Never turn a missing registration into a ready profile.
function Ensure-HostManager {
    $manager=Join-Path $payload 'Cage.HyperVManager.exe'
    if(!(Test-Path -LiteralPath $manager -PathType Leaf)){throw 'The complete Windows host-manager payload is required.'}
    & (Join-Path $PSScriptRoot 'install-windows-host-manager.ps1') -VMId $VMId -HostSID $HostUserSID -PayloadExecutable $manager -PayloadSHA256 (Get-FileHash -LiteralPath $manager -Algorithm SHA256).Hash -Confirm:$false | Out-Null
}

function Restore-MissingHostEnrollmentKeys($Binding) {
    if(!$HostEnrollment){return}
    . (Join-Path $PSScriptRoot 'windows-host-enrollment.ps1')
    foreach($kind in @('gui','packet')){
        $path=if($kind -ceq 'gui'){$Binding.pairing_file}else{$Binding.network_pairing_file}
        if(!$path -or (Test-Path -LiteralPath $path)){continue}
        # Only this fresh VM's fixed Cage keys are recovered. Existing host
        # DPAPI bytes and model credentials are never replaced or read here.
        $key=Invoke-Command -Session $session -ArgumentList $VMId,$Binding.gui_user_sid,$kind -ScriptBlock {
            param($expected,$sid,$kind)
            if([guid](Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $expected){throw 'guest_identity_mismatch'}
            $name=if($kind -ceq 'gui'){'GuestBridge'}else{'GuestNetwork'}
            $file=if($kind -ceq 'gui'){'pairing.bin'}else{'packet.bin'}
            $path=Join-Path (Join-Path (Join-Path $env:ProgramData 'Cage') $name) ($expected.ToString('D')+'\'+$file)
            $item=Get-Item -LiteralPath $path -Force
            if($item -isnot [IO.FileInfo] -or $item.Length -ne 32 -or $item.Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'fresh_pairing_invalid'}
            [IO.File]::ReadAllBytes($path)
        }
        $bytes=[byte[]]$key
        try{if($bytes.Length -ne 32){throw 'The retained fresh Cage pairing is invalid.'};Send-CageHostEnrollmentKey $HostEnrollment $kind $Binding.gui_user_sid $bytes}finally{[Array]::Clear($bytes,0,$bytes.Length)}
    }
}

function Complete-CloneSetup([string]$Binding) {
    if ($choice.source.host.system -cne 'windows-11') { return }
    $answerPath = Join-Path $instance 'windows-guest-tools.iso'
    $vm | Hyper-V\Get-VMDvdDrive | Where-Object Path -eq $answerPath | Hyper-V\Remove-VMDvdDrive
    [IO.File]::Delete($answerPath)
    Remove-CageSetupCredential $VMId $GuestAdministrator $profile $installID $Binding
}

function Publish-ReadyBinding {
    $binding=Read-CageBinding $stageBinding
    if (!$binding.network_pairing_file) { throw 'The protected packet pairing is unavailable.' }
    if($HostEnrollment){
        Ensure-HostManager
        . (Join-Path $PSScriptRoot 'windows-host-enrollment.ps1')
        Complete-CloneSetup $stageBinding
        Complete-CageHostEnrollment $HostEnrollment $binding.gui_user_sid
        return
    }
    $relay=Join-Path $payload 'Cage.GuestClipboard.exe'
    $deadline=[DateTime]::UtcNow.AddSeconds(30)
    $ready=$false
    do {
        # Windows PowerShell 5.1 turns native stderr into a terminating error
        # under Stop, even when redirected. A cold relay must reach the retry.
        $savedPreference=$ErrorActionPreference
        try { $ErrorActionPreference='Continue'; $raw=& $relay --relay-profile $stageBinding status 2>$null }
        finally { $ErrorActionPreference=$savedPreference }
        if ($LASTEXITCODE -eq 0 -and $raw) {
            $state=$raw | ConvertFrom-Json
            if ($state.available -and [guid]$state.vm_id -eq $VMId -and $state.gui_user_sid -eq $binding.gui_user_sid -and
                $state.browser.installed -and $state.browser.editor_installed -and
                $state.capabilities -contains 'cli' -and $state.capabilities -contains 'project-mount' -and $state.capabilities -contains 'desktop') {
                try { $ErrorActionPreference='Continue'; $raw='{"action":"status"}' | & $relay --relay-profile $stageBinding desktop 2>$null }
                finally { $ErrorActionPreference=$savedPreference }
                if ($LASTEXITCODE -eq 0 -and $raw -and ($raw | ConvertFrom-Json).installed) { $ready=$true; break }
            }
        }
        Start-Sleep -Seconds 1
    } while ([DateTime]::UtcNow -lt $deadline)
    if (!$ready) { throw 'Sign out and sign back in to the paired standard guest account to finish Windows app registration. Resume setup from Cage.' }
    # The ordinary host controller uses only this protected fixed-VM service.
    # Host identity is distinct from the GUI account inside the Windows guest.
    Ensure-HostManager
    # Complete retirement before exposing a ready profile. The staged pairing
    # proves an already retired account when a prior publish was interrupted.
    Complete-CloneSetup $stageBinding
    $tempBinding=Join-Path $profile ('windows-guest.'+[guid]::NewGuid()+'.tmp')
    try {
        [IO.File]::Copy($stageBinding,$tempBinding,$false)
        Set-Acl -LiteralPath $tempBinding -AclObject (Get-Acl -LiteralPath $stageBinding)
        [IO.File]::Move($tempBinding,$finalBinding)
    } finally { if ([IO.File]::Exists($tempBinding)) { [IO.File]::Delete($tempBinding) } }
}

$instance=Get-LocalDirectory $InstanceDirectory
$profile=Get-LocalDirectory $HostProfileDirectory
$null=Get-LocalDirectory $payload
$choice=Get-Content -LiteralPath (Join-Path $profile 'guest-install.json') -Raw | ConvertFrom-Json
$installID=[guid]$choice.id
$anchor=Get-Item -LiteralPath (Join-Path $profile 'state') -Force
if(!$StateDirectory){$StateDirectory=if($anchor.Attributes -band [IO.FileAttributes]::ReparsePoint){$targets=@($anchor.Target);if($targets.Count -ne 1){throw 'The selected physical state directory changed.'};$targets[0]}else{$anchor.FullName}}
$physicalState=Get-LocalDirectory $StateDirectory
if($anchor.Attributes -band [IO.FileAttributes]::ReparsePoint){$targets=@($anchor.Target);if($targets.Count -ne 1 -or [IO.Path]::GetFullPath($targets[0]) -ine $physicalState){throw 'The selected physical state directory changed.'}}
elseif($anchor.FullName -ine $physicalState){throw 'The selected physical state directory changed.'}
if ($installID -eq [guid]::Empty -or $choice.system -ne 'windows-11' -or $choice.editor_terms -ne $true -or
    [guid][IO.File]::ReadAllText((Join-Path $profile 'windows-install-vm.txt')) -ne $VMId -or
    $instance -ine (Join-Path $physicalState ('windows-'+$installID.ToString('N')))) {
    throw 'Automatic boot sign-in is restricted to the new VM in this first-run installation.'
}
if($HostEnrollment -and ($HostEnrollment.HostSID -cne $HostUserSID -or [guid]$HostEnrollment.VMID -ne $VMId -or [guid]$HostEnrollment.InstallationID -ne $installID -or
    [IO.Path]::GetFullPath($HostEnrollment.Profile) -ine $profile -or [IO.Path]::GetFullPath($HostEnrollment.State) -ine $physicalState)){throw 'Original user enrollment does not match this fixed installation.'}
$vm=Hyper-V\Get-VM -Id $VMId
if ($VMId -eq [guid]::Empty -or $vm.Name -cne ('Cage-'+$installID.ToString('N')) -or $vm.Id -ne $VMId -or $vm.State -ne 'Running' -or $vm.Generation -ne 2 -or
    !($vm.ConfigurationLocation -eq $instance -or $vm.ConfigurationLocation.StartsWith($instance.TrimEnd('\')+'\',[StringComparison]::OrdinalIgnoreCase)) -or
    @(Hyper-V\Get-VMNetworkAdapter -VM $vm).Count -ne 0 -or @($vm | Hyper-V\Get-VMHardDiskDrive).Count -ne 1 -or
    ($vm | Hyper-V\Get-VMHardDiskDrive).Path -ne (Join-Path $instance 'system.vhdx')) {
    throw 'Start the selected, isolated Windows guest and finish Windows Setup in its console.'
}

$workRoot=Join-Path $instance 'cage-workbench'
$pairDirectory=Join-Path $workRoot 'pair'
$stageBinding=Join-Path $pairDirectory 'windows-guest.json'
$completionMarker=Join-Path $pairDirectory 'workbench-components-ready'
$finalBinding=Join-Path $profile 'windows-guest.json'
if (Test-Path -LiteralPath $finalBinding) {
    $existing=Read-CageBinding $finalBinding
    if (!$existing.network_pairing_file) { throw 'The existing published profile is missing its network pairing.' }
    if (Test-Path -LiteralPath (Join-Path $profile ('windows-setup-'+$installID.ToString('N')+'.dpapi'))) {
        Complete-CloneSetup $finalBinding
    }
    Ensure-HostManager
    return
}
if([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -cne $HostUserSID -and !$HostEnrollment){throw 'Fresh setup for another host user requires the original user enrollment process.'}

$null=New-Item -ItemType Directory -Force $workRoot
$null=Get-LocalDirectory $workRoot
$lockPath=Join-Path $workRoot 'setup.lock'
if ((Test-Path -LiteralPath $lockPath) -and ((Get-Item -LiteralPath $lockPath -Force).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Invalid setup lock.' }
$lock=[IO.File]::Open($lockPath,'OpenOrCreate','ReadWrite','None')
$session=$null
$scratch=$null
try {
    if ((Test-Path -LiteralPath $completionMarker) -and (Test-Path -LiteralPath $stageBinding)) {
        Publish-ReadyBinding
        return
    }
    if (!$GuestAdministrator) {
        Write-Output 'CAGE_GUEST_STAGE windows-11 credentials'
        $GuestAdministrator=Get-Credential -Message '先在虚拟机连接窗口完成 Windows 设置并登录桌面，再输入客机管理员账户。新建 ISO 安装请使用安装前设置的本地账户。请保持此 PowerShell 安装窗口开启。'
    } else { Write-Output 'CAGE_GUEST_STAGE windows-11 services' }
    if (!$GuestAdministrator) { throw 'Windows guest setup canceled.' }
    $session=New-PSSession -VMId $VMId -Credential $GuestAdministrator
    if ($choice.source.host.system -cne 'windows-11') { Remove-CageISOAnswer $vm $session $instance }
    Write-Output 'CAGE_GUEST_STAGE windows-11 services'
    $target=Invoke-Command -Session $session -FilePath (Join-Path $PSScriptRoot 'initialize-windows-workbench-user.ps1') -ArgumentList $VMId
    $firstBoot=Join-Path $pairDirectory 'first-account-boot'
    if (!$target.AlreadySignedIn -and (Test-Path -LiteralPath $firstBoot) -and [IO.File]::ReadAllText($firstBoot) -ne $target.BootID) {
        throw 'The guest has rebooted. Wait for the dedicated workbench account to sign in, or use the Windows sign-in screen if it was manually locked.'
    }

    if (Test-Path -LiteralPath $stageBinding) {
        $staged=Read-CageBinding $stageBinding
        if ($staged.gui_user_sid -ne $target.SID) { throw 'Sign in to the already paired Windows guest account to resume.' }
    }
    $cache=Join-Path $workRoot 'downloads'
    $null=New-Item -ItemType Directory -Force $cache
    $null=Get-LocalDirectory $cache
    $packages=Get-Content -LiteralPath (Join-Path $payload 'windows-workbench-packages.json') -Raw | ConvertFrom-Json
    $cli=Get-Content -LiteralPath (Join-Path $payload 'cli-dependencies.json') -Raw | ConvertFrom-Json
    Resolve-ClaudePackages $packages $cli $cache
    $files=@{}
    Write-Output 'CAGE_GUEST_STAGE windows-11 media'
    foreach ($spec in @($packages.PSObject.Properties)+@($cli.PSObject.Properties)) {
        $files[$spec.Name]=Get-PinnedPackage $spec.Value $cache
    }

    Write-Output 'CAGE_GUEST_STAGE windows-11 tools'
    $scratch=Join-Path $workRoot ('stage-'+[guid]::NewGuid())
    $null=New-Item -ItemType Directory $scratch
    $core=Join-Path $scratch 'core'
    $null=New-Item -ItemType Directory $core
    foreach ($name in @('Cage.GuestClipboard.exe','cage-guest-files.exe','cage-guest-boot.exe')) { Copy-Item -LiteralPath (Join-Path $payload $name) -Destination $core }
    if (!(Test-Path -LiteralPath $stageBinding)) {
        $null=& (Join-Path $bridgeScripts 'install.ps1') -VMId $VMId -InstanceDirectory $instance -GUIUserSID $target.SID -GuestAdministrator $GuestAdministrator -PayloadDirectory $core -HostProfileDirectory $pairDirectory -HostUserSID $HostUserSID -HostEnrollment $HostEnrollment -Confirm:$false
    }
    $staged=Read-CageBinding $stageBinding
    if ($staged.gui_user_sid -ne $target.SID) { throw 'The staged bridge belongs to another user.' }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    if (!$staged.network_pairing_file) {
        $driver=Join-Path $scratch 'driver'
        [IO.Compression.ZipFile]::ExtractToDirectory($files.wintun,$driver)
        $null=& (Join-Path $bridgeScripts 'install-network.ps1') -ProfilePath $stageBinding -GuestAdministrator $GuestAdministrator -AgentPath (Join-Path $core 'Cage.GuestClipboard.exe') -WintunPath (Join-Path $driver 'wintun/bin/amd64/wintun.dll') -WintunLicense (Join-Path $driver 'wintun/LICENSE.txt') -HostEnrollment $HostEnrollment -Confirm:$false
    }
    $staged=Read-CageBinding $stageBinding
    Restore-MissingHostEnrollmentKeys $staged

    $extras=Join-Path $scratch 'extras'
    $null=New-Item -ItemType Directory $extras
    foreach ($name in @('modules','split-extension','windows-browser.mjs','windows-browser-extensions.mjs','windows-phone-browser.mjs','install-windows-workbench-user-tools.ps1','install-cli.ps1','cli-dependencies.json','claude-desktop-windows.ps1')) {
        Copy-Item -LiteralPath (Join-Path $payload $name) -Destination $extras -Recurse
    }
    if ($componentManifest) {
        foreach ($entry in $componentManifest.files) {
            if (!$entry.target -or !$entry.target.StartsWith('bridge/')) { continue }
            $relative=$entry.target.Substring(7)
            if ($relative -in @('Cage.GuestClipboard.exe','cage-guest-files.exe','cage-guest-boot.exe')) { continue }
            if ($entry.source -cne ('payload/'+$relative) -or $relative -match '(^|/)\.\.?(/|$)|[:\\]') { throw '组件安装位置无效。' }
            $targetFile=Join-Path $extras $relative
            $null=New-Item -ItemType Directory -Path ([IO.Path]::GetDirectoryName($targetFile)) -Force
            Copy-Item -LiteralPath (Join-Path $PSScriptRoot $entry.source) -Destination $targetFile -Force
            if ((Get-FileHash -LiteralPath $targetFile).Hash -ne $entry.installed_sha256) { throw '复制后的组件摘要不匹配。' }
        }
    }
    $nodeArchive=[IO.Compression.ZipFile]::OpenRead($files.node)
    try { [IO.Compression.ZipFileExtensions]::ExtractToFile($nodeArchive.GetEntry('node-v24.22.2-win-x64/node.exe'),(Join-Path $extras 'node.exe')) } finally { $nodeArchive.Dispose() }
    Expand-PinnedTar $files.playwright (Join-Path $extras 'playwright')
    Expand-PinnedTar $files.playwrightCore (Join-Path $extras 'playwright/node_modules/playwright-core')
    [IO.Compression.ZipFile]::ExtractToDirectory($files.editorCLI,(Join-Path $extras 'vscode-cli'))
    Expand-PinnedTar $files.editorServer (Join-Path $extras 'vscode-server')
    [IO.File]::WriteAllText((Join-Path $extras 'editor-runtime.json'),'{"commit":"261c84cb1eddf11235f9e2c921425e9663fbe556"}')
    [IO.File]::WriteAllText((Join-Path $extras 'guest-config.json'),'{"network_mode":"packet"}')
    # Transfer large trees in one archive over the authenticated VM session.
    $zipPath=Join-Path $scratch 'extras.zip'
    & (Join-Path $env:SystemRoot 'System32/tar.exe') -a -cf $zipPath -C $extras .
    if ($LASTEXITCODE -ne 0) { throw 'Could not stage the guest application archive.' }
    $guestStage=Invoke-Command -Session $session -ArgumentList $target.Program -ScriptBlock {
        param($program)
        $path=Join-Path $program 'setup-packages'
        if (Test-Path -LiteralPath $path) { if ((Get-Item -LiteralPath $path -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Invalid protected staging directory.' } }
        else { $null=New-Item -ItemType Directory $path }
        $path
    }
    foreach ($file in @($zipPath,$files.desktop,$files.winfsp,$files.sshfs,$files.uv,$files.python,$files.claude,$files.git,$files.gh)) {
        Copy-Item -LiteralPath $file -Destination $guestStage -ToSession $session -Force
    }
    $installer=Join-Path $PSScriptRoot 'install-windows-workbench-components.ps1'
    $options=@($VMId,$target.SID,$target.Program,$guestStage,($packages | ConvertTo-Json -Depth 6 -Compress),($cli | ConvertTo-Json -Depth 6 -Compress),(Get-FileHash -LiteralPath $zipPath).Hash)
    $null=Invoke-Command -Session $session -FilePath $installer -ArgumentList ($options+@('Prepare'))
    if (!$target.AlreadySignedIn) {
        # Only this named new VM is rebooted, once. A later manual lock is
        # never treated as a request to restart or unlock Windows.
        [IO.File]::WriteAllText($firstBoot,$target.BootID)
        Remove-PSSession $session; $session=$null
        Hyper-V\Stop-VM -VM $vm -ErrorAction Stop
        Hyper-V\Start-VM -VM $vm -ErrorAction Stop
        $deadline=[DateTime]::UtcNow.AddMinutes(4)
        do {
            try {
                $session=New-PSSession -VMId $VMId -Credential $GuestAdministrator -ErrorAction Stop
                $logged=Invoke-Command -Session $session -ArgumentList $VMId,$target.Name -ScriptBlock {
                    param($expected,$name)
                    if ([guid](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $expected) {throw 'wrong VM'}
                    (Get-CimInstance Win32_ComputerSystem).UserName -eq ($env:COMPUTERNAME+'\'+$name)
                }
                if ($logged) {break}
            } catch { $logged=$false }
            if ($session) { Remove-PSSession $session; $session=$null }
            Start-Sleep -Seconds 2
        } while ([DateTime]::UtcNow -lt $deadline)
        if (!$session -or !$logged) { throw 'Windows did not finish its first sign-in to the dedicated workbench account. Its account and disk are preserved.' }
    }
    $null=Invoke-Command -Session $session -FilePath $installer -ArgumentList ($options+@('UserTools'))
    if (@(Hyper-V\Get-VMNetworkAdapter -VM (Hyper-V\Get-VM -Id $VMId)).Count -ne 0) { throw 'The Windows guest networking has changed.' }
    if ($componentManifest) {
        $null=Invoke-Command -Session $session -FilePath (Join-Path $PSScriptRoot 'publish-windows-component-manifest.ps1') -ArgumentList $VMId,$target.SID,($componentManifest|ConvertTo-Json -Depth 8 -Compress)
    }

    [IO.File]::WriteAllText($completionMarker,$target.SID)
    Write-Output 'CAGE_GUEST_STAGE windows-11 services'
    Publish-ReadyBinding
} finally {
    if ($session) { Remove-PSSession $session }
    if ($scratch -and (Test-Path -LiteralPath $scratch)) { Remove-Item -LiteralPath $scratch -Recurse -Force }
    $lock.Dispose()
}
