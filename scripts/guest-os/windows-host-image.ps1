#requires -Version 5.1
# Functions used only for a new private Windows guest disk.
function New-CageISOAnswer([pscredential]$Administrator) {
    if (!$Administrator -or $Administrator.UserName.Length -gt 20 -or
        $Administrator.UserName -match '["/\\\[\]:;|=,+*?<>@\x00-\x1f]' -or
        $Administrator.UserName.Trim() -cne $Administrator.UserName -or $Administrator.UserName.EndsWith('.') -or
        $Administrator.UserName -in @('Administrator', 'Guest', 'DefaultAccount', 'WDAGUtilityAccount', 'cagesetup') -or
        $Administrator.Password.Length -eq 0) {
        throw '请输入新的客机本地管理员名称和非空密码。用户名最多 20 个字符，请使用普通本地名称。'
    }
    $answer = [xml]@'
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
  <settings pass="specialize"/>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE><HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE></OOBE>
      <UserAccounts><LocalAccounts><LocalAccount wcm:action="add"><Name/><Description>Cage ISO initial administrator</Description><Group>Administrators</Group><Password><Value/><PlainText>true</PlainText></Password></LocalAccount></LocalAccounts></UserAccounts>
    </component>
  </settings>
</unattend>
'@
    $ns = [Xml.XmlNamespaceManager]::new($answer.NameTable)
    $ns.AddNamespace('u', 'urn:schemas-microsoft-com:unattend')
    $answer.SelectSingleNode('//u:LocalAccount/u:Name', $ns).InnerText = $Administrator.UserName
    $answer.SelectSingleNode('//u:LocalAccount/u:Password/u:Value', $ns).InnerText = $Administrator.GetNetworkCredential().Password
    return $answer
}

function Remove-CageISOAnswer($VM, $Session, [string]$InstanceDirectory) {
    Invoke-Command -Session $Session -ArgumentList $VM.Id -ScriptBlock {
        param($expected)
        if ([guid](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $expected) { throw 'wrong guest' }
        $setup = Get-ItemProperty 'HKLM:\SYSTEM\Setup'
        if ($setup.SystemSetupInProgress -ne 0 -or $setup.OOBEInProgress -ne 0 -or !(Test-Path 'C:\Program Files\PowerShell\7\pwsh.exe')) {
            throw '请在虚拟机连接窗口完成 Windows 首次设置并登录桌面，然后回 Cage 继续安装。'
        }
        foreach ($path in @('C:\Windows\Panther\Unattend.xml', 'C:\Windows\Panther\Unattend\Unattend.xml')) {
            if (!(Test-Path -LiteralPath $path -PathType Leaf)) { continue }
            try { $answer = [xml][IO.File]::ReadAllText($path) }
            catch { continue } # Preserve an unrecognized answer without logging its contents.
            $ns = [Xml.XmlNamespaceManager]::new($answer.NameTable)
            $ns.AddNamespace('u', 'urn:schemas-microsoft-com:unattend')
            if ($answer.SelectSingleNode('//u:LocalAccount[u:Description="Cage ISO initial administrator"]', $ns)) {
                Remove-Item -LiteralPath $path -Force
            }
        }
    }
    $answerPath = Join-Path $InstanceDirectory 'windows-guest-tools.iso'
    $VM | Hyper-V\Get-VMDvdDrive | Where-Object Path -eq $answerPath | Hyper-V\Remove-VMDvdDrive
    [IO.File]::Delete($answerPath)
}

function New-CageCloneAnswer([pscredential]$Administrator, [string]$Language) {
    if (!$Administrator -or $Administrator.UserName -cne 'cagesetup' -or $Language -notmatch '^[a-zA-Z]{2,3}-[a-zA-Z0-9-]{2,12}$') {
        throw 'The new guest setup identity or language is invalid.'
    }
    $answer = [xml]@'
<unattend xmlns="urn:schemas-microsoft-com:unattend" xmlns:wcm="http://schemas.microsoft.com/WMIConfig/2002/State">
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>CAGE-WINDOWS</ComputerName><TimeZone>UTC</TimeZone>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-International-Core" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <InputLocale>en-US</InputLocale><SystemLocale>en-US</SystemLocale><UILanguage>en-US</UILanguage><UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE><HideEULAPage>true</HideEULAPage><HideOnlineAccountScreens>true</HideOnlineAccountScreens><HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE><ProtectYourPC>3</ProtectYourPC></OOBE>
      <UserAccounts><LocalAccounts><LocalAccount wcm:action="add"><Name>cagesetup</Name><Group>Administrators</Group><Password><Value/><PlainText>true</PlainText></Password></LocalAccount></LocalAccounts></UserAccounts>
    </component>
  </settings>
</unattend>
'@
    $ns = [Xml.XmlNamespaceManager]::new($answer.NameTable)
    $ns.AddNamespace('u', 'urn:schemas-microsoft-com:unattend')
    foreach ($name in @('InputLocale', 'SystemLocale', 'UILanguage', 'UserLocale')) {
        $answer.SelectSingleNode('//u:component[@name="Microsoft-Windows-International-Core"]/u:'+$name, $ns).InnerText = $Language
    }
    $answer.SelectSingleNode('//u:LocalAccount/u:Password/u:Value', $ns).InnerText = $Administrator.GetNetworkCredential().Password
    return $answer
}

function Install-CageWindowsImage([string]$Image, [string]$Disk, [string]$Edition, [string]$Language, [xml]$Answer, [string]$Build) {
    $candidates = @()
    foreach ($summary in @(Get-WindowsImage -ImagePath $Image)) {
        $details = Get-WindowsImage -ImagePath $Image -Index $summary.ImageIndex
        if ($details.EditionId -eq $Edition -and [string]$details.Architecture -in @('9','amd64','x64') -and $details.Languages -contains $Language) {
            $candidates += $details
        }
    }
    if ($candidates.Count -ne 1) { throw 'The Windows image does not contain exactly one matching edition, architecture and language.' }
    if ($Build -and [string]$candidates[0].Version -cne ('10.0.'+$Build)) { throw 'The Windows image build differs from the selected installation preview.' }
    $mount = $null
    try {
        $mount = Mount-VHD -Path $Disk -Passthru
        $target = Get-Disk -Number $mount.DiskNumber
        if ($target.PartitionStyle -ne 'RAW' -or $target.IsBoot -or $target.IsSystem) {
            throw 'Windows installation requires the newly created empty virtual disk.'
        }
        $target | Initialize-Disk -PartitionStyle GPT
        $efi = $target | New-Partition -Size 260MB -GptType '{c12a7328-f81f-11d2-ba4b-00a0c93ec93b}' -AssignDriveLetter
        $efi | Format-Volume -FileSystem FAT32 -NewFileSystemLabel CAGE_BOOT -Confirm:$false | Out-Null
        $target | New-Partition -Size 16MB -GptType '{e3c9e316-0b5c-4db8-817d-f92df00215ae}' | Out-Null
        $windows = $target | New-Partition -UseMaximumSize -AssignDriveLetter
        $windows | Format-Volume -FileSystem NTFS -NewFileSystemLabel CAGE_WINDOWS -Confirm:$false | Out-Null
        $destination = $windows.DriveLetter + ':\'
        Write-Output '正在将匹配本机版本的干净 Windows 写入独立虚拟磁盘…'
        Expand-WindowsImage -ImagePath $Image -Index $candidates[0].ImageIndex -ApplyPath $destination -Compact -CheckIntegrity | Out-Null
        $panther = Join-Path $destination 'Windows\Panther'
        $null = [IO.Directory]::CreateDirectory($panther)
        $Answer.Save((Join-Path $panther 'Unattend.xml'))
        $boot = Join-Path ([Environment]::GetFolderPath('System')) 'bcdboot.exe'
        & $boot (Join-Path $destination 'Windows') /s ($efi.DriveLetter+':') /f UEFI
        if ($LASTEXITCODE -ne 0) { throw 'Could not prepare the new guest virtual disk boot files.' }
    } finally {
        if ($mount) { Dismount-VHD -Path $Disk }
    }
}

function Get-CageSetupCredential([string]$Profile, [guid]$InstallationID) {
    $path = Join-Path $Profile ('windows-setup-'+$InstallationID.ToString('N')+'.dpapi')
    if (Test-Path -LiteralPath $path) {
        $item = Get-Item -LiteralPath $path -Force
        if ($item -isnot [IO.FileInfo] -or $item.Length -gt 16384 -or $item.Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw 'The saved guest setup credential is invalid.'
        }
        $password = Get-Content -LiteralPath $path -Raw | ConvertTo-SecureString
    } else {
        $instance = Join-Path (Join-Path $Profile 'state') ('windows-'+$InstallationID.ToString('N'))
        $receipt = Join-Path $Profile 'windows-install-vm.txt'
        $stagedBinding = Join-Path $instance 'cage-workbench/pair/windows-guest.json'
        if ((Test-Path -LiteralPath $receipt -PathType Leaf) -and
            (Test-Path -LiteralPath (Join-Path $instance 'cage-workbench/pair/workbench-components-ready')) -and
            (Test-CageSetupAccountDisabled ([guid][IO.File]::ReadAllText($receipt)) $stagedBinding)) {
            return $null
        }
        if ((Test-Path -LiteralPath $receipt) -or (Test-Path -LiteralPath $instance)) {
            throw 'The saved setup credential for this Windows guest is missing. Restore the original encrypted credential to continue with its existing account and disk.'
        }
        $random = New-Object byte[] 32
        $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
        try { $generator.GetBytes($random); $password = ConvertTo-SecureString ('Cg!'+[Convert]::ToBase64String($random)) -AsPlainText -Force }
        finally { $generator.Dispose(); [Array]::Clear($random,0,$random.Length) }
        $encrypted = ConvertFrom-SecureString $password
        $file = [IO.File]::Open($path, 'CreateNew', 'Write', 'None')
        try {
            $bytes = [Text.Encoding]::UTF8.GetBytes($encrypted)
            $file.Write($bytes,0,$bytes.Length)
        } finally { $file.Dispose() }
    }
    return [pscredential]::new('cagesetup', $password)
}

function Wait-CageWindowsSetup([guid]$VMId, [pscredential]$Credential) {
    $deadline = [DateTime]::UtcNow.AddMinutes(20)
    Write-Output '正在等待 Windows 完成首次设置…'
    do {
        $session = $null
        try {
            $session = New-PSSession -VMId $VMId -Credential $Credential -ErrorAction Stop
            $ready = Invoke-Command -Session $session -ArgumentList $VMId -ScriptBlock {
                param($expected)
                if ([guid](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $expected) { throw 'wrong guest' }
                $setup = Get-ItemProperty 'HKLM:\SYSTEM\Setup'
                $setup.SystemSetupInProgress -eq 0 -and $setup.OOBEInProgress -eq 0 -and (Test-Path 'C:\Program Files\PowerShell\7\pwsh.exe')
            }
            if ($ready) { return }
        } catch { Write-Verbose $_ }
        finally { if ($session) { Remove-PSSession $session } }
        Start-Sleep -Seconds 3
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'Windows setup is still incomplete. The private disk and progress are saved. Continue installation from Cage.'
}

function Test-CageSetupAccountDisabled([guid]$VMId, [string]$Binding) {
    if (!(Test-Path -LiteralPath $binding -PathType Leaf)) { return $false }
    $relay = Join-Path $PSScriptRoot 'payload/Cage.GuestClipboard.exe'
    $savedPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $raw = & $relay --relay-profile $binding status 2>$null
        if ($LASTEXITCODE -ne 0 -or !$raw) { return $false }
        $status = $raw | ConvertFrom-Json -ErrorAction Stop
        return [guid]$status.vm_id -eq $VMId -and $status.setup_account_disabled -is [bool] -and $status.setup_account_disabled
    } catch { return $false }
    finally { $ErrorActionPreference = $savedPreference }
}

function Remove-CageSetupCredential([guid]$VMId, [pscredential]$Credential, [string]$Profile, [guid]$InstallationID, [string]$Binding) {
    $path = Join-Path $Profile ('windows-setup-'+$InstallationID.ToString('N')+'.dpapi')
    if (!$Binding) { $Binding = Join-Path $Profile 'windows-guest.json' }
    # A crash can occur after guest retirement but before the host deletes its
    # encrypted copy. The paired standard-user channel can prove that state.
    if (Test-CageSetupAccountDisabled $VMId $Binding) {
        [IO.File]::Delete($path)
        return
    }
    if (!$Credential) { throw 'The setup account retirement could not be verified. Its existing account and disk were preserved.' }
    $session = New-PSSession -VMId $VMId -Credential $Credential
    try {
        Invoke-Command -Session $session -ArgumentList $VMId -ScriptBlock {
            param($expected)
            if ([guid](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $expected) { throw 'wrong guest' }
            foreach ($path in @('C:\Windows\Panther\Unattend.xml', 'C:\Windows\Panther\Unattend\Unattend.xml')) {
                if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
            }
            Disable-LocalUser -Name cagesetup
        }
    } finally { Remove-PSSession $session }
    [IO.File]::Delete($path)
}
