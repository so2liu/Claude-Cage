#requires -Version 5.1
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$installers = @('install-cli.ps1', 'install-ccs.ps1')
$functions = @{}
foreach ($name in $installers) {
    $errors = $null
    $ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot ('guest-clipboard-windows/'+$name)), [ref]$null, [ref]$errors)
    if ($errors.Count) { throw $errors[0] }
    $protect = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Protect-Path' }, $true)
    $wideWrite = $protect.Find({
        param($node)
        ($node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Set-Acl') -or
        ($node -is [Management.Automation.Language.InvokeMemberExpressionAst] -and $node.Member.Value -in @('SetOwner','SetGroup','SetAuditRule','SetAuditRuleProtection'))
    }, $true)
    if ($wideWrite) { throw "$name must persist only DACL changes for the ordinary GUI user." }
    $functions[$name] = $ast.FindAll({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in @('Assert-NoReparse','Protect-Path','New-PrivateDirectory','PrivateDirectory') }, $true)
}
if ([Environment]::OSVersion.Platform -ne 'Win32NT') {
    Write-Output 'Parsed both installers and checked the DACL-only write contract. Native ACL fixtures require Windows.'
    return
}
$user = [Security.Principal.WindowsIdentity]::GetCurrent()
if ([Security.Principal.WindowsPrincipal]::new($user).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'Run the ACL fixtures as an ordinary user.' }
$privileges = & (Join-Path ([Environment]::SystemDirectory) 'whoami.exe') /priv /fo csv /nh | ConvertFrom-Csv -Header Name,Description,State
if ($LASTEXITCODE -ne 0 -or @($privileges | Where-Object Name -eq 'SeSecurityPrivilege').Count) { throw 'The ACL fixtures require a token without SeSecurityPrivilege.' }
$readSections = [Security.AccessControl.AccessControlSections]::Owner -bor [Security.AccessControl.AccessControlSections]::Group -bor [Security.AccessControl.AccessControlSections]::Access
function Read-TestSecurity([string]$Path, [bool]$Directory) {
    if ($Directory) { return [Security.AccessControl.DirectorySecurity]::new($Path, $readSections) }
    return [Security.AccessControl.FileSecurity]::new($Path, $readSections)
}
function Check-PrivateAccess($Before, $After, [bool]$Directory) {
    $sidType = [Security.Principal.SecurityIdentifier]
    if ($Before.GetOwner($sidType) -ne $After.GetOwner($sidType) -or $Before.GetGroup($sidType) -ne $After.GetGroup($sidType)) { throw 'Owner or group changed.' }
    $expected = @($user.User.Value, 'S-1-5-18', 'S-1-5-32-544')
    $rules = @($After.GetAccessRules($true, $true, $sidType))
    if (!$After.AreAccessRulesProtected -or $rules.Count -ne 3) { throw 'The private DACL has the wrong protection or rule count.' }
    if ((@($rules.IdentityReference.Value | Sort-Object) -join ',') -cne (@($expected | Sort-Object) -join ',')) { throw 'The private DACL has the wrong principals.' }
    $inheritance = if ($Directory) { [Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit' } else { [Security.AccessControl.InheritanceFlags]::None }
    foreach ($rule in $rules) {
        if ($rule.IsInherited -or $rule.FileSystemRights -ne [Security.AccessControl.FileSystemRights]::FullControl -or
            $rule.AccessControlType -ne 'Allow' -or $rule.InheritanceFlags -ne $inheritance -or $rule.PropagationFlags -ne 'None') { throw 'The private DACL changed its access or inheritance contract.' }
    }
}
$root = Join-Path ([IO.Path]::GetTempPath()) ('cage-user-tools-acl-'+[guid]::NewGuid().ToString('N'))
$null = [IO.Directory]::CreateDirectory($root)
try {
    foreach ($name in $installers) {
        foreach ($definition in $functions[$name]) { . ([scriptblock]::Create($definition.Extent.Text)) }
        $directory = Join-Path $root ($name+' tools')
        $null = [IO.Directory]::CreateDirectory($directory)
        $before = Read-TestSecurity $directory $true
        if ($name -eq 'install-cli.ps1') { New-PrivateDirectory $directory } else { PrivateDirectory $directory }
        Check-PrivateAccess $before (Read-TestSecurity $directory $true) $true
        # A fresh child inherits all three directory rules before its own
        # explicit protection. Its content represents a retained install receipt.
        $file = Join-Path $directory 'retained receipt.json'
        [IO.File]::WriteAllText($file, 'existing fixture contents')
        $before = Read-TestSecurity $file $false
        $inherited = @($before.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
        if ($inherited.Count -ne 3 -or @($inherited | Where-Object { !$_.IsInherited }).Count) { throw 'New files did not inherit the protected directory rules.' }
        Protect-Path $file $false
        Check-PrivateAccess $before (Read-TestSecurity $file $false) $false
        foreach ($entry in @(@($directory,$true), @($file,$false))) {
            $before = Read-TestSecurity $entry[0] $entry[1]
            Protect-Path $entry[0] $entry[1]
            $after = Read-TestSecurity $entry[0] $entry[1]
            if ($before.GetSecurityDescriptorSddlForm($readSections) -cne $after.GetSecurityDescriptorSddlForm($readSections)) { throw 'Retry changed existing security metadata.' }
            $actualUser = $user
            try {
                $user = @{ User = [Security.Principal.SecurityIdentifier]::new('S-1-5-21-111-222-333-1001') }
                try { Protect-Path $entry[0] $entry[1]; throw 'Wrong owner accepted' }
                catch { if ($_.Exception.Message -notlike '*must already belong to the current GUI user*') { throw } }
            } finally { $user = $actualUser }
            $after = Read-TestSecurity $entry[0] $entry[1]
            if ($before.GetSecurityDescriptorSddlForm($readSections) -cne $after.GetSecurityDescriptorSddlForm($readSections)) { throw 'Owner rejection changed security metadata.' }
        }
        if ([IO.File]::ReadAllText($file) -cne 'existing fixture contents') { throw 'Protection changed existing file contents.' }
    }
    Write-Output 'Passed ordinary-user CLI/CCS directory and file DACLs, inherited children, owner/group preservation, retries and owner rejection.'
} finally { [IO.Directory]::Delete($root, $true) }
