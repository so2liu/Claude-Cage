#requires -Version 5.1
# This module is dot-sourced by the fixed setup scripts. It exposes no general
# command, path or service RPC; raw pairing bytes stay inside the private pipe.
if(-not ('Cage.HostEnrollment.Native' -as [type])) {
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Text;
using Microsoft.Win32.SafeHandles;
namespace Cage.HostEnrollment {
public static class Native {
 [StructLayout(LayoutKind.Sequential)] struct FileTime {public uint Low,High;}
 [StructLayout(LayoutKind.Sequential)] struct Basic {public IntPtr Exit,Peb,Affinity,Priority,Pid,Parent;}
 [DllImport("kernel32.dll",SetLastError=true)] public static extern IntPtr OpenProcess(uint access,bool inherit,uint pid);
 [DllImport("kernel32.dll")] public static extern bool CloseHandle(IntPtr handle);
 [DllImport("kernel32.dll")] public static extern IntPtr GetCurrentProcess();
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool DuplicateHandle(IntPtr sourceProcess,IntPtr source,IntPtr targetProcess,out IntPtr target,uint access,bool inherit,uint options);
 [DllImport("kernel32.dll",SetLastError=true)] public static extern bool GetNamedPipeServerProcessId(SafePipeHandle pipe,out uint pid);
 [DllImport("kernel32.dll",SetLastError=true)] static extern bool GetProcessTimes(IntPtr process,out FileTime created,out FileTime exited,out FileTime kernel,out FileTime user);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,ExactSpelling=true,SetLastError=true)] static extern bool QueryFullProcessImageNameW(IntPtr process,uint flags,StringBuilder image,ref uint size);
 [DllImport("kernel32.dll",CharSet=CharSet.Unicode,ExactSpelling=true,SetLastError=true)] static extern uint GetLongPathNameW(string path,StringBuilder expanded,uint capacity);
 [DllImport("advapi32.dll",SetLastError=true)] static extern bool OpenProcessToken(IntPtr process,uint access,out IntPtr token);
 [DllImport("advapi32.dll",SetLastError=true)] static extern bool GetTokenInformation(IntPtr token,int kind,IntPtr data,uint bytes,out uint needed);
 [DllImport("ntdll.dll")] static extern int NtQueryInformationProcess(IntPtr process,int kind,out Basic basic,uint bytes,out uint needed);
 public static long Started(uint pid) {
  var p=OpenProcess(0x1000,false,pid);if(p==IntPtr.Zero)throw new InvalidOperationException("enrollment_peer_invalid");
  try {FileTime c,e,k,u;if(!GetProcessTimes(p,out c,out e,out k,out u))throw new InvalidOperationException("enrollment_peer_invalid");return (long)(((ulong)c.High<<32)|c.Low);}finally{CloseHandle(p);}
 }
 public static string Image(uint pid) {
  var p=OpenProcess(0x1000,false,pid);if(p==IntPtr.Zero)throw new InvalidOperationException("enrollment_peer_invalid");
  try {var value=new StringBuilder(32768);uint n=(uint)value.Capacity;if(!QueryFullProcessImageNameW(p,0,value,ref n))throw new InvalidOperationException("enrollment_peer_invalid");return value.ToString();}finally{CloseHandle(p);}
 }
 static string LongPath(string path) {
  var expanded=new StringBuilder(32768);
  uint n=GetLongPathNameW(path,expanded,(uint)expanded.Capacity);
  if(n==0||n>=expanded.Capacity)throw new InvalidOperationException("enrollment_image_path_unavailable");
  return expanded.ToString();
 }
 public static bool SameImagePath(string actual,string expected) {
  if(string.Equals(actual,expected,StringComparison.OrdinalIgnoreCase))return true;
  // The kernel may report an 8.3 path for a process started by a long path.
  return string.Equals(LongPath(actual),LongPath(expected),StringComparison.OrdinalIgnoreCase);
 }
 public static uint Parent(uint pid) {
  var p=OpenProcess(0x1000,false,pid);if(p==IntPtr.Zero)throw new InvalidOperationException("enrollment_peer_invalid");
  try {Basic value;uint n;if(NtQueryInformationProcess(p,0,out value,(uint)Marshal.SizeOf(typeof(Basic)),out n)!=0)throw new InvalidOperationException("enrollment_peer_invalid");return checked((uint)value.Parent.ToInt64());}finally{CloseHandle(p);}
 }
 public static string UserSID(uint pid) {
  var p=OpenProcess(0x1000,false,pid);if(p==IntPtr.Zero)throw new InvalidOperationException("enrollment_peer_invalid");
  IntPtr token=IntPtr.Zero,data=IntPtr.Zero;
  try {uint n;if(!OpenProcessToken(p,8,out token))throw new InvalidOperationException("enrollment_peer_invalid");GetTokenInformation(token,1,IntPtr.Zero,0,out n);if(n==0||n>65536)throw new InvalidOperationException("enrollment_peer_invalid");data=Marshal.AllocHGlobal((int)n);if(!GetTokenInformation(token,1,data,n,out n))throw new InvalidOperationException("enrollment_peer_invalid");return new SecurityIdentifier(Marshal.ReadIntPtr(data)).Value;}
  finally{if(data!=IntPtr.Zero)Marshal.FreeHGlobal(data);if(token!=IntPtr.Zero)CloseHandle(token);CloseHandle(p);}
 }
}
}
'@
}

function Assert-CageHostEnrollmentPeer($Context,$Stream) {
    [uint32]$serverPID=0
    if(![Cage.HostEnrollment.Native]::GetNamedPipeServerProcessId($Stream.SafePipeHandle,[ref]$serverPID) -or $serverPID -ne $Context.WorkerPID){throw 'The original host enrollment peer could not be verified.'}
    if([Cage.HostEnrollment.Native]::Started($serverPID) -ne [long]$Context.WorkerStarted -or
        ![Cage.HostEnrollment.Native]::SameImagePath([Cage.HostEnrollment.Native]::Image($serverPID),$Context.WorkerExe) -or
        [Cage.HostEnrollment.Native]::UserSID($serverPID) -cne $Context.HostSID -or
        [Cage.HostEnrollment.Native]::Parent($serverPID) -ne $Context.OwnerPID -or
        [Cage.HostEnrollment.Native]::Started([uint32]$Context.OwnerPID) -ne [long]$Context.OwnerStarted -or
        [Cage.HostEnrollment.Native]::UserSID([uint32]$Context.OwnerPID) -cne $Context.HostSID){throw 'The original host enrollment process changed.'}
}

function Invoke-CageHostEnrollment($Context,[hashtable]$Request,[byte[]]$Secret) {
    $deadline=[DateTime]::UtcNow.AddSeconds(12)
    do {
        $stream=[IO.Pipes.NamedPipeClientStream]::new('.',$Context.Pipe,[IO.Pipes.PipeDirection]::InOut,[IO.Pipes.PipeOptions]::Asynchronous,[Security.Principal.TokenImpersonationLevel]::Identification)
        $reader=$null;$writer=$null
        try {
            $stream.Connect(3000);Assert-CageHostEnrollmentPeer $Context $stream
            $reader=[IO.StreamReader]::new($stream,[Text.UTF8Encoding]::new($false),$false,4096,$true)
            $writer=[IO.StreamWriter]::new($stream,[Text.UTF8Encoding]::new($false),4096,$true);$writer.NewLine="`n";$writer.AutoFlush=$true
            $helloTask=$reader.ReadLineAsync();if(!$helloTask.Wait(10000)){throw 'Original host enrollment handshake timed out.'}
            $line=$helloTask.Result;if(!$line -or $line.Length -gt 16384){throw 'Invalid original host enrollment handshake.'};$hello=$line|ConvertFrom-Json
            if($hello.version -ne 1){throw 'Invalid original host enrollment handshake.'}
            if(!$hello.authorized){Start-Sleep -Milliseconds 100;continue}
            $Request.version=1;$Request.challenge=$hello.challenge;$Request.vm_id=$Context.VMID
            $writer.WriteLine(($Request|ConvertTo-Json -Compress))
            if($Secret){if($Secret.Length -ne 32){throw 'Invalid new pairing key.'};$stream.Write($Secret,0,$Secret.Length);$stream.Flush()}
            $timeout=if($Request.action -ceq 'finalize'){45000}else{10000}
            $replyTask=$reader.ReadLineAsync();if(!$replyTask.Wait($timeout)){throw 'Original host enrollment response timed out.'}
            $line=$replyTask.Result;if(!$line -or $line.Length -gt 16384){throw 'Invalid original host enrollment response.'};$reply=$line|ConvertFrom-Json
            Assert-CageHostEnrollmentPeer $Context $stream
            if($reply.version -ne 1 -or !$reply.ok){throw 'Original host enrollment did not complete; retained files were preserved.'}
            return $reply
        }finally{if($writer){$writer.Dispose()};if($reader){$reader.Dispose()};$stream.Dispose()}
    }while([DateTime]::UtcNow -lt $deadline)
    throw 'The administrator was not registered by the original host process.'
}

function Set-CageHostEnrollmentAdmin($Context,$Process) {
    $target=[Cage.HostEnrollment.Native]::OpenProcess(0x40,$false,[uint32]$Context.WorkerPID)
    if($target -eq [IntPtr]::Zero){throw 'The original enrollment worker is unavailable.'}
    $duplicate=[IntPtr]::Zero
    try {
        if(![Cage.HostEnrollment.Native]::DuplicateHandle([Cage.HostEnrollment.Native]::GetCurrentProcess(),$Process.Handle,$target,[ref]$duplicate,0,$false,2)){throw 'The authorized administrator process handle could not be retained.'}
        $null=Invoke-CageHostEnrollment $Context @{action='allow-admin';pid=[uint32]$Process.Id;process_handle=[uint64]$duplicate.ToInt64()} $null
    }finally{
        # On an ambiguous reply the worker is stopped by its original parent.
        # Closing a numeric handle again here could target a reused handle.
        [void][Cage.HostEnrollment.Native]::CloseHandle($target)
    }
}

function Send-CageHostEnrollmentKey($Context,[string]$Kind,[string]$GuestSID,[byte[]]$Secret) {
    if($Kind -notin @('gui','packet')){throw 'Invalid original host pairing kind.'}
    $null=Invoke-CageHostEnrollment $Context @{action=('deliver-'+$Kind);gui_user_sid=$GuestSID} $Secret
}

function Complete-CageHostEnrollment($Context,[string]$GuestSID) {
    $null=Invoke-CageHostEnrollment $Context @{action='finalize';gui_user_sid=$GuestSID} $null
}

function Stop-CageHostEnrollment($Context) {
    $null=Invoke-CageHostEnrollment $Context @{action='stop'} $null
}

function Wait-CageHostEnrollment($Context, $Worker) {
    $deadline=[DateTime]::UtcNow.AddSeconds(10)
    do {
        if ($Worker -and $Worker.HasExited) {
            throw "enrollment_worker_exited (exit=$($Worker.ExitCode)). Check the enrollment error code in operation details, then continue installation."
        }
        try { $null=Invoke-CageHostEnrollment $Context @{action='ready'} $null; return }
        catch { Start-Sleep -Milliseconds 100 }
    } while ([DateTime]::UtcNow -lt $deadline)
    throw 'The original user enrollment worker did not become available.'
}
