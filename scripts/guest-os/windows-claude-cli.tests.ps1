# Exercise the production ordinary-user selection with real version commands
# and files. Windows certificate results are substituted on other platforms.
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$global:LASTEXITCODE=0
$errors=$null
$source=Join-Path $PSScriptRoot 'guest-clipboard-windows/install-cli.ps1'
$ast=[Management.Automation.Language.Parser]::ParseFile($source,[ref]$null,[ref]$errors)
if ($errors.Count) { throw $errors[0] }
foreach ($name in @('Assert-NoReparse','Select-ClaudeCodeBinary')) {
    $definition=$ast.Find({param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name},$true)
    if (!$definition) { throw "Missing production function $name" }
    . ([scriptblock]::Create($definition.Extent.Text))
}
$ast=[Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'install-windows-workbench-components.ps1'),[ref]$null,[ref]$errors)
if ($errors.Count) { throw $errors[0] }
foreach ($name in @('Assert-LocalClaudeTool','Assert-ClaudeCodeReceipt')) {
    $definition=$ast.Find({param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name},$true)
    . ([scriptblock]::Create($definition.Extent.Text))
}
$temporaryBase=[IO.Path]::GetTempPath()
if ([Environment]::OSVersion.Platform -eq 'Unix' -and (Test-Path -LiteralPath '/private/tmp')) { $temporaryBase='/private/tmp' }
$root=Join-Path $temporaryBase ('claude-cli-selection-'+[guid]::NewGuid().ToString('N'))
$null=New-Item -ItemType Directory -Path $root
$script:validSignature=$true
function Get-AuthenticodeSignature {
    param($LiteralPath)
    [pscustomobject]@{Status=$(if ($script:validSignature) {'Valid'} else {'NotSigned'});SignerCertificate=@{Subject='CN=Anthropic, PBC'}}
}
function New-ClaudeVersion([string]$Version) {
    $file=Join-Path $root ('claude-'+$Version+'.ps1')
    [IO.File]::WriteAllText($file,"Write-Output '$Version (Claude Code)'`nexit 0`n")
    return $file
}
function Reject-Selection([scriptblock]$Action) {
    $failed=$false
    try { & $Action } catch { $failed=$true }
    if (!$failed) { throw 'An invalid Claude Code selection was accepted.' }
}
try {
    $incoming=New-ClaudeVersion '2.2.0'
    $digest=(Get-FileHash $incoming).Hash.ToLowerInvariant()
    $selected=Select-ClaudeCodeBinary $incoming '2.2.0' $digest $root
    if ($selected.Version -ne '2.2.0' -or $selected.Path -ne $incoming) { throw 'Fresh installation selected another release.' }
    $previous=New-ClaudeVersion '2.3.0'
    $previousDigest=(Get-FileHash $previous).Hash.ToLowerInvariant()
    $receipt=Join-Path $root 'cli-tools.json'
    [IO.File]::WriteAllText($receipt,(@{claude=$previous;claude_sha256=$previousDigest}|ConvertTo-Json))
    $selected=Select-ClaudeCodeBinary $incoming '2.2.0' $digest $root
    if ($selected.Version -ne '2.3.0' -or $selected.Path -ne $previous -or $selected.SHA256 -ne $previousDigest) { throw 'An existing newer Claude Code was downgraded.' }
    $newer=New-ClaudeVersion '2.4.0'
    $selected=Select-ClaudeCodeBinary $newer '2.4.0' (Get-FileHash $newer).Hash.ToLowerInvariant() $root
    if ($selected.Path -ne $newer) { throw 'A newer incoming release was not selected.' }
    $expected=[pscustomobject]@{version='2.2.0';sha256=$digest}
    $value=[pscustomobject]@{version=1;claude=$incoming;claude_version='2.2.0';claude_sha256=$digest}
    Assert-ClaudeCodeReceipt $value $expected $receipt
    $value.claude='\\untrusted.example\share\claude.exe'
    Reject-Selection { Assert-ClaudeCodeReceipt $value $expected $receipt }
    $value.claude=$incoming
    $value.claude_version='9.9.9'
    Reject-Selection { Assert-ClaudeCodeReceipt $value $expected $receipt }
    $value.claude=$previous; $value.claude_sha256=$previousDigest; $value.claude_version='2.3.0'
    Reject-Selection { Assert-ClaudeCodeReceipt $value $expected $receipt }
    [IO.File]::WriteAllText($receipt,(@{claude=$previous;claude_sha256=$previousDigest;claude_version='2.3.0'}|ConvertTo-Json))
    Assert-ClaudeCodeReceipt $value $expected $receipt
    $value.claude_version='9.9.9'
    Reject-Selection { Assert-ClaudeCodeReceipt $value $expected $receipt }
    Reject-Selection { Select-ClaudeCodeBinary $incoming '2.1.0' $digest $root }
    $script:validSignature=$false
    Reject-Selection { Select-ClaudeCodeBinary $incoming '2.2.0' $digest $root }
    $script:validSignature=$true
    [IO.File]::AppendAllText($previous,'# changed')
    Reject-Selection { Select-ClaudeCodeBinary $incoming '2.2.0' $digest $root }
    Write-Output 'Passed actual Claude Code versions, newer-installed preservation, upgrades, protected receipt binding, legacy receipt retention and spoofed version/altered binary/unsigned binary rejection (certificate API simulated).'
} finally { Remove-Item -LiteralPath $root -Recurse -Force }
