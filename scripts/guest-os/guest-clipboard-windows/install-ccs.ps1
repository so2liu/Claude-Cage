# Execute as the paired ordinary GUI user. Management copies only the validated
# receipt into the readonly bridge; no ProgramFiles write permission is added.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$RuntimeArchive,
    [Parameter(Mandatory)][string]$NodeArchive,
    [Parameter(Mandatory)][string]$BridgeDirectory,
    [string]$ExistingNode = '',
    [string]$ReceiptDirectory = (Join-Path $env:USERPROFILE 'Cage\CLI\install-receipts')
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
if (-not [Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([Runtime.InteropServices.OSPlatform]::Windows)) { throw 'Native Windows is required.' }
$user=[Security.Principal.WindowsIdentity]::GetCurrent()
if ((New-Object Security.Principal.WindowsPrincipal($user)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Use the paired ordinary GUI user.' }
$bridge=[IO.Path]::GetFullPath($BridgeDirectory)
if (-not (Test-Path (Join-Path $bridge 'Cage.GuestClipboard.exe') -PathType Leaf)) { throw 'Install the paired bridge first.' }
function Assert-NoReparse([string]$Path) {
    $item=Get-Item -LiteralPath $Path -Force
    while ($null -ne $item) { if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'A native CCS path is a reparse point.' }; if ($item -is [IO.FileInfo]) { $item=$item.Directory } else { $item=$item.Parent } }
}
function Protect-Path([string]$Path,[bool]$Directory) {
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
    $acl.SetAccessRuleProtection($true,$false)
    foreach ($sid in @($user.User,[Security.Principal.SecurityIdentifier]'S-1-5-18',[Security.Principal.SecurityIdentifier]'S-1-5-32-544')) {
        if ($Directory) { $rule=New-Object Security.AccessControl.FileSystemAccessRule($sid,'FullControl','ContainerInherit,ObjectInherit','None','Allow') }
        else { $rule=New-Object Security.AccessControl.FileSystemAccessRule($sid,'FullControl','Allow') }
        $acl.AddAccessRule($rule)
    }
    # Change only Access on a descriptor loaded without Audit. Set-Acl can
    # request SeSecurityPrivilege when persisting a newly built descriptor.
    $existing.SetSecurityDescriptorSddlForm($acl.GetSecurityDescriptorSddlForm($accessSection), $accessSection)
    $item = Get-Item -LiteralPath $Path -Force
    if ($PSVersionTable.PSEdition -eq 'Desktop') { $item.SetAccessControl($existing) }
    else { [IO.FileSystemAclExtensions]::SetAccessControl($item, $existing) }
}
function PrivateDirectory([string]$Path) {
    $parent=[IO.DirectoryInfo]$Path
    while (-not $parent.Exists) { $parent=$parent.Parent;if($null -eq $parent){throw 'Invalid native tool parent.'} }
    Assert-NoReparse $parent.FullName
    if(-not (Test-Path -LiteralPath $Path)){New-Item -ItemType Directory -Path $Path | Out-Null}
    Assert-NoReparse $Path;Protect-Path $Path $true
}
Assert-NoReparse $RuntimeArchive;Assert-NoReparse $NodeArchive;Assert-NoReparse $bridge
if ((Get-FileHash $RuntimeArchive -Algorithm SHA256).Hash.ToLowerInvariant() -ne 'a0ab3bbac6b2c14ba53020474a4d6a6b307b9faf6f10998568b3200535c6b3e7') { throw 'CCS runtime archive checksum failed.' }
if ((Get-FileHash $NodeArchive -Algorithm SHA256).Hash.ToLowerInvariant() -ne 'ca1bff5d8ead13c9f2ea404a176db46bd430704d5b5113734049fa365e7799e2') { throw 'Official Node archive checksum failed.' }
$tools=Join-Path $env:USERPROFILE 'Cage\CLI\tools';PrivateDirectory $tools
$runtime=Join-Path $tools 'ccs\8.11.0'
if (-not (Test-Path -LiteralPath $runtime)) {
    $parent=Split-Path $runtime -Parent;PrivateDirectory $parent
    $stage=Join-Path $parent ('stage-'+[Guid]::NewGuid().ToString('N'));PrivateDirectory $stage
    & (Join-Path $env:SystemRoot 'System32\tar.exe') -xzf $RuntimeArchive -C $stage
    if($LASTEXITCODE -ne 0){throw 'CCS archive extraction failed; staged files were retained.'}
    [IO.Directory]::Move($stage,$runtime)
}
Assert-NoReparse $runtime
$manifest=Join-Path $runtime 'runtime-files.json'
$files=Get-Content -LiteralPath $manifest -Raw | ConvertFrom-Json
foreach($entry in $files.PSObject.Properties) {
    $file=[IO.Path]::GetFullPath((Join-Path $runtime $entry.Name))
    if(-not $file.StartsWith($runtime+'\',[StringComparison]::OrdinalIgnoreCase)){throw 'CCS runtime contains an invalid path.'}
    Assert-NoReparse $file
    if((Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant() -ne $entry.Value){throw 'A CCS runtime file changed.'}
}
$nodeSha='1c331340fa6cac5e4294725959070a143fa0507195a78ad77eed7943f719aac9'
if ($ExistingNode -ne '') {
    Assert-NoReparse $ExistingNode
    if((Get-FileHash $ExistingNode -Algorithm SHA256).Hash.ToLowerInvariant() -ne $nodeSha){throw 'Existing Node does not match the fixed official version.'}
    $node=[IO.Path]::GetFullPath($ExistingNode)
} else {
    $nodeDir=Join-Path $tools 'node\24.22.2';PrivateDirectory $nodeDir
    $node=Join-Path $nodeDir 'node-v24.22.2-win-x64\node.exe'
    if(-not(Test-Path -LiteralPath $node)){Expand-Archive -LiteralPath $NodeArchive -DestinationPath $nodeDir}
    Assert-NoReparse $node
    if((Get-FileHash $node -Algorithm SHA256).Hash.ToLowerInvariant() -ne $nodeSha){throw 'Native Node changed.'}
}
$signature=Get-AuthenticodeSignature -FilePath $node
if($signature.Status -ne 'Valid'){throw 'Native Node signature is invalid.'}
$main=Join-Path $runtime 'node_modules\@kaitranntt\ccs\dist\ccs.js'
$receiptDir=[IO.Path]::GetFullPath($ReceiptDirectory)
$allowed=[IO.Path]::GetFullPath((Join-Path $env:USERPROFILE 'Cage\CLI'))
if(-not $receiptDir.StartsWith($allowed+'\',[StringComparison]::OrdinalIgnoreCase)){throw 'Receipt staging must stay in the current user Cage CLI directory.'}
PrivateDirectory $receiptDir
$receipt=[ordered]@{version=1;node=$node;node_version='24.22.2';node_sha256=$nodeSha;main=$main;main_sha256=(Get-FileHash $main -Algorithm SHA256).Hash.ToLowerInvariant();manifest=$manifest;manifest_sha256=(Get-FileHash $manifest -Algorithm SHA256).Hash.ToLowerInvariant();ccs_version='8.11.0'}
$temporary=Join-Path $receiptDir ('ccs-tools.'+[Guid]::NewGuid().ToString('N')+'.json')
[IO.File]::WriteAllText($temporary,($receipt | ConvertTo-Json),(New-Object Text.UTF8Encoding($false)))
Protect-Path $temporary $false
Move-Item -LiteralPath $temporary -Destination (Join-Path $receiptDir 'ccs-tools.json') -Force
Write-Output 'Native CCS runtime validated. Management must copy the staged ccs-tools.json into the readonly paired bridge. Existing CCS profiles and credentials were not imported or changed.'
