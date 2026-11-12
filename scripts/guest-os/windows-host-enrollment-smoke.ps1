#requires -Version 5.1
[CmdletBinding()]
param([Parameter(Mandatory)][string]$ManagerExecutable)
$ErrorActionPreference='Stop';$ProgressPreference='SilentlyContinue'
$identity=[Security.Principal.WindowsIdentity]::GetCurrent()
if(([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)){throw 'Run this same-user protocol/DPAPI smoke as the ordinary host user.'}
. (Join-Path $PSScriptRoot 'windows-host-enrollment.ps1')
$root=Join-Path $env:TEMP ('cage-host-enrollment-'+[guid]::NewGuid().ToString('N'))
$profile=Join-Path $root 'profile';$state=Join-Path $profile 'state';$id=[guid]::NewGuid();$vm=[guid]::NewGuid()
$instance=Join-Path $state ('windows-'+$id.ToString('N'));$pair=Join-Path $instance 'cage-workbench/pair'
$null=New-Item -ItemType Directory -Path $pair -Force
$choice=@{version=1;system='windows-11';media=(Join-Path $root 'fixture.iso');sha256=('0'*64);editor_terms=$true;id=$id.ToString('D')}
[IO.File]::WriteAllText((Join-Path $profile 'guest-install.json'),($choice|ConvertTo-Json))
[IO.File]::WriteAllText((Join-Path $profile 'windows-install-vm.txt'),$vm.ToString('D'))
$pipe='Cage.Enroll.'+[guid]::NewGuid().ToString('N');$worker=$null;$sender=$null;$context=$null
try {
    $workerArguments=@('user-enrollment','--vm-id',$vm.ToString('D'),'--host-sid',$identity.User.Value,'--profile',('"'+$profile+'"'),'--state',('"'+$state+'"'),'--installation-id',$id.ToString('D'),'--pipe',$pipe,'--parent-pid',[string]$PID)
    $worker=Start-Process -FilePath $ManagerExecutable -ArgumentList $workerArguments -NoNewWindow -PassThru
    $context=@{Pipe=$pipe;VMID=$vm.ToString('D');HostSID=$identity.User.Value;WorkerPID=[uint32]$worker.Id;WorkerStarted=[Cage.HostEnrollment.Native]::Started([uint32]$worker.Id);WorkerExe=[IO.Path]::GetFullPath($ManagerExecutable);OwnerPID=[uint32]$PID;OwnerStarted=[Cage.HostEnrollment.Native]::Started([uint32]$PID)}
    Wait-CageHostEnrollment $context $worker
    $ctxPath=Join-Path $root 'context.json';[IO.File]::WriteAllText($ctxPath,($context|ConvertTo-Json))
    $module=Join-Path $PSScriptRoot 'windows-host-enrollment.ps1'
    $senderPath=Join-Path $root 'sender.ps1';$result=Join-Path $root 'result.json'
    $script=@'
param($ContextPath,$Module,$Pair,$Result)
$ErrorActionPreference='Stop'
. $Module
$ctx=Get-Content -LiteralPath $ContextPath -Raw|ConvertFrom-Json
Start-Sleep -Milliseconds 500
$sid='S-1-5-21-111-222-333-1001';$key=[byte[]](1..32)
try {
 Send-CageHostEnrollmentKey $ctx 'gui' $sid $key
 $path=Join-Path $Pair 'pairing.dpapi';$before=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
 Send-CageHostEnrollmentKey $ctx 'gui' $sid $key
 $same=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash -ceq $before
 $different=$false;try{Send-CageHostEnrollmentKey $ctx 'gui' $sid ([byte[]](2..33))}catch{$different=$true}
 $wrongSID=$false;try{Send-CageHostEnrollmentKey $ctx 'packet' 'S-1-5-21-111-222-333-1002' $key}catch{$wrongSID=$true}
 [IO.File]::WriteAllText($Result,(@{sameCipher=$same;changedKeyRejected=$different;guestChangedRejected=$wrongSID;cipherPreserved=((Get-FileHash -LiteralPath $path).Hash -ceq $before)}|ConvertTo-Json))
}finally{[Array]::Clear($key,0,$key.Length)}
'@
    [IO.File]::WriteAllText($senderPath,$script)
    $system=Join-Path ([Environment]::GetFolderPath('System')) 'WindowsPowerShell/v1.0/powershell.exe'
    $sender=Start-Process -FilePath $system -ArgumentList @('-NoLogo','-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',('"'+$senderPath+'"'),'-ContextPath',('"'+$ctxPath+'"'),'-Module',('"'+$module+'"'),'-Pair',('"'+$pair+'"'),'-Result',('"'+$result+'"')) -NoNewWindow -PassThru
    # This fixture deliberately uses a same-user PowerShell child. It proves
    # kernel handle/PID binding and CurrentUser DPAPI, not alternate-admin UAC.
    Set-CageHostEnrollmentAdmin $context $sender
    if(!$sender.WaitForExit(30000) -or $sender.ExitCode -ne 0){throw 'The native fixture sender did not complete.'}
    $passed=Get-Content -LiteralPath $result -Raw|ConvertFrom-Json
    if(!$passed.sameCipher -or !$passed.changedKeyRejected -or !$passed.guestChangedRejected -or !$passed.cipherPreserved){throw 'Native enrollment mutation guards failed.'}
    Add-Type -AssemblyName System.Security
    $entropy=[Security.Cryptography.SHA256]::Create().ComputeHash([Text.Encoding]::UTF8.GetBytes('Cage.HyperV.v1:'+$vm.ToString('D')+':S-1-5-21-111-222-333-1001'))
    $raw=[Security.Cryptography.ProtectedData]::Unprotect([IO.File]::ReadAllBytes((Join-Path $pair 'pairing.dpapi')),$entropy,'CurrentUser')
    try{if(($raw -join ',') -cne (([byte[]](1..32)) -join ',')){throw 'Original user DPAPI interoperability failed.'}}finally{[Array]::Clear($raw,0,$raw.Length)}
    if(Test-Path -LiteralPath (Join-Path $profile 'windows-guest.json')){throw 'A fixture without actual VM/GUI proof published a final profile.'}
    Write-Output 'Native same-user enrollment/DPAPI protocol smoke passed; alternate-admin UAC and actual VM enrollment remain separate.'
}finally {
    if($context){try{Stop-CageHostEnrollment $context}catch{}}
    if($sender){if(!$sender.HasExited){$sender.Kill();$sender.WaitForExit()};$sender.Dispose()}
    if($worker){if(!$worker.WaitForExit(5000)){$worker.Kill();$worker.WaitForExit()};$worker.Dispose()}
    # Keep this script's own result directory for evidence. No VM, account,
    # service, guest data or user configuration was created or replaced.
}
