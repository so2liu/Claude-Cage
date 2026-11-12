#requires -Version 5.1
$ErrorActionPreference = 'Stop'
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'windows-host-enrollment.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw $parseErrors[0] }
$wait = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Wait-CageHostEnrollment' }, $true)
. ([scriptblock]::Create($wait.Extent.Text))
$script:requests = 0
$worker = [pscustomobject]@{ HasExited = $false; ExitCode = 1 }
function Invoke-CageHostEnrollment {
    param($Context, $Request, $Secret)
    $script:requests++
    if ($script:failRequest) { $worker.HasExited = $true; throw 'Injected pipe failure' }
    if ($Request.action -cne 'ready') { throw 'Unexpected enrollment request' }
}
Wait-CageHostEnrollment @{} $worker
if ($script:requests -ne 1) { throw 'The ready handshake was not performed.' }
$script:failRequest = $true
$watch = [Diagnostics.Stopwatch]::StartNew()
try { Wait-CageHostEnrollment @{} $worker; throw 'Worker failure was ignored.' }
catch {
    if ($_.Exception.Message -notlike '*enrollment_worker_exited*') { throw }
}
if ($script:requests -ne 2 -or $watch.Elapsed.TotalSeconds -gt 2) { throw 'An exited worker was retried until the ready timeout.' }
Write-Output 'Passed ready handshake and early worker-exit reporting.'
