# Shared verified package preparation for initial installation and maintenance.
function Invoke-WorkbenchDownload([string]$Address, [string]$Target, [string]$Name, [string]$Cache, [string]$Digest = '') {
    $downloader = Join-Path (Split-Path $PSScriptRoot -Parent) 'cage.exe'
    if (Test-Path -LiteralPath $downloader -PathType Leaf) {
        $arguments = @('host-download', $Address, $Target, $Name)
        $verification = @()
        if ($Digest) { $verification = @((Join-Path $Cache '.downloads'), $Digest) }
        # An array-splatted wildcard expands on Unix PowerShell. Keep Accept
        # quoted at the native call boundary, including metadata downloads.
        & $downloader @arguments '*/*' @verification | ForEach-Object { [Console]::WriteLine($_) }
        if ($LASTEXITCODE -ne 0) { throw ('Package download failed: '+$Name) }
    } else {
        Invoke-WebRequest -UseBasicParsing -Uri $Address -OutFile $Target
    }
}

function Read-ClaudeReleaseMetadata([string]$Address, [string]$Cache) {
    $temporary = Join-Path $Cache ([guid]::NewGuid().ToString('N')+'.metadata')
    try {
        Invoke-WorkbenchDownload $Address $temporary 'Claude release metadata' $Cache
        if ((Get-Item -LiteralPath $temporary).Length -gt 1048576) { throw 'Claude release metadata is too large.' }
        return [IO.File]::ReadAllText($temporary).Trim()
    } finally { if ([IO.File]::Exists($temporary)) { [IO.File]::Delete($temporary) } }
}

function Resolve-ClaudePackages($Packages, $CLI, [string]$Cache) {
    # Resolve once per customer operation. Cached artifacts remain bound to the
    # selected release; a cache from an earlier install never selects a version.
    $verifier = Join-Path $PSScriptRoot 'payload/claude-desktop-windows.ps1'
    if (!(Test-Path -LiteralPath $verifier)) { $verifier = Join-Path $PSScriptRoot 'claude-desktop-windows.ps1' }
    . $verifier
    $release = 'https://downloads.claude.ai/claude-code-releases'
    $version = Read-ClaudeReleaseMetadata "$release/latest" $Cache
    if ($version -notmatch '^\d+\.\d+\.\d+$') { throw 'Invalid official Claude Code version.' }
    $manifest = (Read-ClaudeReleaseMetadata "$release/$version/manifest.json" $Cache) | ConvertFrom-Json
    $digest = $manifest.platforms.'win32-x64'.checksum
    if ($manifest.version -cne $version -or $manifest.platforms.'win32-x64'.binary -cne 'claude.exe' -or $digest -notmatch '^[a-fA-F0-9]{64}$') { throw 'The official Claude Code manifest does not match Windows x64 and the selected release.' }
    $CLI.claude = [pscustomobject]@{version=$version;filename="claude-$version-win32-x64.exe";url="$release/$version/win32-x64/claude.exe";sha256=$digest.ToLowerInvariant()}
    $desktop = (Read-ClaudeReleaseMetadata 'https://api.anthropic.com/api/desktop/win32/x64/msix/latest' $Cache) | ConvertFrom-Json
    if ($desktop.version -notmatch '^\d+\.\d+\.\d+$' -or
        $desktop.url -notmatch ('^https://downloads\.claude\.ai/releases/win32/x64/'+[regex]::Escape($desktop.version)+'/Claude-[a-f0-9]+\.msix$')) {
        throw 'Invalid official Claude Desktop release.'
    }
    $spec = [pscustomobject]@{version=$desktop.version;filename=([uri]$desktop.url).Segments[-1];url=$desktop.url;sha256=''}
    # Desktop metadata has no digest. Authenticate the complete signed MSIX
    # before deriving the digest used by the protected guest transfer.
    $file = Get-PinnedPackage $spec $Cache -ClaudeDesktop
    $spec.sha256 = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
    $Packages.desktop = $spec
}

function Get-PinnedPackage($Spec, [string]$Cache, [switch]$ClaudeDesktop) {
    if (!$ClaudeDesktop -and $Spec.sha256 -notmatch '^[a-fA-F0-9]{64}$') { throw 'A package has no verified checksum.' }
    $file = Join-Path $Cache $Spec.filename
    $cached=Test-Path -LiteralPath $file
    if ($cached) {
        $item=Get-Item -LiteralPath $file -Force
        if ($item -isnot [IO.FileInfo] -or $item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'The download cache contains an invalid file.' }
        if ($ClaudeDesktop) {
            try { Assert-ClaudeDesktopRelease $file $Spec.version; return $file } catch { }
        }
        if ((Get-FileHash -LiteralPath $file).Hash -eq $Spec.sha256) { return $file }
        # Keep the old cache entry until a complete replacement has passed verification.
    }
    $tmp = $file+'.'+[guid]::NewGuid()+'.download'
    try {
        Invoke-WorkbenchDownload $Spec.url $tmp $Spec.filename $Cache $Spec.sha256
        if ($ClaudeDesktop) { Assert-ClaudeDesktopRelease $tmp $Spec.version }
        elseif ((Get-FileHash -LiteralPath $tmp).Hash -ne $Spec.sha256) { throw ('Package verification failed: '+$Spec.filename) }
        if ($cached) { [IO.File]::Replace($tmp,$file,[Management.Automation.Language.NullString]::Value) }
        else { [IO.File]::Move($tmp,$file) }
    } finally { if ([IO.File]::Exists($tmp)) { [IO.File]::Delete($tmp) } }
    return $file
}

function Expand-PinnedTar([string]$File,[string]$Destination) {
    $null=New-Item -ItemType Directory -Path $Destination -Force
    & (Join-Path $env:SystemRoot 'System32/tar.exe') -xf $File -C $Destination --strip-components 1
    if ($LASTEXITCODE) { throw ('Package extraction failed: '+[IO.Path]::GetFileName($File)) }
}
