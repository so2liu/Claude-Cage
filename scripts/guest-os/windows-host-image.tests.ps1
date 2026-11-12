#requires -Version 5.1
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot 'windows-host-image.ps1')
$credential=[pscredential]::new('cagesetup',(ConvertTo-SecureString 'Test-only_Account!123' -AsPlainText -Force))
$answer=New-CageCloneAnswer $credential 'zh-CN'
$ns=[Xml.XmlNamespaceManager]::new($answer.NameTable)
$ns.AddNamespace('u','urn:schemas-microsoft-com:unattend')
if($answer.SelectSingleNode('//u:LocalAccount/u:Name',$ns).InnerText -cne 'cagesetup'){throw 'Wrong setup account.'}
if($answer.SelectSingleNode('//u:LocalAccount/u:Password/u:Value',$ns).InnerText -cne $credential.GetNetworkCredential().Password){throw 'Setup password changed in XML.'}
if($answer.SelectSingleNode('//u:UILanguage',$ns).InnerText -cne 'zh-CN'){throw 'Image language was not preserved.'}
if($answer.SelectNodes('//u:ProductKey | //u:AutoLogon',$ns).Count){throw 'Unexpected host key or automatic administrator logon.'}
function Get-WindowsImage {
    param($ImagePath,$Index)
    if(!$Index){return [pscustomobject]@{ImageIndex=1}}
    return [pscustomobject]@{ImageIndex=1;EditionId='Professional';Architecture='amd64';Languages=@('zh-CN');Version='10.0.26200.6584'}
}
$script:mounted=$false
function Mount-VHD { param($Path,[switch]$Passthru); $script:mounted=$true; throw 'ReachedDiskPreparation' }
try { Install-CageWindowsImage 'test.esd' 'test.vhdx' 'Professional' 'zh-CN' $answer '26200.9999'; throw 'Wrong build accepted.' }
catch { if($_.Exception.Message -notlike '*build differs*'){throw} }
if($script:mounted){throw 'An unmatched image touched a disk.'}
try { Install-CageWindowsImage 'test.esd' 'test.vhdx' 'Professional' 'zh-CN' $answer '26200.6584'; throw 'Expected disk preparation.' }
catch { if($_.Exception.Message -cne 'ReachedDiskPreparation'){throw} }
if(!$script:mounted){throw 'Matching image did not reach disk preparation.'}
$profile = Join-Path ([IO.Path]::GetTempPath()) ('cage-setup-credential-'+[guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $profile
try {
    $installation = [guid]::NewGuid()
    $receipt = Join-Path $profile 'windows-install-vm.txt'
    [IO.File]::WriteAllText($receipt, [guid]::NewGuid().ToString('D'))
    try { $null = Get-CageSetupCredential $profile $installation; throw 'A retained guest received a new setup password.' }
    catch { if ($_.Exception.Message -notlike '*saved setup credential*missing*') { throw } }
    if (Test-Path -LiteralPath (Join-Path $profile ('windows-setup-'+$installation.ToString('N')+'.dpapi'))) { throw 'Created a replacement setup credential.' }
    [IO.File]::Delete($receipt)
    $credentialPath = Join-Path $profile ('windows-setup-'+$installation.ToString('N')+'.dpapi')
    [IO.File]::WriteAllText($credentialPath, 'encrypted-test-fixture')
    $script:disabled = $false
    $script:sessionAttempts = 0
    function Test-CageSetupAccountDisabled { param($VMId,$Profile); return $script:disabled }
    function New-PSSession {
        param($VMId,$Credential)
        $script:sessionAttempts++
        throw 'InjectedCleanupConnectionFailure'
    }
    try { Remove-CageSetupCredential ([guid]::NewGuid()) $credential $profile $installation; throw 'Cleanup failure was ignored.' }
    catch { if ($_.Exception.Message -cne 'InjectedCleanupConnectionFailure') { throw } }
    if (!(Test-Path -LiteralPath $credentialPath)) { throw 'Failed cleanup discarded the original credential.' }
    $script:disabled = $true
    Remove-CageSetupCredential ([guid]::NewGuid()) $credential $profile $installation
    if ($script:sessionAttempts -ne 1 -or (Test-Path -LiteralPath $credentialPath)) { throw 'Retired setup account could not complete cleanup without logging in again.' }
    $pair = Join-Path (Join-Path (Join-Path $profile 'state') ('windows-'+$installation.ToString('N'))) 'cage-workbench/pair'
    $null = [IO.Directory]::CreateDirectory($pair)
    [IO.File]::WriteAllText((Join-Path $pair 'workbench-components-ready'), 'ready')
    [IO.File]::WriteAllText((Join-Path $pair 'windows-guest.json'), '{}')
    [IO.File]::WriteAllText($receipt, [guid]::NewGuid().ToString('D'))
    if ($null -ne (Get-CageSetupCredential $profile $installation) -or (Test-Path -LiteralPath $credentialPath)) {
        throw 'Retirement followed by an interrupted publish regenerated a credential.'
    }
} finally { [IO.Directory]::Delete($profile, $true) }

$tokens = $null; $parseErrors = $null
$completion = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'complete-windows-guest.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw 'The completion script has syntax errors.' }
$publisher = $completion.Find({param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -ceq 'Publish-ReadyBinding'}, $true)
# Publication needs a live relay and Windows ACLs. Keep the required ordering
# checked here; the whole native path remains part of Windows real testing.
$retireAt = $publisher.Body.Extent.Text.IndexOf('Complete-CloneSetup $stageBinding')
$publishAt = $publisher.Body.Extent.Text.IndexOf('Complete-CageHostEnrollment')
if ($retireAt -lt 0 -or $publishAt -le $retireAt) { throw 'Host enrollment can publish before clone retirement.' }
$retireAt = $publisher.Body.Extent.Text.LastIndexOf('Complete-CloneSetup $stageBinding')
$publishAt = $publisher.Body.Extent.Text.IndexOf('[IO.File]::Copy($stageBinding')
if ($retireAt -lt 0 -or $publishAt -le $retireAt) { throw 'The local host can publish before clone retirement.' }

$manifestPublisher = Join-Path $PSScriptRoot 'publish-windows-component-manifest.ps1'
$manifestBytes = [IO.File]::ReadAllBytes($manifestPublisher)
if ($manifestBytes.Length -lt 3 -or $manifestBytes[0] -ne 0xEF -or $manifestBytes[1] -ne 0xBB -or $manifestBytes[2] -ne 0xBF) {
    throw 'The component manifest publisher must start with a UTF-8 BOM for Windows PowerShell 5.1.'
}
$parseErrors = $null
$null = [Management.Automation.Language.Parser]::ParseFile($manifestPublisher, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw "The component manifest publisher failed Windows PowerShell parsing: $($parseErrors[0].Message)" }
Write-Output 'Passed isolated account XML, locale preservation, build validation, retained credential identity and interrupted retirement recovery.'
