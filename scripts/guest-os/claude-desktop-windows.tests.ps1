#requires -Version 5.1
# Run on any platform with PowerShell. The tests never call Windows deployment APIs.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'claude-desktop-windows.ps1')

function Expect-Failure([scriptblock]$action, [string]$reason) {
    try { & $action } catch {
        if ($_.Exception.Message.Contains($reason)) { return }
        throw
    }
    throw "Expected rejection: $reason"
}

$subject = [Security.Cryptography.X509Certificates.X500DistinguishedName]::new($publisher)
$signature = [pscustomobject]@{ Status = 'Valid'; SignerCertificate = @{ SubjectName = $subject } }
Assert-ClaudeSignature $signature
$signature.Status = 'HashMismatch'
Expect-Failure { Assert-ClaudeSignature $signature } 'did not validate'
$signature.Status = 'Valid'
$lookalike = $publisher.Replace('4860621', '1234567')
$signature.SignerCertificate.SubjectName = [Security.Cryptography.X509Certificates.X500DistinguishedName]::new($lookalike)
Expect-Failure { Assert-ClaudeSignature $signature } 'does not match'

$manifest = [xml]@'
<Package xmlns="http://schemas.microsoft.com/appx/manifest/foundation/windows10">
  <Identity Name="Claude" Version="4.23118.0.0" ProcessorArchitecture="x64"/>
  <Applications>
    <Application Id="SshProxy" Executable="app\resources\claude-ssh-proxy.exe"/>
    <Application Id="Claude" Executable="app\Claude.exe" EntryPoint="Windows.FullTrustApplication"/>
  </Applications>
</Package>
'@
$manifest.Package.Identity.SetAttribute('Publisher', $publisher)
if ((Assert-ClaudeManifest $manifest) -ne [version]'4.23118.0.0') { throw 'Version mismatch.' }
$wrongApp = $manifest.Clone()
$wrongApp.Package.Applications.Application[1].SetAttribute('Executable', 'app\helper.exe')
Expect-Failure { Assert-ClaudeManifest $wrongApp } 'entry point'

function Get-AppxPackage { $pkg }
function Get-AppxPackageManifest { $manifest }
$pkg = [pscustomobject]@{
    PackageFamilyName = 'OtherFamily'; IsDevelopmentMode = $false; IsPartiallyStaged = $false
    SignatureKind = 'Developer'; Status = 'Ok'; Publisher = $publisher
    PackageFullName = 'test-fixture'; InstallLocation = $PSScriptRoot
}
if (Get-RegisteredClaude) { throw 'An unrelated package was accepted.' }
$pkg.PackageFamilyName = $packageFamily
if ((Get-RegisteredClaude).Aumid -cne 'Claude_pzs8sxrjxfjjc!Claude') { throw 'Incorrect AUMID.' }
$pkg.IsDevelopmentMode = $true
Expect-Failure { Get-RegisteredClaude } 'registered Claude package'

# Execute the actual install path against a local archive. Mock just the
# operating-system signature and registration results, never deploy this fixture.
$fixture = Join-Path ([IO.Path]::GetTempPath()) (([Guid]::NewGuid().ToString()) + '.msix')
$installedCount = 0
function Get-AuthenticodeSignature { $signature }
function Add-AppxPackage {
    if ($deploymentError) { throw $deploymentError }
    $script:installedCount++
    $script:pkg.PackageFamilyName = $packageFamily
}
try {
    Add-Type -AssemblyName System.IO.Compression
    $handle = [IO.File]::Open($fixture, 'CreateNew', 'Write', 'None')
    try {
        $zip = [IO.Compression.ZipArchive]::new($handle, 'Create', $true)
        try {
            $stream = $zip.CreateEntry('AppxManifest.xml').Open()
            try { $manifest.Save($stream) } finally { $stream.Dispose() }
        } finally { $zip.Dispose() }
    } finally { $handle.Dispose() }
    $signature.Status = 'NotSigned'
    Expect-Failure { Install-ClaudeMsix $fixture } 'did not validate'
    $signature.Status = 'Valid'
    Expect-Failure { Install-ClaudeMsix $fixture } 'does not match'
    if ($installedCount -ne 0) { throw 'An untrusted package reached Add-AppxPackage.' }
    $signature.SignerCertificate.SubjectName = $subject
    $pkg.IsDevelopmentMode = $false
    $pkg.PackageFamilyName = 'absent'
    $deploymentError = 'Deployment failed: 0x80073D28'
    Expect-Failure { Install-ClaudeMsix $fixture } 'requires guest administrator rights'
    $deploymentError = 'Deployment failed: 0x80073CF6'
    Expect-Failure { Install-ClaudeMsix $fixture } '0x80073CF6'
    $deploymentError = $null
    $first = Install-ClaudeMsix $fixture
    $again = Install-ClaudeMsix $fixture
    if (!$first.Registered -or !$again.Registered -or $installedCount -ne 1) {
        throw 'Expected exactly one registration after two Install actions.'
    }
    Assert-ClaudeDesktopRelease $fixture '4.23118.0'
    Expect-Failure { Assert-ClaudeDesktopRelease $fixture '4.23119.0' } 'differs from the selected release'
    $script:provisioned=@()
    $script:provisionCount=0
    function Get-AppxProvisionedPackage { param([switch]$Online) $script:provisioned }
    function Add-AppxProvisionedPackage {
        param([switch]$Online,$PackagePath,[switch]$SkipLicense,$Regions)
        $script:provisionCount++
        $script:provisioned=@([pscustomobject]@{DisplayName='Claude';PackageName='Claude_4.23118.0.0_x64__pzs8sxrjxfjjc'})
    }
    Install-ProvisionedClaude $fixture
    Install-ProvisionedClaude $fixture
    if ($provisionCount -ne 1) { throw 'Provisioning did not preserve the same installed version.' }
    $script:provisioned=@([pscustomobject]@{DisplayName='Claude';PackageName='Claude_4.23119.0.0_x64__pzs8sxrjxfjjc'})
    Install-ProvisionedClaude $fixture
    if ($provisionCount -ne 1) { throw 'Provisioning downgraded a newer installed version.' }
    $script:provisioned=@([pscustomobject]@{DisplayName='Claude';PackageName='Claude_4.23117.0.0_x64__pzs8sxrjxfjjc'})
    Install-ProvisionedClaude $fixture
    if ($provisionCount -ne 2) { throw 'Provisioning did not upgrade the older installed version.' }
    $manifest.Package.Identity.SetAttribute('Version','4.23119.0.0')
    $script:provisioned=@()
    Install-ProvisionedClaude $fixture 'S-1-5-21-1-2-3-1001'
    if ($provisionCount -ne 2) { throw 'Provisioning replaced a newer version registered for the paired user.' }
    $manifest.Package.Identity.SetAttribute('Version','4.23118.0.0')
    $signature.Status='HashMismatch'
    Expect-Failure { Install-ProvisionedClaude $fixture } 'did not validate'
    if ($provisionCount -ne 2) { throw 'An untrusted MSIX reached provisioning.' }
    $signature.Status='Valid'
} finally { if ([IO.File]::Exists($fixture)) { [IO.File]::Delete($fixture) } }

$target = [Guid]::NewGuid()
$kvpId = $target.ToString()
$computer = @{ Manufacturer = 'Microsoft Corporation'; Model = 'Virtual Machine' }
function Get-CimInstance { $computer }
function Get-ItemProperty { @{ VirtualMachineId = $kvpId } }
Assert-TargetGuest $target
Expect-Failure { Assert-TargetGuest ([Guid]::NewGuid()) } 'does not match'
$computer.Model = 'Surface'
Expect-Failure { Assert-TargetGuest $target } 'requires a Hyper-V guest'
Write-Output 'Passed Claude manifest, signing, repeat install, registration and Hyper-V guest identity checks (simulated).'
