#requires -Version 5.1
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'windows-host-image.ps1')
function Assert-Check([bool]$Condition, [string]$Message) { if (!$Condition) { throw $Message } }
$credential = [pscredential]::new('CageAdmin', (ConvertTo-SecureString 'Test-only<&>"Account!123' -AsPlainText -Force))
$answer = New-CageISOAnswer $credential
$ns = [Xml.XmlNamespaceManager]::new($answer.NameTable)
$ns.AddNamespace('u', 'urn:schemas-microsoft-com:unattend')
$roundtrip = [xml]$answer.OuterXml
Assert-Check ($roundtrip.SelectSingleNode('//u:LocalAccount/u:Name', $ns).InnerText -ceq 'CageAdmin') 'Initial account name changed.'
Assert-Check ($roundtrip.SelectSingleNode('//u:LocalAccount/u:Password/u:Value', $ns).InnerText -ceq $credential.GetNetworkCredential().Password) 'Password XML escaping changed the credential.'
Assert-Check ($answer.SelectSingleNode('//u:LocalAccount/u:Group', $ns).InnerText -ceq 'Administrators') 'PowerShell Direct needs the initial administrator.'
foreach ($setting in @('HideOnlineAccountScreens', 'HideWirelessSetupInOOBE')) {
    Assert-Check ($answer.SelectSingleNode('//u:OOBE/u:'+$setting, $ns).InnerText -ceq 'true') 'Offline OOBE settings are missing.'
}
Assert-Check ($answer.SelectNodes('//u:DiskConfiguration | //u:ImageInstall | //u:HideEULAPage | //u:AcceptEula | //u:ProductKey | //u:AutoLogon | //u:UILanguage | //u:SkipMachineOOBE', $ns).Count -eq 0) 'Manual ISO choices or administrator sign-in were overridden.'
foreach ($name in @('Administrator', 'cagesetup', 'host\user', 'user@example.com', 'abcdefghijklmnopqrstu')) {
    try {
        $null = New-CageISOAnswer ([pscredential]::new($name, $credential.Password))
        throw 'Invalid account accepted'
    } catch { if ($_.Exception.Message -eq 'Invalid account accepted') { throw } }
}

# Execute the production branch which chooses the answer before creating a disk.
$errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'new-windows-test-vm.ps1'), [ref]$null, [ref]$errors)
Assert-Check ($errors.Count -eq 0) 'VM creation script failed to parse.'
$selection = @($ast.EndBlock.Statements | Where-Object { $_.Extent.Text.StartsWith('if ($fromHost) {') })[0]
Assert-Check ($null -ne $selection) 'Missing production answer selection.'
$createAnswer = [scriptblock]::Create($selection.Extent.Text)
$script:prompts = 0
function Get-Credential { param($UserName, $Message); $script:prompts++; return $credential }
$fromHost = $false
$GuestAdministrator = $null
. $createAnswer
Assert-Check ($script:prompts -eq 1 -and $answer.SelectNodes('//u:LocalAccount', $ns).Count -eq 1) 'The manual route did not collect an offline account.'
. $createAnswer
Assert-Check ($script:prompts -eq 1) 'An explicit credential prompted again.'
$fromHost = $true
$GuestAdministrator = [pscredential]::new('cagesetup', $credential.Password)
$ImageLanguage = 'zh-CN'
. $createAnswer
Assert-Check ($answer.SelectSingleNode('//u:LocalAccount/u:Name', $ns).InnerText -ceq 'cagesetup') 'Clone setup account changed.'
Assert-Check ($answer.SelectSingleNode('//u:UILanguage', $ns).InnerText -ceq 'zh-CN') 'Clone locale changed.'

# The owner's progress reader accepts only known stages and reads the final
# stage after process exit. Arbitrary child output never becomes UI content.
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'setup-windows-guest.ps1'), [ref]$null, [ref]$errors)
Assert-Check ($errors.Count -eq 0) 'Setup orchestration failed to parse.'
$wait = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Wait-CageWindowsInstaller' }, $true)
. ([scriptblock]::Create($wait.Extent.Text))
$progressFile = [IO.Path]::GetTempFileName()
$heldProgress = [IO.File]::Open($progressFile, 'Open', 'ReadWrite', 'ReadWrite')
$writer = [IO.File]::Open($progressFile, 'Open', 'Write', 'ReadWrite')
try {
    $process = [pscustomobject]@{ Position = 0; Writer = $writer; Stages = @('CAGE_GUEST_STAGE windows-11 account', 'private diagnostic fixture', 'CAGE_GUEST_STAGE windows-11 media', 'CAGE_GUEST_STAGE windows-11 tools', 'CAGE_GUEST_STAGE windows-11 services', 'CAGE_GUEST_STAGE windows-11 oobe') }
    $process | Add-Member -MemberType ScriptMethod -Name WaitForExit -Value {
        param($Timeout)
        $bytes = [Text.Encoding]::UTF8.GetBytes($this.Stages[$this.Position])
        $this.Writer.Position = 0
        $this.Writer.SetLength(0)
        $this.Writer.Write($bytes, 0, $bytes.Length)
        $this.Writer.Flush()
        $this.Position++
        return $this.Position -eq $this.Stages.Count
    }
    $stages = @(Wait-CageWindowsInstaller $process $progressFile)
    Assert-Check ($stages.Count -eq 5 -and $stages[-1] -ceq 'CAGE_GUEST_STAGE windows-11 oobe') 'Final progress or stage filtering failed.'
    $writer.Dispose()

    # Execute the exact encoded wrapper in a real child PowerShell process.
    # UAC itself is covered by native acceptance, while file sharing is exercised here.
    $wrapper = $ast.Find({ param($node) $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq '$command' -and $node.Right.Extent.Text.StartsWith("@'") }, $true)
    $literal = $wrapper.Right.Find({ param($node) $node -is [Management.Automation.Language.StringConstantExpressionAst] }, $true)
    $command = $literal.Value
    $fixtureScript = $progressFile+'.ps1'
    $stdout = $progressFile+'.stdout'
    $stderr = $progressFile+'.stderr'
    try {
        [IO.File]::WriteAllText($fixtureScript, "Write-Output 'CAGE_GUEST_STAGE windows-11 account'`nWrite-Output 'private diagnostic fixture'`nWrite-Output 'CAGE_GUEST_STAGE windows-11 oobe'")
        $payload = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes((@{ProgressFile=$progressFile}|ConvertTo-Json -Compress)))
        $scriptPath = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($fixtureScript))
        $command = $command.Replace('__PARAMETERS__', $payload).Replace('__SCRIPT__', $scriptPath)
        $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
        $child = Start-Process -FilePath (Get-Process -Id $PID).Path -ArgumentList @('-NoLogo','-NoProfile','-NonInteractive','-EncodedCommand',$encoded) -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
        try {
            $null = $child.Handle
            $forwarded = @(Wait-CageWindowsInstaller $child $progressFile)
            Assert-Check ($child.ExitCode -eq 0) ('Encoded installer wrapper failed. '+[IO.File]::ReadAllText($stderr))
            Assert-Check ($forwarded[-1] -ceq 'CAGE_GUEST_STAGE windows-11 oobe') 'The actual child could not publish its final stage.'
        } finally { $child.Dispose() }
    } finally {
        foreach ($path in @($fixtureScript,$stdout,$stderr)) { [IO.File]::Delete($path) }
    }
} finally { $writer.Dispose(); $heldProgress.Dispose(); [IO.File]::Delete($progressFile) }

# An interrupted or unauthenticated guest retains its answer media. Successful
# cleanup only removes the fixed tools ISO and preserves the original disk.
$directory = Join-Path ([IO.Path]::GetTempPath()) ('cage-iso-answer-'+[guid]::NewGuid().ToString('N'))
$null = [IO.Directory]::CreateDirectory($directory)
try {
    $iso = Join-Path $directory 'windows-guest-tools.iso'
    $disk = Join-Path $directory 'system.vhdx'
    [IO.File]::WriteAllText($iso, 'private answer fixture')
    [IO.File]::WriteAllText($disk, 'existing personal disk fixture')
    $vm = [pscustomobject]@{ Id = [guid]::NewGuid() }
    $script:guestReady = $false
    $script:removedDVDs = @()
    function Invoke-Command { param($Session, $ArgumentList, $ScriptBlock); if (!$script:guestReady) { throw 'Guest setup incomplete' } }
    function Hyper-V\Get-VMDvdDrive { process { @([pscustomobject]@{Path=$iso}, [pscustomobject]@{Path='original-windows.iso'}) } }
    function Hyper-V\Remove-VMDvdDrive { process { $script:removedDVDs += $_.Path } }
    try { Remove-CageISOAnswer $vm 'session' $directory; throw 'Incomplete setup was accepted' }
    catch { if ($_.Exception.Message -cne 'Guest setup incomplete') { throw } }
    Assert-Check ((Test-Path -LiteralPath $iso) -and $script:removedDVDs.Count -eq 0) 'Unfinished setup lost its answer media.'
    $script:guestReady = $true
    Remove-CageISOAnswer $vm 'session' $directory
    Assert-Check (!(Test-Path -LiteralPath $iso) -and $script:removedDVDs.Count -eq 1 -and $script:removedDVDs[0] -eq $iso) 'Cleanup touched the wrong DVD.'
    Assert-Check ([IO.File]::ReadAllText($disk) -ceq 'existing personal disk fixture') 'Cleanup changed the retained disk.'
} finally { [IO.Directory]::Delete($directory, $true) }
Write-Output 'Passed manual ISO account/OOBE XML, retained manual choices, clone parity and elevated installer progress forwarding.'
