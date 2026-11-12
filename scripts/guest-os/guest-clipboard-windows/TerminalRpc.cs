using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json.Nodes;
using Microsoft.Win32.SafeHandles;

namespace Cage.GuestClipboard;

// The authenticated GUI relay owns this terminal. Commands and project paths are
// never accepted from the wire; a future project grant must be resolved here.
internal static class TerminalRpc
{
    internal sealed record Launch(string Executable, string[] Arguments, string Directory, Dictionary<string, string> Environment);
    private static readonly SemaphoreSlim Slots = new(12, 12);
    [ModuleInitializer]
    internal static void Register() => GuestOperations.Register("terminal", Run);
    internal static string PowerShellPath()
    {
        var pwsh=Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles),"PowerShell","7","pwsh.exe");
        return File.Exists(pwsh)?pwsh:Path.Combine(Environment.SystemDirectory,"WindowsPowerShell","v1.0","powershell.exe");
    }
    internal static Task Run(Stream stream, CancellationToken token, Func<bool> sessionAvailable) => RunCore(stream, token, sessionAvailable, null);
    internal static Task RunWithLaunch(Stream stream, CancellationToken token, Func<bool> sessionAvailable, Launch launch) => RunCore(stream, token, sessionAvailable, launch);
    private static async Task RunCore(Stream stream, CancellationToken token, Func<bool> sessionAvailable, Launch? launch)
    {
        if (!sessionAvailable()) throw new InvalidDataException("requires_gui_login");
        if (!await Slots.WaitAsync(0, token)) throw new InvalidDataException("terminal_limit");
        using var lifetime = CancellationTokenSource.CreateLinkedTokenSource(token);
        using var writes = new SemaphoreSlim(1, 1);
        NativeTerminal? terminal = null;
        Task? output = null, inputs = null, guard = null;
        Task<int>? exited = null;
        Action<string, string>? revoked = null;
        Action? projectAvailable = null;
        async Task Send(JsonObject value)
        {
            await writes.WaitAsync(lifetime.Token);
            try { await Protocol.Write(stream, value, lifetime.Token); } finally { writes.Release(); }
        }
        try
        {
            var open = await Protocol.Read(stream, 65536, lifetime.Token) ?? throw new EndOfStreamException();
            if (open["action"]?.GetValue<string>() != "open" || open["shell"]?.GetValue<string>() != (launch is null ? "powershell" : "claude") || !sessionAvailable())
                throw new InvalidDataException("terminal_request_invalid");
            var directory = open["workspace"]?.GetValue<string>() switch
            {
                "home" => Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),
                "project" => ProjectRpc.Resolve(open["project_id"]?.GetValue<string>() ?? "", open["grant"]?.GetValue<string>() ?? ""),
                _ => throw new InvalidDataException("terminal_request_invalid")
            };
            if (launch is not null && !Path.GetFullPath(launch.Directory).Equals(Path.GetFullPath(directory), StringComparison.OrdinalIgnoreCase)) throw new InvalidDataException("terminal_directory_invalid");
            if (open["workspace"]?.GetValue<string>() == "project")
            {
                var id = open["project_id"]!.GetValue<string>(); var grant = open["grant"]!.GetValue<string>();
                revoked = (removed, oldGrant) => { if (removed == id && oldGrant == grant) lifetime.Cancel(); };
                ProjectRpc.Revoked += revoked;
                projectAvailable = () => ProjectRpc.Resolve(id, grant);
                projectAvailable();
            }
            terminal = new NativeTerminal(Size(open, "cols"), Size(open, "rows"), directory, launch);
            await Send(new JsonObject { ["ready"] = true });
            output = Task.Run(async () =>
            {
                var buffer = new byte[32 << 10]; int count;
                // ConPTY shutdown needs a live drain even after the viewer stops.
                while ((count = await terminal.Output.ReadAsync(buffer, CancellationToken.None)) != 0)
                {
                    if (lifetime.IsCancellationRequested) continue;
                    try { await Send(new JsonObject { ["data"] = Convert.ToBase64String(buffer, 0, count) }); }
                    catch { lifetime.Cancel(); }
                }
            });
            inputs = Task.Run(async () =>
            {
                while (await Protocol.Read(stream, 100 << 10, lifetime.Token) is { } value)
                {
                    if (!sessionAvailable()) throw new InvalidDataException("requires_gui_login");
                    lifetime.Token.ThrowIfCancellationRequested(); projectAvailable?.Invoke();
                    switch (value["action"]?.GetValue<string>())
                    {
                        case "input":
                            var bytes = Convert.FromBase64String(value["data"]?.GetValue<string>() ?? "");
                            if (bytes.Length > 64 << 10) throw new InvalidDataException("terminal_input_limit");
                            await terminal.Input.WriteAsync(bytes, lifetime.Token); await terminal.Input.FlushAsync(lifetime.Token); break;
                        case "resize": terminal.Resize(Size(value, "cols"), Size(value, "rows")); break;
                        case "close": return;
                        default: throw new InvalidDataException("terminal_request_invalid");
                    }
                }
            }, lifetime.Token);
            guard = Task.Run(async () =>
            {
                while (!lifetime.IsCancellationRequested)
                {
                    await Task.Delay(100, lifetime.Token);
                    if (!sessionAvailable()) throw new InvalidDataException("requires_gui_login");
                }
            }, lifetime.Token);
            exited = Task.Run(terminal.Wait);
            var first = await Task.WhenAny(inputs, exited, guard, output);
            if (first == exited || first == output)
            {
                if (first == output) { await output; terminal.KillOwnJob(); }
                var code = await exited;
                terminal.KillOwnJob();
                terminal.CloseConsole();
                await output;
                await Send(new JsonObject { ["exit"] = code });
            }
            else await first;
        }
        finally
        {
            lifetime.Cancel(); terminal?.Stop();
            if (revoked is not null) ProjectRpc.Revoked -= revoked;
            foreach (var task in new Task?[] { inputs, output, guard, exited })
                if (task is not null) try { await task; } catch { /* teardown observes every worker */ }
            terminal?.Dispose(); Slots.Release();
        }
    }
    private static short Size(JsonObject value, string name)
    {
        var size = value[name]?.GetValue<int>() ?? 0;
        var maximum = name == "cols" ? 599 : 500;
        if (size < 1 || size > maximum) throw new InvalidDataException("terminal_size_invalid");
        return (short)size;
    }

    private sealed class NativeTerminal : IDisposable
    {
        internal FileStream Input { get; }
        internal FileStream Output { get; }
        private nint console, process, job;
        private readonly object gate = new();
        private int disposed;
        internal NativeTerminal(short cols, short rows, string directory, Launch? launch)
        {
            SafeFileHandle? inputRead = null, inputWrite = null, outputRead = null, outputWrite = null;
            nint attributes = 0, environment = 0;
            try
            {
                Check(CreatePipe(out inputRead, out inputWrite, 0, 0));
                Check(CreatePipe(out outputRead, out outputWrite, 0, 0));
                Marshal.ThrowExceptionForHR(CreatePseudoConsole(new Coord(cols, rows), inputRead, outputWrite, 0, out console));
                nuint bytes = 0; InitializeProcThreadAttributeList(0, 1, 0, ref bytes);
                attributes = Marshal.AllocHGlobal((nint)bytes);
                Check(InitializeProcThreadAttributeList(attributes, 1, 0, ref bytes));
                Check(UpdateProcThreadAttribute(attributes, 0, 0x00020016, console, (nuint)IntPtr.Size, 0, 0));
                var startup = new StartupInfoEx(); startup.Startup.cb = Marshal.SizeOf<StartupInfoEx>();
                startup.Startup.flags = 0x100; startup.Attributes = attributes;
                var shell = PowerShellPath();
                var arguments = new[] { "-NoLogo" };
                if (launch is not null)
                {
                    shell = launch.Executable; arguments = launch.Arguments;
                }
                var variables = launch?.Environment ?? WindowsCliRpc.ToolEnvironment();
                environment = Marshal.StringToHGlobalUni(string.Join('\0', variables.OrderBy(x => x.Key, StringComparer.OrdinalIgnoreCase).Select(x => x.Key + "=" + x.Value)) + "\0\0");
                if (!File.Exists(shell)) throw new InvalidDataException("terminal_shell_missing");
                job = CreateJobObject(0, null); Check(job != 0);
                var limits = new JobLimits(); limits.Basic.flags = 0x2000; // KILL_ON_JOB_CLOSE
                Check(SetInformationJobObject(job, 9, ref limits, (uint)Marshal.SizeOf<JobLimits>()));
                Check(CreateProcess(shell, new StringBuilder(string.Join(' ', new[] { shell }.Concat(arguments).Select(QuoteArgument))), 0, 0, false,
                    0x00080000 | 0x00000400 | 0x00000004, environment, directory, ref startup, out var created));
                process = created.Process;
                try
                {
                    Check(AssignProcessToJobObject(job, process));
                    if (ResumeThread(created.Thread) == uint.MaxValue) throw new Win32Exception();
                }
                catch { TerminateProcess(process, 1); throw; }
                finally { CloseHandle(created.Thread); }
                Input = new FileStream(inputWrite, FileAccess.Write, 65536, false); inputWrite = null;
                Output = new FileStream(outputRead, FileAccess.Read, 65536, false); outputRead = null;
            }
            catch { Dispose(); throw; }
            finally
            {
                inputRead?.Dispose(); inputWrite?.Dispose(); outputRead?.Dispose(); outputWrite?.Dispose();
                if (attributes != 0) { DeleteProcThreadAttributeList(attributes); Marshal.FreeHGlobal(attributes); }
                if (environment != 0) Marshal.FreeHGlobal(environment);
            }
        }
        internal void Resize(short cols, short rows) { lock (gate) { if (console == 0) throw new ObjectDisposedException(nameof(NativeTerminal)); Marshal.ThrowExceptionForHR(ResizePseudoConsole(console, new Coord(cols, rows))); } }
        internal int Wait()
        {
            var handle = process;
            if (handle == 0 || WaitForSingleObject(handle, uint.MaxValue) != 0 || !GetExitCodeProcess(handle, out var code)) throw new Win32Exception();
            return unchecked((int)code);
        }
        internal void CloseConsole() { lock (gate) { if (console != 0) { ClosePseudoConsole(console); console = 0; } } }
        internal void KillOwnJob() { lock (gate) { if (job != 0) { CloseHandle(job); job = 0; } } }
        internal void Stop()
        {
            if (Interlocked.Exchange(ref disposed, 1) != 0) return;
            KillOwnJob();
            Input?.Dispose();
            // Keep output readable until ConPTY flushes; the active output pump drains it.
            CloseConsole(); Output?.Dispose();
        }
        public void Dispose()
        {
            Stop();
            if (process != 0) { CloseHandle(process); process = 0; }
        }
        private static void Check(bool value) { if (!value) throw new Win32Exception(Marshal.GetLastWin32Error()); }
    }
    internal static string QuoteArgument(string value)
    {
        var result = new StringBuilder("\""); int slashes = 0;
        foreach (var c in value)
        {
            if (c == '\\') { slashes++; continue; }
            result.Append('\\', c == '"' ? slashes * 2 + 1 : slashes); result.Append(c); slashes = 0;
        }
        return result.Append('\\', slashes * 2).Append('"').ToString();
    }
    [StructLayout(LayoutKind.Sequential)] private readonly struct Coord(short x, short y) { internal readonly short X = x, Y = y; }
    [StructLayout(LayoutKind.Sequential)] private struct StartupInfo { internal int cb; internal nint reserved, desktop, title; internal int x,y,xSize,ySize,xChars,yChars,fill,flags; internal short show,reserved2Size; internal nint reserved2,input,output,error; }
    [StructLayout(LayoutKind.Sequential)] private struct StartupInfoEx { internal StartupInfo Startup; internal nint Attributes; }
    [StructLayout(LayoutKind.Sequential)] private struct ProcessInfo { internal nint Process, Thread; internal uint ProcessId, ThreadId; }
    [StructLayout(LayoutKind.Sequential)] private struct BasicLimits { internal long processTime,jobTime; internal uint flags; internal nuint minWorkingSet,maxWorkingSet; internal uint activeProcessLimit; internal nuint affinity; internal uint priority,scheduling; }
    [StructLayout(LayoutKind.Sequential)] private struct IoCounters { internal ulong readOperations,writeOperations,otherOperations,readBytes,writeBytes,otherBytes; }
    [StructLayout(LayoutKind.Sequential)] private struct JobLimits { internal BasicLimits Basic; internal IoCounters Io; internal nuint processMemory,jobMemory,peakProcessMemory,peakJobMemory; }
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool CreatePipe(out SafeFileHandle read,out SafeFileHandle write,nint attributes,uint size);
    [DllImport("kernel32.dll")] private static extern int CreatePseudoConsole(Coord size,SafeFileHandle input,SafeFileHandle output,uint flags,out nint console);
    [DllImport("kernel32.dll")] private static extern int ResizePseudoConsole(nint console,Coord size);
    [DllImport("kernel32.dll")] private static extern void ClosePseudoConsole(nint console);
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool InitializeProcThreadAttributeList(nint list,int count,uint flags,ref nuint size);
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool UpdateProcThreadAttribute(nint list,uint flags,nuint attribute,nint value,nuint size,nint previous,nint returned);
    [DllImport("kernel32.dll")] private static extern void DeleteProcThreadAttributeList(nint list);
    [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool CreateProcess(string app,StringBuilder command,nint processAttributes,nint threadAttributes,bool inherit,uint flags,nint environment,string directory,ref StartupInfoEx startup,out ProcessInfo process);
    [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] private static extern nint CreateJobObject(nint attributes,string? name);
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool SetInformationJobObject(nint job,int kind,ref JobLimits limits,uint size);
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool AssignProcessToJobObject(nint job,nint process);
    [DllImport("kernel32.dll",SetLastError=true)] private static extern uint ResumeThread(nint thread);
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool TerminateProcess(nint process,uint code);
    [DllImport("kernel32.dll",SetLastError=true)] private static extern uint WaitForSingleObject(nint handle,uint milliseconds);
    [DllImport("kernel32.dll",SetLastError=true)] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool GetExitCodeProcess(nint process,out uint code);
    [DllImport("kernel32.dll")] [return:MarshalAs(UnmanagedType.Bool)] private static extern bool CloseHandle(nint handle);
}
