#requires -Version 5.1
<#
.SYNOPSIS
Inspect, install or launch the official x64 Claude Desktop MSIX as the current user.
.DESCRIPTION
Run with 64-bit Windows PowerShell inside the target VM. For Install and Launch,
pass the Id obtained independently from Get-VM on the Hyper-V host.
The full MSIX includes a service. Register it in an elevated session for the
target guest user, or provision it in the guest image before that user signs in.
Launch in the user's normal interactive session.
Download the local MSIX from https://support.claude.com/en/articles/12622703 .
#>
[CmdletBinding()]
param(
    [ValidateSet('Inspect', 'Install', 'Launch')][string]$Action = 'Inspect',
    [Guid]$ExpectedVMId = [Guid]::Empty,
    [string]$MsixPath
)
$ErrorActionPreference = 'Stop'

# Verified against AppxManifest.xml and AppxSignature.p7x in the official
# Windows x64 4.23118.0.0 MSIX. The package also contains helper applications.
$publisher = 'CN="Anthropic, PBC", O="Anthropic, PBC", L=San Francisco, S=California, C=US, SERIALNUMBER=4860621, OID.2.5.4.15=Private Organization, OID.1.3.6.1.4.1.311.60.2.1.2=Delaware, OID.1.3.6.1.4.1.311.60.2.1.3=US'
$packageFamily = 'Claude_pzs8sxrjxfjjc'

function Assert-TargetGuest([Guid]$expected) {
    if ($expected -eq [Guid]::Empty) {
        throw 'Pass -ExpectedVMId with the target VM Id from Get-VM on the Hyper-V host.'
    }
    $system = Get-CimInstance Win32_ComputerSystem
    if ($system.Manufacturer -ne 'Microsoft Corporation' -or $system.Model -ne 'Virtual Machine') {
        throw 'This operation requires a Hyper-V guest.'
    }
    $kvp = Get-ItemProperty -LiteralPath 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters' -Name VirtualMachineId
    $actual = [Guid]::Empty
    if (![Guid]::TryParse($kvp.VirtualMachineId, [ref]$actual) -or $expected -ne $actual) {
        throw 'The Hyper-V guest VM Id does not match the host-provided target.'
    }
}

function Assert-ClaudeManifest([xml]$manifest) {
    $ns = [Xml.XmlNamespaceManager]::new($manifest.NameTable)
    $ns.AddNamespace('m', 'http://schemas.microsoft.com/appx/manifest/foundation/windows10')
    $identity = $manifest.SelectSingleNode('/m:Package/m:Identity', $ns)
    $app = $manifest.SelectSingleNode('/m:Package/m:Applications/m:Application[@Id="Claude"]', $ns)
    if (!$identity -or $identity.GetAttribute('Name') -cne 'Claude' -or
        $identity.GetAttribute('Publisher') -cne $publisher -or
        $identity.GetAttribute('ProcessorArchitecture') -ne 'x64' -or !$app -or
        $app.GetAttribute('Executable') -ine 'app\Claude.exe' -or
        $app.GetAttribute('EntryPoint') -cne 'Windows.FullTrustApplication') {
        throw 'Unexpected Claude x64 package identity or application entry point.'
    }
    return [version]$identity.GetAttribute('Version')
}

function Assert-ClaudeSignature($signature) {
    if ($signature.Status -ne 'Valid' -or !$signature.SignerCertificate) {
        throw 'Windows did not validate the MSIX signature.'
    }
    # Decode both subjects with the same Windows formatter, including the
    # organization registration number. A matching common name alone is insufficient.
    $expected = [Security.Cryptography.X509Certificates.X500DistinguishedName]::new($publisher)
    $format = [Security.Cryptography.X509Certificates.X500DistinguishedNameFlags]::Reversed
    if ($signature.SignerCertificate.SubjectName.Decode($format) -cne $expected.Decode($format)) {
        throw 'The trusted signing certificate does not match the Anthropic publisher.'
    }
}

function Get-RegisteredClaude([string]$UserSID = '') {
    $options=@{Name='Claude'}
    if ($UserSID) { $options.User=$UserSID }
    $packages = @(Get-AppxPackage @options | Where-Object PackageFamilyName -ceq $packageFamily)
    if ($packages.Count -eq 0) { return }
    if ($packages.Count -ne 1) { throw 'Multiple registrations found for the Claude package family.' }
    $pkg = $packages[0]
    if ($pkg.Publisher -cne $publisher -or $pkg.IsDevelopmentMode -or $pkg.IsPartiallyStaged -or
        $pkg.SignatureKind -notin 'Developer','Store','Enterprise' -or $pkg.Status -ne 'Ok') {
        throw 'The registered Claude package is unsigned, unhealthy or has an unexpected publisher.'
    }
    $manifestOptions=@{Package=$pkg.PackageFullName}
    if ($UserSID) { $manifestOptions.User=$UserSID }
    $version = Assert-ClaudeManifest (Get-AppxPackageManifest @manifestOptions)
    return [pscustomobject]@{
        Registered = $true
        Version = $version.ToString()
        Aumid = "$packageFamily!Claude"
        Executable = Join-Path $pkg.InstallLocation 'app\Claude.exe'
    }
}

function Get-VerifiedClaudeMsixVersion([IO.FileInfo]$file, [IO.Stream]$handle) {
    Assert-ClaudeSignature (Get-AuthenticodeSignature -LiteralPath $file.FullName)
    Add-Type -AssemblyName System.IO.Compression
    $zip = [IO.Compression.ZipArchive]::new($handle, 'Read', $true)
    try {
        $entry = $zip.GetEntry('AppxManifest.xml')
        if (!$entry) { throw 'The MSIX is missing AppxManifest.xml.' }
        $stream = $entry.Open()
        try {
            $manifest = [xml]::new()
            $manifest.XmlResolver = $null
            $manifest.Load($stream)
            return (Assert-ClaudeManifest $manifest)
        } finally { $stream.Dispose() }
    } finally { $zip.Dispose() }
}

function Assert-ClaudeDesktopRelease([string]$Path, [string]$ExpectedVersion) {
    $handle = [IO.File]::Open($Path, 'Open', 'Read', 'Read')
    try {
        $version = Get-VerifiedClaudeMsixVersion (Get-Item -LiteralPath $Path) $handle
        if ($version -ne [version]($ExpectedVersion+'.0')) { throw 'The signed Claude Desktop package differs from the selected release.' }
    } finally { $handle.Dispose() }
}

function Install-ProvisionedClaude([string]$Path, [string]$UserSID = '') {
    $handle = [IO.File]::Open($Path, 'Open', 'Read', 'Read')
    try {
        $version = Get-VerifiedClaudeMsixVersion (Get-Item -LiteralPath $Path) $handle
        if ($UserSID) {
            $registered=Get-RegisteredClaude $UserSID
            if ($registered -and [version]$registered.Version -ge $version) { return }
        }
        $installed = @(Get-AppxProvisionedPackage -Online | Where-Object DisplayName -ceq 'Claude')
        if ($installed.Count -gt 1 -or ($installed.Count -eq 1 -and $installed[0].PackageName -notmatch '^Claude_[0-9.]+_x64__pzs8sxrjxfjjc$')) {
            throw 'An unexpected existing Claude registration was preserved.'
        }
        if ($installed.Count -and [version]($installed[0].PackageName.Split('_')[1]) -ge $version) { return }
        $null = Add-AppxProvisionedPackage -Online -PackagePath $Path -SkipLicense -Regions 'all'
        $installed = @(Get-AppxProvisionedPackage -Online | Where-Object DisplayName -ceq 'Claude')
        if ($installed.Count -ne 1 -or $installed[0].PackageName -notmatch '^Claude_[0-9.]+_x64__pzs8sxrjxfjjc$' -or
            [version]($installed[0].PackageName.Split('_')[1]) -lt $version) { throw 'Windows did not provision the selected Claude Desktop version.' }
    } finally { $handle.Dispose() }
}

function Install-ClaudeMsix([string]$path) {
    if (!$path) { throw 'Install requires -MsixPath with a locally downloaded x64 MSIX.' }
    $file = Get-Item -LiteralPath $path
    if ($file -isnot [IO.FileInfo] -or $file.Extension -ne '.msix' -or
        $file.FullName.StartsWith('\\') -or
        [IO.DriveInfo]::new($file.Directory.Root.FullName).DriveType -eq 'Network') {
        throw 'Select a local .msix file on the guest.'
    }
    # Keep the file read-locked while Windows checks and registers the package.
    $handle = [IO.File]::Open($file.FullName, 'Open', 'Read', 'Read')
    try {
        $version = Get-VerifiedClaudeMsixVersion $file $handle
        $registered = Get-RegisteredClaude
        if ($registered -and [version]$registered.Version -ge $version) { return $registered }
        try { Add-AppxPackage -Path $file.FullName } catch {
            if ($_.Exception.HResult.ToString('X8') -eq '80073D28' -or $_.ToString() -match '\b0x80073D28\b') {
                throw '0x80073D28: The Claude MSIX service requires guest administrator rights. Register it elevated as this guest user, or provision it in the guest image for standard users.'
            }
            throw
        }
        $registered = Get-RegisteredClaude
        if (!$registered -or [version]$registered.Version -lt $version) {
            throw 'Windows did not register the expected version for this user.'
        }
        return $registered
    } finally { $handle.Dispose() }
}

function Open-ClaudeWindow($registered) {
    if (!$registered) { throw 'Claude Desktop is not registered for this user.' }
    $session = [Diagnostics.Process]::GetCurrentProcess().SessionId
    $shell = Get-Process -Name explorer -ErrorAction SilentlyContinue | Where-Object SessionId -eq $session
    if ($session -eq 0 -or !$shell -or ![Environment]::UserInteractive) {
        throw 'Launch requires a signed-in desktop session. Open PowerShell in the guest console.'
    }
    $explorer = Join-Path ([Environment]::GetFolderPath('Windows')) 'explorer.exe'
    Start-Process -FilePath $explorer -ArgumentList ('shell:AppsFolder\' + $registered.Aumid)
    $timer = [Diagnostics.Stopwatch]::StartNew()
    do {
        $window = Get-Process -Name Claude -ErrorAction SilentlyContinue | Where-Object {
            $_.SessionId -eq $session -and $_.MainWindowHandle -ne 0 -and $_.Path -eq $registered.Executable
        } | Select-Object -First 1
        if ($window) {
            return [pscustomobject]@{ Aumid = $registered.Aumid; WindowProcessId = $window.Id }
        }
        Start-Sleep -Milliseconds 400
    } while ($timer.Elapsed.TotalSeconds -lt 30)
    throw 'Activation was requested, but no window from the registered Claude executable appeared within 30 seconds.'
}

if ($MyInvocation.InvocationName -eq '.') { return } # Allow isolated validation tests to load the checks.
if ([Environment]::OSVersion.Platform -ne 'Win32NT' -or ![Environment]::Is64BitProcess) {
    throw 'Run this script in 64-bit Windows PowerShell.'
}
if ($Action -ne 'Install' -and $MsixPath) { throw 'Only Install accepts -MsixPath.' }
if ($Action -ne 'Inspect') { Assert-TargetGuest $ExpectedVMId }
switch ($Action) {
    'Inspect' {
        $registered = Get-RegisteredClaude
        if (!$registered) { $registered = [pscustomobject]@{ Registered = $false } }
        $registered | ConvertTo-Json
    }
    'Install' { Install-ClaudeMsix $MsixPath | ConvertTo-Json }
    'Launch' { Open-ClaudeWindow (Get-RegisteredClaude) | ConvertTo-Json }
}
