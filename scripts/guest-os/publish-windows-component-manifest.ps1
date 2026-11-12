# Records only the files verified in this fixed guest after installation.
param([guid]$VMId,[string]$GUIUserSID,[string]$ManifestJSON)
$ErrorActionPreference='Stop'
if ([guid](Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Virtual Machine\Guest\Parameters').VirtualMachineId -ne $VMId -or
    $GUIUserSID -notmatch '^S-1-5-21-\d+-\d+-\d+-\d+$' -or [Text.Encoding]::UTF8.GetByteCount($ManifestJSON) -gt 4MB) {
    throw '组件版本记录与客机身份不一致。'
}
$manifest=$ManifestJSON|ConvertFrom-Json
if ($manifest.protocol -ne 1 -or $manifest.system -cne 'windows-11' -or $manifest.revision -notmatch '^[a-f0-9]{64}$' -or
    @($manifest.files).Count -lt 1 -or @($manifest.files).Count -gt 10000) { throw '组件版本清单无效。' }
$roots=@{bridge=(Join-Path $env:ProgramFiles ('Cage Guest Bridge\'+$VMId));network=(Join-Path $env:ProgramFiles ('Cage Guest Network\'+$VMId))}
$seen=@{}
foreach ($entry in $manifest.files) {
    if (!$entry.target) { continue }
    if ($entry.target -notmatch '^(bridge|network)/[^:\\]+$' -or $entry.target -match '(^|/)\.\.?(/|$)' -or
        $entry.installed_sha256 -notmatch '^[a-f0-9]{64}$' -or $seen.ContainsKey($entry.target)) { throw '组件文件记录无效。' }
    $seen[$entry.target]=$true
    $parts=$entry.target -split '/',2
    $file=Join-Path $roots[$parts[0]] $parts[1]
    $item=Get-Item -LiteralPath $file -Force
    if ($item -isnot [IO.FileInfo]) { throw '安装的组件文件缺失，请继续同步环境组件。' }
    for ($parent=$item; $parent; $parent=if($parent -is [IO.FileInfo]){$parent.Directory}else{$parent.Parent}) {
        if ($parent.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '组件安装目录包含文件链接。' }
    }
    if ((Get-FileHash -LiteralPath $file).Hash -ne $entry.installed_sha256) { throw '安装的组件与当前应用版本不匹配，请继续同步环境组件。' }
}
if (!$seen.Count) { throw '组件清单缺少安装文件。' }
$target=Join-Path $roots.bridge 'native-components.json'
if (Test-Path -LiteralPath $target) {
    $old=Get-Item -LiteralPath $target -Force
    if ($old -isnot [IO.FileInfo] -or $old.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '现有组件记录无法安全更新。' }
}
$temporary=Join-Path $roots.bridge ('native-components.'+[guid]::NewGuid().ToString('N')+'.json')
try {
    $file=[IO.File]::Open($temporary,'CreateNew','Write','None')
    try {
        $bytes=[Text.UTF8Encoding]::new($false).GetBytes($ManifestJSON)
        $file.Write($bytes,0,$bytes.Length)
        $file.Flush($true)
    } finally { $file.Dispose() }
    $acl=[Security.AccessControl.FileSecurity]::new()
    $acl.SetAccessRuleProtection($true,$false)
    $acl.SetOwner([Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))
    foreach ($sid in @('S-1-5-18','S-1-5-32-544')) {
        $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($sid),'FullControl','Allow'))
    }
    $acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new([Security.Principal.SecurityIdentifier]::new($GUIUserSID),'ReadAndExecute','Allow'))
    Set-Acl -LiteralPath $temporary -AclObject $acl
    if (Test-Path -LiteralPath $target) { [IO.File]::Replace($temporary,$target,[Management.Automation.Language.NullString]::Value) }
    else { [IO.File]::Move($temporary,$target) }
} finally {
    if ([IO.File]::Exists($temporary)) { [IO.File]::Delete($temporary) }
}
