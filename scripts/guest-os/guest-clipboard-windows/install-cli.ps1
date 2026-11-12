# Run as the already paired ordinary GUI user. All packages are staged offline;
# this installer changes only Cage-owned tool files and the bridge receipt.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BundleDirectory,
    [Parameter(Mandatory)][string]$BridgeDirectory,
    [string]$ReceiptDirectory = (Join-Path $env:USERPROFILE 'Cage\CLI\install-receipts'),
    [switch]$WorkbenchSetup
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not [System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Windows)) { throw 'Native Windows is required.' }
$user = [Security.Principal.WindowsIdentity]::GetCurrent()
if ((New-Object Security.Principal.WindowsPrincipal($user)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Use the paired ordinary GUI user.' }
$manifest = Get-Content (Join-Path $BundleDirectory 'resolved-cli-dependencies.json') -Raw | ConvertFrom-Json
$bundle = [IO.Path]::GetFullPath($BundleDirectory)
$bridge = [IO.Path]::GetFullPath($BridgeDirectory)
if (-not (Test-Path (Join-Path $bridge 'Cage.GuestClipboard.exe') -PathType Leaf)) { throw 'The paired GUI bridge must be installed first.' }
function Assert-NoReparse([string]$Path) {
    $item = Get-Item -LiteralPath $Path -Force
    while ($null -ne $item) {
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'A Cage tool path is a reparse point.' }
        if ($item -is [IO.FileInfo]) { $item = $item.Directory } else { $item = $item.Parent }
    }
}
function Protect-Path([string]$Path, [bool]$Directory) {
    $ownerSection = [Security.AccessControl.AccessControlSections]::Owner
    $accessSection = [Security.AccessControl.AccessControlSections]::Access
    if ($Directory) {
        $existing = [Security.AccessControl.DirectorySecurity]::new($Path, $ownerSection)
        $acl = New-Object Security.AccessControl.DirectorySecurity
    } else {
        $existing = [Security.AccessControl.FileSecurity]::new($Path, $ownerSection)
        $acl = New-Object Security.AccessControl.FileSecurity
    }
    if ($existing.GetOwner([Security.Principal.SecurityIdentifier]) -ne $user.User) { throw 'The Cage tool path must already belong to the current GUI user.' }
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($sid in @($user.User, [Security.Principal.SecurityIdentifier]'S-1-5-18', [Security.Principal.SecurityIdentifier]'S-1-5-32-544')) {
        if ($Directory) { $rule = New-Object Security.AccessControl.FileSystemAccessRule($sid,'FullControl','ContainerInherit,ObjectInherit','None','Allow') }
        else { $rule = New-Object Security.AccessControl.FileSystemAccessRule($sid,'FullControl','Allow') }
        $acl.AddAccessRule($rule)
    }
    # Change only Access on a descriptor loaded without Audit. Set-Acl can
    # request SeSecurityPrivilege when persisting a newly built descriptor.
    $existing.SetSecurityDescriptorSddlForm($acl.GetSecurityDescriptorSddlForm($accessSection), $accessSection)
    $item = Get-Item -LiteralPath $Path -Force
    if ($PSVersionTable.PSEdition -eq 'Desktop') { $item.SetAccessControl($existing) }
    else { [IO.FileSystemAclExtensions]::SetAccessControl($item, $existing) }
}
function New-PrivateDirectory([string]$Path) {
    $parent = [IO.DirectoryInfo]$Path
    while (-not $parent.Exists) { $parent = $parent.Parent; if ($null -eq $parent) { throw 'A Cage tool parent is invalid.' } }
    Assert-NoReparse $parent.FullName
    if (-not (Test-Path -LiteralPath $Path)) { New-Item -ItemType Directory -Path $Path | Out-Null }
    Assert-NoReparse $Path; Protect-Path $Path $true
}
function Select-ClaudeCodeBinary([string]$Claude, [string]$Version, [string]$Digest, [string]$BridgeDirectory) {
    $reported=(& $claude --version | Select-Object -First 1)
    if ($LASTEXITCODE -ne 0 -or $reported -notmatch '^(\d+\.\d+\.\d+)(?:\s|$)' -or $Matches[1] -ne $Version) { throw 'The downloaded Claude Code version differs from the selected release.' }
    $claudeVersion=$Version
    $claudeDigest=$Digest
    $previousReceipt=Join-Path $BridgeDirectory 'cli-tools.json'
    if (Test-Path -LiteralPath $previousReceipt) {
        $previous=Get-Content -LiteralPath $previousReceipt -Raw | ConvertFrom-Json
        Assert-NoReparse $previous.claude
        if ((Get-FileHash -LiteralPath $previous.claude -Algorithm SHA256).Hash -ne $previous.claude_sha256) { throw 'The installed Claude Code no longer matches its protected receipt.' }
        $signature=Get-AuthenticodeSignature -LiteralPath $previous.claude
        if ($signature.Status -ne 'Valid' -or $signature.SignerCertificate.Subject -notmatch 'Anthropic,? PBC') { throw 'The installed Claude Code signature is invalid.' }
        $reported=(& $previous.claude --version | Select-Object -First 1)
        if ($LASTEXITCODE -ne 0 -or $reported -notmatch '^(\d+\.\d+\.\d+)(?:\s|$)') { throw 'The installed Claude Code version could not be verified.' }
        if ([version]$Matches[1] -gt [version]$claudeVersion) {
            $claudeVersion=$Matches[1]; $claude=$previous.claude; $claudeDigest=$previous.claude_sha256
        }
    }
    return [pscustomobject]@{Path=$claude;Version=$claudeVersion;SHA256=$claudeDigest}
}

function Check-Package($Entry) {
    $path = Join-Path $bundle $Entry.filename
    Assert-NoReparse $path
    if ((Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant() -ne $Entry.sha256) { throw 'An offline native package failed its fixed checksum.' }
    return $path
}
$packages = @{}
foreach ($name in @('uv','python','claude','git','gh')) { $packages[$name] = Check-Package $manifest.$name }
foreach ($name in @('claude','git')) {
    $signature = Get-AuthenticodeSignature -FilePath $packages[$name]
    if ($signature.Status -ne 'Valid') { throw 'An official Windows executable has an invalid Authenticode signature.' }
    if ($name -eq 'claude' -and $signature.SignerCertificate.Subject -notmatch 'Anthropic,? PBC') { throw 'The native Claude binary has the wrong signer.' }
}
$tools = Join-Path $env:USERPROFILE 'Cage\CLI\tools'
New-PrivateDirectory $tools
$uvDir = Join-Path $tools ('uv\' + $manifest.uv.version)
New-PrivateDirectory $uvDir
$uv = Join-Path $uvDir 'uv.exe'
if (-not (Test-Path -LiteralPath $uv)) { Expand-Archive -LiteralPath $packages.uv -DestinationPath $uvDir }
Assert-NoReparse $uv
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [IO.Compression.ZipFile]::OpenRead($packages.uv)
try {
    $entry = $zip.Entries | Where-Object { $_.FullName -eq 'uv.exe' } | Select-Object -First 1
    if ($null -eq $entry) { throw 'The fixed uv archive has no native executable.' }
    $reader = $entry.Open(); $hasher = [Security.Cryptography.SHA256]::Create()
    try { $expected = ([BitConverter]::ToString($hasher.ComputeHash($reader))).Replace('-','').ToLowerInvariant() } finally { $reader.Dispose(); $hasher.Dispose() }
    if ((Get-FileHash $uv -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw 'The Cage-owned uv executable changed.' }
} finally { $zip.Dispose() }
if ((& $uv --version) -notmatch ('^uv ' + [Regex]::Escape($manifest.uv.version) + '(?: |$)')) { throw 'The offline uv version does not match.' }
$pythonDir = Join-Path $tools 'python'
New-PrivateDirectory $pythonDir
$pythonMeta = Join-Path $tools ('python-' + $manifest.python.version + '.json')
$download = [ordered]@{name='cpython';arch=@{family='x86_64';variant=$null};os='windows';libc='none';major=3;minor=13;patch=18;prerelease='';url=([Uri]$packages.python).AbsoluteUri;sha256=$manifest.python.sha256;variant=$null;build='20261026'}
$metadata = @{}; $metadata[$manifest.python.key] = $download
[IO.File]::WriteAllText($pythonMeta, ($metadata | ConvertTo-Json -Depth 5), (New-Object Text.UTF8Encoding($false)))
Protect-Path $pythonMeta $false
$oldCache = [Environment]::GetEnvironmentVariable('UV_PYTHON_CACHE_DIR', 'Process')
try {
    [Environment]::SetEnvironmentVariable('UV_PYTHON_CACHE_DIR', $null, 'Process')
    & $uv python install $manifest.python.key --install-dir $pythonDir --no-bin --no-registry --no-config --offline --python-downloads-json-url $pythonMeta
    if ($LASTEXITCODE -ne 0) { throw 'Offline uv Python installation failed.' }
} finally { [Environment]::SetEnvironmentVariable('UV_PYTHON_CACHE_DIR', $oldCache, 'Process') }
$python = Join-Path $pythonDir ($manifest.python.key + '\python.exe')
Assert-NoReparse $python
if ((& $python -X utf8 -I -c 'import platform; print(platform.python_version())') -ne $manifest.python.version) { throw 'The native Python version does not match.' }
$gitDir = Join-Path $tools ('git\' + $manifest.git.version)
if (-not (Test-Path (Join-Path $gitDir 'bin\bash.exe'))) {
    New-PrivateDirectory $gitDir
    $extract = Start-Process -FilePath $packages.git -ArgumentList @('-y', ('-o"' + $gitDir + '"')) -Wait -PassThru -NoNewWindow
    if ($extract.ExitCode -ne 0) { throw 'Portable Git extraction failed.' }
}
$bash = Join-Path $gitDir 'bin\bash.exe'; Assert-NoReparse $bash
$ghDir = Join-Path $tools ('gh\' + $manifest.gh.version)
New-PrivateDirectory $ghDir
if (-not (Test-Path (Join-Path $ghDir 'bin\gh.exe'))) { Expand-Archive -LiteralPath $packages.gh -DestinationPath $ghDir }
$gh = Join-Path $ghDir 'bin\gh.exe'; Assert-NoReparse $gh
$ghZip = [IO.Compression.ZipFile]::OpenRead($packages.gh)
try {
    $entry = $ghZip.GetEntry('bin/gh.exe')
    if ($null -eq $entry) { throw 'The fixed GitHub CLI archive has no native executable.' }
    $reader = $entry.Open(); $hasher = [Security.Cryptography.SHA256]::Create()
    try { $expected = ([BitConverter]::ToString($hasher.ComputeHash($reader))).Replace('-','').ToLowerInvariant() } finally { $reader.Dispose(); $hasher.Dispose() }
    if ((Get-FileHash $gh -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw 'The Cage-owned GitHub CLI executable changed.' }
} finally { $ghZip.Dispose() }
$claudeDir = Join-Path $tools ('claude\' + $manifest.claude.version)
New-PrivateDirectory $claudeDir
$claude = Join-Path $claudeDir 'claude.exe'
if (-not (Test-Path -LiteralPath $claude)) { [IO.File]::Copy($packages.claude, $claude, $false) }
if ((Get-FileHash -LiteralPath $claude -Algorithm SHA256).Hash.ToLowerInvariant() -ne $manifest.claude.sha256) { throw 'The Cage-owned native Claude binary changed.' }
$selected=Select-ClaudeCodeBinary $claude $manifest.claude.version $manifest.claude.sha256 $bridge
$claude=$selected.Path; $claudeVersion=$selected.Version; $claudeDigest=$selected.SHA256
# The official native binary is used as a versioned custom launcher. No default
# Claude account, existing installer, project, settings or history is replaced.
$receipt = [ordered]@{version=1;python=$python;python_version=$manifest.python.version;python_sha256=(Get-FileHash $python -Algorithm SHA256).Hash.ToLowerInvariant();claude=$claude;claude_version=$claudeVersion;claude_sha256=$claudeDigest;git_bash=$bash;git_bash_sha256=(Get-FileHash $bash -Algorithm SHA256).Hash.ToLowerInvariant();uv_version=$manifest.uv.version}
$receipt.gh=$gh
$receipt.gh_sha256=(Get-FileHash $gh -Algorithm SHA256).Hash.ToLowerInvariant()
Assert-NoReparse $bridge
if ($WorkbenchSetup) {
    # The provisioning process owns this file and its directory. The paired
    # user can write the receipt, but cannot replace the file or change its ACL.
    $staged = Join-Path $bridge 'setup-packages\cli-receipt.in'
    Assert-NoReparse $staged
    [IO.File]::WriteAllText($staged, ($receipt | ConvertTo-Json), (New-Object Text.UTF8Encoding($false)))
    return
}
$receiptDir = [IO.Path]::GetFullPath($ReceiptDirectory)
$receiptRoot = [IO.Path]::GetFullPath((Join-Path $env:USERPROFILE 'Cage\CLI'))
if (-not $receiptDir.StartsWith($receiptRoot + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'The receipt staging directory must be inside the current user Cage CLI directory.' }
New-PrivateDirectory $receiptDir
$target = Join-Path $receiptDir 'cli-tools.json'
$temporary = Join-Path $receiptDir ('cli-tools.' + [Guid]::NewGuid().ToString('N') + '.json')
[IO.File]::WriteAllText($temporary, ($receipt | ConvertTo-Json), (New-Object Text.UTF8Encoding($false)))
Protect-Path $temporary $false
Move-Item -LiteralPath $temporary -Destination $target -Force
Write-Output 'Native Cage CLI tools installed. Management must copy the staged receipt into the readonly paired bridge, then restart only that bridge. Official account login remains user-controlled.'
