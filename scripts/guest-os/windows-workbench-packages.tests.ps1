# Customer-time release resolution and cache validation. Network and Windows
# certificate APIs are substitutes; the production resolver and MSIX parser run.
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
Add-Type -AssemblyName System.IO.Compression
. (Join-Path $PSScriptRoot 'claude-desktop-windows.ps1')
. (Join-Path $PSScriptRoot 'windows-workbench-packages.ps1')
$root=Join-Path ([IO.Path]::GetTempPath()) ('claude-release-test-'+[guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $root
$script:release='2.1.999'
$script:desktopRelease='5.1231.2'
$script:badChecksum=$false
$script:badSignature=$false
$script:badURL=$false
$script:badManifest=$false
$script:metadataReads=0
$script:desktopReads=0
$code=[Text.Encoding]::UTF8.GetBytes('verified Claude Code release')
$sha=[Security.Cryptography.SHA256]::Create()
try { $script:codeDigest=([BitConverter]::ToString($sha.ComputeHash($code))).Replace('-','').ToLowerInvariant() } finally { $sha.Dispose() }
$script:expectedSubject=[Security.Cryptography.X509Certificates.X500DistinguishedName]::new($publisher)
function Get-AuthenticodeSignature {
    param($LiteralPath)
    [pscustomobject]@{Status=$(if ($script:badSignature) {'NotSigned'} else {'Valid'});SignerCertificate=@{SubjectName=$script:expectedSubject}}
}
function New-ReleaseMsix([string]$Path) {
    $manifest=[xml]'<Package xmlns="http://schemas.microsoft.com/appx/manifest/foundation/windows10"><Identity Name="Claude" ProcessorArchitecture="x64"/><Applications><Application Id="Claude" Executable="app\Claude.exe" EntryPoint="Windows.FullTrustApplication"/></Applications></Package>'
    $manifest.Package.Identity.SetAttribute('Publisher',$publisher)
    $manifest.Package.Identity.SetAttribute('Version',$script:desktopRelease+'.0')
    $file=[IO.File]::Open($Path,'Create','Write','None')
    try {
        $archive=[IO.Compression.ZipArchive]::new($file,'Create',$true)
        try {
            $stream=$archive.CreateEntry('AppxManifest.xml').Open()
            try { $manifest.Save($stream) } finally { $stream.Dispose() }
        } finally { $archive.Dispose() }
    } finally { $file.Dispose() }
}
function Invoke-WorkbenchDownload {
    param($Address,$Target,$Name,$Cache,$Digest='')
    switch -Regex ($Address) {
        '/claude-code-releases/latest$' { $script:metadataReads++; [IO.File]::WriteAllText($Target,$script:release); return }
        '/manifest.json$' {
            $script:metadataReads++
            $digest=if ($script:badChecksum) {'0'*64} else {$script:codeDigest}
            $selected=if ($script:badManifest) {'0.0.0'} else {$script:release}
            [IO.File]::WriteAllText($Target,(@{version=$selected;platforms=@{'win32-x64'=@{checksum=$digest;binary='claude.exe'}}}|ConvertTo-Json -Depth 4)); return
        }
        '/api/desktop/win32/x64/msix/latest$' {
            $script:metadataReads++
            $hostName=if ($script:badURL) {'untrusted.example'} else {'downloads.claude.ai'}
            [IO.File]::WriteAllText($Target,(@{version=$script:desktopRelease;url="https://$hostName/releases/win32/x64/$script:desktopRelease/Claude-$($script:desktopRelease.Replace('.','')).msix"}|ConvertTo-Json)); return
        }
        '/claude.exe$' { [IO.File]::WriteAllBytes($Target,$code); return }
        '\.msix$' { $script:desktopReads++; New-ReleaseMsix $Target; return }
        default { throw "Unexpected production download: $Address" }
    }
}
function Expect-ReleaseFailure([scriptblock]$Action) {
    $failed=$false
    try { & $Action } catch { $failed=$true }
    if (!$failed) { throw 'Invalid release was accepted.' }
}
try {
    $packages=Get-Content -LiteralPath (Join-Path $PSScriptRoot 'windows-workbench-packages.json') -Raw | ConvertFrom-Json
    $cli=Get-Content -LiteralPath (Join-Path $PSScriptRoot 'guest-clipboard-windows/cli-dependencies.json') -Raw | ConvertFrom-Json
    $python=$cli.python|ConvertTo-Json -Compress
    Resolve-ClaudePackages $packages $cli $root
    if ($cli.claude.version -ne '2.1.999' -or $packages.desktop.version -ne '5.1231.2') { throw 'Customer operation did not select the official latest releases.' }
    $msix=Join-Path $root $packages.desktop.filename
    if ($packages.desktop.sha256 -ne (Get-FileHash $msix).Hash.ToLowerInvariant()) { throw 'Verified Desktop transport digest is missing.' }
    $null=Get-PinnedPackage $cli.claude $root
    $script:release='2.2.1'
    $script:desktopRelease='5.1232.0'
    Resolve-ClaudePackages $packages $cli $root
    if ($cli.claude.version -ne '2.2.1' -or $packages.desktop.version -ne '5.1232.0' -or $metadataReads -ne 6 -or $desktopReads -ne 2) { throw 'A cached previous release selected the next customer operation.' }
    Resolve-ClaudePackages $packages $cli $root
    if ($metadataReads -ne 9 -or $desktopReads -ne 2) { throw 'Release metadata was cached or a verified release artifact was redownloaded.' }
    $null=Get-PinnedPackage $cli.claude $root
    if (($cli.python|ConvertTo-Json -Compress) -cne $python) { throw 'Resolving Claude changed the pinned Python dependency.' }
    $script:badURL=$true
    Expect-ReleaseFailure { Resolve-ClaudePackages $packages $cli $root }
    $script:badURL=$false
    $script:badManifest=$true
    Expect-ReleaseFailure { Resolve-ClaudePackages $packages $cli $root }
    $script:badManifest=$false
    $script:badSignature=$true
    Expect-ReleaseFailure { Resolve-ClaudePackages $packages $cli $root }
    $script:badSignature=$false
    $script:badChecksum=$true
    Resolve-ClaudePackages $packages $cli $root
    Expect-ReleaseFailure { Get-PinnedPackage $cli.claude $root }
    if ([IO.File]::ReadAllBytes((Join-Path $root $cli.claude.filename)).Length -ne $code.Length) { throw 'Failed verification removed the previous cached artifact.' }
    $script:release='<html>not a version</html>'
    Expect-ReleaseFailure { Resolve-ClaudePackages $packages $cli $root }
    Write-Output 'Passed customer-time latest resolution, signed Desktop identity, immutable artifact reuse, pinned dependency preservation and invalid metadata/signature/checksum rejection (simulated network and certificate APIs).'
} finally { Remove-Item -LiteralPath $root -Recurse -Force }
