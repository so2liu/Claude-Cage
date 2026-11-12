# Used by complete-windows-guest.ps1 through an authenticated VM-only
# PowerShell Direct session, after the ordinary GUI SID has been pinned.
param([guid]$VMId,[string]$GUIUserSID,[string]$Bridge,[string]$Stage,[string]$PackagesJSON,[string]$CLIJSON,[string]$ExtrasSHA,[ValidateSet('Prepare','UserTools')][string]$Phase,[switch]$RuntimeInstalled)
$Packages=$PackagesJSON | ConvertFrom-Json
$CLI=$CLIJSON | ConvertFrom-Json
$ErrorActionPreference='Stop'
if ([guid](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $VMId -or
    $Bridge -ne (Join-Path $env:ProgramFiles ("Cage Guest Bridge\"+$VMId)) -or $Stage -ne (Join-Path $Bridge 'setup-packages')) {
    throw 'Incorrect guest installation target.'
}
foreach ($file in @($Packages.desktop,$Packages.winfsp,$Packages.sshfs,$CLI.uv,$CLI.python,$CLI.claude,$CLI.git,$CLI.gh)) {
    if ((Get-FileHash -LiteralPath (Join-Path $Stage $file.filename)).Hash -ne $file.sha256) { throw ('Guest package digest mismatch: '+$file.filename) }
}
$extras=Join-Path $Stage 'extras.zip'
if ((Get-FileHash -LiteralPath $extras).Hash -ne $ExtrasSHA) { throw 'Guest components digest mismatch.' }
$taskName="Cage-GUI-Bridge-$VMId-$GUIUserSID"
$bridgeTask=Get-ScheduledTask -TaskName $taskName
if ($bridgeTask.Description -cne "Cage protected GUI bridge $VMId $GUIUserSID" -or $bridgeTask.Principal.RunLevel -ne 'Limited' -or
    $bridgeTask.Actions[0].Execute -ne (Join-Path $Bridge 'Cage.GuestClipboard.exe')) { throw 'The registered bridge does not match this guest.' }

# All paths below are inside the administrator-owned bridge. Extraction and
# package operations never take a filename supplied by an ordinary guest user.
function Assert-LocalClaudeTool([string]$Path) {
    if (![IO.Path]::IsPathRooted($Path) -or $Path.StartsWith('\\') -or
        [IO.DriveInfo]::new([IO.Path]::GetPathRoot($Path)).DriveType -eq 'Network') { throw 'Claude Code must use a local tool file.' }
    $item=Get-Item -LiteralPath $Path -Force
    if ($item -isnot [IO.FileInfo]) { throw 'Claude Code must use a local tool file.' }
    while ($item) {
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'A Claude Code tool path is a reparse point.' }
        if ($item -is [IO.FileInfo]) { $item=$item.Directory } else { $item=$item.Parent }
    }
}

function Assert-ClaudeCodeReceipt($Value, $Selected, [string]$PreviousReceipt) {
    if ($Value.version -ne 1 -or $Value.claude_version -notmatch '^\d+\.\d+\.\d+$') { throw 'Invalid Claude Code version or hash.' }
    if ($Value.claude_sha256 -eq $Selected.sha256) {
        if ($Value.claude_version -cne $Selected.version) { throw 'The Claude Code receipt differs from the selected release.' }
    } else {
        $previous=Get-Content -LiteralPath $PreviousReceipt -Raw | ConvertFrom-Json
        # A retained version must already be bound by the administrator's
        # receipt. Legacy receipts without a version cannot certify a newer
        # release from a user's claim; fail while leaving that receipt intact.
        if (!$previous.PSObject.Properties['claude_version'] -or $previous.claude_version -cne $Value.claude_version -or
            $previous.claude -cne $Value.claude -or $previous.claude_sha256 -cne $Value.claude_sha256 -or
            [version]$Value.claude_version -le [version]$Selected.version) { throw 'The retained Claude Code does not match its protected version receipt.' }
    }
    Assert-LocalClaudeTool $Value.claude
    if ((Get-FileHash -LiteralPath $Value.claude -Algorithm SHA256).Hash -ne $Value.claude_sha256) { throw 'The selected Claude Code changed after installation.' }
    $signature=Get-AuthenticodeSignature -LiteralPath $Value.claude
    if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Anthropic,? PBC') { throw 'The selected Claude Code signature is invalid.' }
}

function Install-GuestComponents {
    # Windows PowerShell 5.1 ZIP APIs and Copy-Item truncate long editor
    # paths on a fresh Windows install. The OS-supplied archive tool supports
    # long paths. Only the verified, Cage-assembled application archive is
    # extracted into the administrator-owned guest bridge.
    if (!$RuntimeInstalled) {
        & (Join-Path $env:SystemRoot 'System32/tar.exe') -xf $extras -C $Bridge
        if ($LASTEXITCODE -ne 0) { throw 'Could not unpack the verified guest application components.' }
    }

    foreach ($package in @($Packages.winfsp,$Packages.sshfs)) {
        $file=Join-Path $Stage $package.filename
        $proc=Start-Process -FilePath (Join-Path $env:SystemRoot 'System32/msiexec.exe') -ArgumentList ('/i "'+$file+'" /qn /norestart') -Wait -PassThru
        if ($proc.ExitCode -notin @(0,3010)) { throw ('Windows file-system driver install failed: '+$package.filename+' ('+$proc.ExitCode+')') }
    }

    $msix=Join-Path $Stage $Packages.desktop.filename
    . (Join-Path $Bridge 'claude-desktop-windows.ps1')
    Assert-ClaudeDesktopRelease $msix $Packages.desktop.version
    Install-ProvisionedClaude $msix $GUIUserSID
    $name='Cage-Workbench-Boot-'+$VMId.ToString('D')
    $image='"'+(Join-Path $Bridge 'cage-guest-boot.exe')+'" '+$VMId.ToString('D')
    $service=Get-CimInstance Win32_Service -Filter "Name='$name'"
    if (!$service) {
        $service=New-Service -Name $name -BinaryPathName $image -StartupType Automatic -DisplayName 'Cage dedicated workbench boot sign-in'
    } elseif ($service.PathName -cne $image -or $service.StartName -ne 'LocalSystem') { throw 'An unrelated boot service was preserved.' }
    Start-Service -Name $name
}
if ($Phase -eq 'Prepare') { Install-GuestComponents; return }

. (Join-Path $Bridge 'claude-desktop-windows.ps1')
# This directory is administrator-owned. Only the separately ACL-limited
# receipt can be written by the paired ordinary user.
[IO.File]::WriteAllText((Join-Path $Stage 'resolved-cli-dependencies.json'),$CLIJSON,[Text.UTF8Encoding]::new($false))
[IO.File]::WriteAllText((Join-Path $Stage 'resolved-workbench-packages.json'),$PackagesJSON,[Text.UTF8Encoding]::new($false))
$receipt=Join-Path $Stage 'cli-receipt.in'
if (!(Test-Path -LiteralPath $receipt)) { $f=[IO.File]::Open($receipt,'CreateNew','Write','None'); $f.Dispose() }
$admin=[Security.Principal.SecurityIdentifier]::new('S-1-5-32-544')
function Set-ReceiptAccess([string]$Path,[string]$UserRights) {
    $acl=[Security.AccessControl.FileSecurity]::new()
    $acl.SetAccessRuleProtection($true,$false)
    $acl.SetOwner($admin)
    foreach ($sid in @('S-1-5-18','S-1-5-32-544')) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid),'FullControl','Allow'))
    }
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($GUIUserSID),$UserRights,'Allow'))
    Set-Acl -LiteralPath $Path -AclObject $acl
}
Set-ReceiptAccess $receipt 'Read,Write'
$cliTask="Cage-Workbench-CLI-Setup-$VMId"
$description="Cage guest workbench tool install $VMId $GUIUserSID"
if ($old=Get-ScheduledTask -TaskName $cliTask -ErrorAction SilentlyContinue) {
    if ($old.Description -cne $description -or $old.State -eq 'Running') { throw 'A workbench installation is already active.' }
    Unregister-ScheduledTask -TaskName $cliTask -Confirm:$false
}
$action=New-ScheduledTaskAction -Execute (Join-Path $env:SystemRoot 'System32/WindowsPowerShell/v1.0/powershell.exe') -Argument ('-NoProfile -NonInteractive -ExecutionPolicy RemoteSigned -File "'+(Join-Path $Bridge 'install-windows-workbench-user-tools.ps1')+'" -BundleDirectory "'+$Stage+'" -BridgeDirectory "'+$Bridge+'"')
$principal=New-ScheduledTaskPrincipal -UserId $GUIUserSID -LogonType Interactive -RunLevel Limited
$settings=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit (New-TimeSpan -Minutes 15)
$null=Register-ScheduledTask -TaskName $cliTask -Action $action -Principal $principal -Settings $settings -Description $description
try {
    [IO.File]::WriteAllText($receipt,'')
    Start-ScheduledTask -TaskName $cliTask
    $deadline=[DateTime]::UtcNow.AddMinutes(15)
    do {
        Start-Sleep -Seconds 1
        $task=Get-ScheduledTask -TaskName $cliTask
        $info=Get-ScheduledTaskInfo -TaskName $cliTask
        $ran=$info.LastRunTime.Year -gt 2000 -and $info.LastTaskResult -notin @(267008,267009,267011)
    } while ((!$ran -or $task.State -eq 'Running') -and [DateTime]::UtcNow -lt $deadline)
    if (!$ran -or $task.State -eq 'Running' -or $info.LastTaskResult -ne 0) { throw 'Claude Code tools did not finish for the signed-in standard guest user.' }
    # Copy only the bounded allowlisted JSON fields from the file opened
    # without write sharing. Never execute a user-supplied receipt path here.
    $handle=[IO.File]::Open($receipt,'Open','Read','Read')
    try {
        if ($handle.Length -lt 2 -or $handle.Length -gt 16384) { throw 'Invalid Claude Code tools receipt.' }
        $reader=[IO.StreamReader]::new($handle)
        $value=$reader.ReadToEnd() | ConvertFrom-Json
    } finally { $handle.Dispose() }
    Assert-ClaudeCodeReceipt $value $CLI.claude (Join-Path $Bridge 'cli-tools.json')
    $result=[ordered]@{version=1}
    $result.claude_version=$value.claude_version
    foreach ($name in @('python','claude','git_bash','gh')) {
        $hash=$name+'_sha256'
        if (![IO.Path]::IsPathRooted($value.$name) -or $value.$hash -notmatch '^[a-fA-F0-9]{64}$') { throw 'Invalid Claude tool record.' }
        $result[$name]=$value.$name
        $result[$hash]=$value.$hash.ToLowerInvariant()
    }
    $target=Join-Path $Bridge 'cli-tools.json'
    [IO.File]::WriteAllText($target,($result | ConvertTo-Json -Compress),[Text.UTF8Encoding]::new($false))
    Set-ReceiptAccess $target 'Read'
} finally {
    if ((Get-ScheduledTask -TaskName $cliTask).State -eq 'Running') { Stop-ScheduledTask -TaskName $cliTask }
    Unregister-ScheduledTask -TaskName $cliTask -Confirm:$false
}
if ($bridgeTask.State -eq 'Running') {
    Stop-ScheduledTask -TaskName $taskName
    $deadline=[DateTime]::UtcNow.AddSeconds(15)
    while ((Get-ScheduledTask -TaskName $taskName).State -eq 'Running' -and [DateTime]::UtcNow -lt $deadline) { Start-Sleep -Milliseconds 250 }
}
Start-ScheduledTask -TaskName $taskName

# Packages have been verified and installed. The instance-scoped host cache
# permits repairs; the guest does not need a second copy of the installers.
foreach ($package in @($Packages.desktop,$Packages.winfsp,$Packages.sshfs,$CLI.uv,$CLI.python,$CLI.claude,$CLI.git,$CLI.gh)) {
    [IO.File]::Delete((Join-Path $Stage $package.filename))
}
[IO.File]::Delete($extras)
