using System.Reflection;
using System.Runtime.CompilerServices;
using System.Security.AccessControl;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;
using System.Diagnostics;

namespace Cage.GuestClipboard;

internal static class WindowsCliRpc
{
    private static readonly Regex ProfileID = new("^[a-f0-9]{16}$",RegexOptions.CultureInvariant);
    private static readonly Regex AccountID = new("^(default|[a-f0-9]{24})$",RegexOptions.CultureInvariant);
    private static readonly Regex Subject = new("^[a-f0-9]{64}$",RegexOptions.CultureInvariant);
    private static readonly Regex CCSProfile = new("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$",RegexOptions.CultureInvariant);
    private static readonly HashSet<string> Modes=["discover","view","rpc","active","handoff","purge","account-switch","account-switch-cancel"];
    private static readonly object Gate=new();
    private static readonly Dictionary<string,int> Active=new(StringComparer.OrdinalIgnoreCase);
    [ModuleInitializer] internal static void Register()
    {
        if(!GuestOperations.Names.Contains("cli"))return;
        try{LoadTools();}catch(Exception){return;}
        GuestOperations.Register("cli",Run);
    }
    private sealed record Tools(string Python,string Claude,string Bash,string? GitHub);
    private sealed record Layout(string Profile,string Root,string Accounts,string State,string Shared,string Code,Tools Tools);
    internal static async Task Run(Stream stream,CancellationToken token,Func<bool> available)
    {
        if(!available())throw new InvalidDataException("requires_gui_login");
        var request=await Protocol.Read(stream,4<<20,token)??throw new EndOfStreamException();
        var profile=request["profile"]?.GetValue<string>()??"";
        if(!ProfileID.IsMatch(profile))throw new InvalidDataException("cli_profile_invalid");
        var layout=Prepare(profile);
        var action=request["action"]?.GetValue<string>()??"";
        if(action=="terminal")
        {
            var kind=request["kind"]?.GetValue<string>()??"";
            if(kind is not ("claude" or "claude-login"))throw new InvalidDataException("cli_kind_invalid");
            var account=request["account"]?.GetValue<string>()??"default";
            if(!AccountID.IsMatch(account)||kind=="claude-login"&&account=="default")throw new InvalidDataException("cli_account_invalid");
            var directory=Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
            var open=new JsonObject{["action"]="open",["cols"]=request["cols"]?.DeepClone()??100,["rows"]=request["rows"]?.DeepClone()??30,["workspace"]="home",["shell"]="claude"};
            var arguments=new List<string>{"-X","utf8","-I"};
            var env=EnvironmentFor(layout,account);
            using var lease=AccountLease(layout,account);
            if(kind=="claude-login")arguments.AddRange([Path.Combine(layout.Code,"cage-claude-account.py"),"login",account]);
            else
            {
                var id=request["project_id"]?.GetValue<string>()??"";var grant=request["grant"]?.GetValue<string>()??"";
                directory=ProjectRpc.Resolve(id,grant);open["workspace"]="project";open["project_id"]=id;open["grant"]=grant;
                var payload=request["conversation"]?.DeepClone() as JsonObject??throw new InvalidDataException("cli_conversation_invalid");
                Guid.Parse(payload["id"]?.GetValue<string>()??"");Guid.Parse(payload["session_id"]?.GetValue<string>()??"");
                ValidatePaths(layout,payload);
                var subject=request["subject"]?.GetValue<string>()??"";
                if(subject!=""&&!Subject.IsMatch(subject))throw new InvalidDataException("cli_identity_invalid");
                payload["account"]=account;payload["subject"]=subject;payload["prefix"]=new JsonArray(layout.Tools.Claude);
                if(request["ccs_profile"] is JsonValue c&&c.TryGetValue<string>(out var ccs)&&ccs!="")
                {
                    if(!CCSProfile.IsMatch(ccs)||account!="default"||subject!="")throw new InvalidDataException("ccs_profile_invalid");
                    var tools=LoadCCSTools();env["CAGE_CCS_NODE"]=tools.Node;env["CAGE_CCS_MAIN"]=tools.Main;
                    env["CAGE_CCS_PRELOAD"]=Path.Combine(layout.Code,"windows_ccs_launch.cjs");
                    env["CCS_CLAUDE_PATH"]=layout.Tools.Claude;
                    env["CCS_DIR"]=Path.Combine(layout.Root,"ccs");env["CCS_HOME"]=Path.Combine(layout.Root,"ccs-home");
                    PrivateDirectory(env["CCS_DIR"]);PrivateDirectory(env["CCS_HOME"]);payload["ccs_profile"]=ccs;
                }
                arguments.AddRange([Path.Combine(layout.Code,"cli_channel.py"),"launch",payload.ToJsonString()]);
            }
            if(!available())throw new InvalidDataException("requires_gui_login");
            using var prefixed=new PrefixStream(stream,Protocol.Utf8.GetBytes(open.ToJsonString()+"\n"));
            await TerminalRpc.RunWithLaunch(prefixed,token,available,new(layout.Tools.Python,arguments.ToArray(),directory,env));
            return;
        }
        string script;string[] args;JsonNode? input=null;var selected="default";
        if(action=="account")
        {
            var mode=request["mode"]?.GetValue<string>()??"";
            if(mode is not ("snapshot" or "usage" or "quotas"))throw new InvalidDataException("cli_account_operation_invalid");
            selected=request["account"]?.GetValue<string>()??"default";
            if(!AccountID.IsMatch(selected))throw new InvalidDataException("cli_account_invalid");
            args=[mode,selected];
            if(mode=="usage"){var subject=request["subject"]?.GetValue<string>()??"";if(!Subject.IsMatch(subject))throw new InvalidDataException("cli_identity_invalid");args=[mode,selected,subject];}
            if(mode=="quotas")
            {
                var ids=request["ids"] as JsonArray??throw new InvalidDataException("cli_account_invalid");
                if(ids.Count is <1 or >100||ids.Any(x=>!AccountID.IsMatch(x?.GetValue<string>()??"")))throw new InvalidDataException("cli_account_invalid");
                args=[mode,..ids.Select(x=>x!.GetValue<string>())];
            }
            script=Path.Combine(layout.Code,"cage-claude-account.py");
        }
        else if(action=="channel")
        {
            var mode=request["mode"]?.GetValue<string>()??"";if(!Modes.Contains(mode))throw new InvalidDataException("cli_channel_operation_invalid");
            input=request["input"]?.DeepClone()??throw new InvalidDataException("cli_channel_input_invalid");
            ValidatePaths(layout,input as JsonObject??throw new InvalidDataException("cli_channel_input_invalid"));
            if(input["config"] is JsonValue c&&c.TryGetValue<string>(out var config)&&config!="")
            {
                var relative=Path.GetRelativePath(layout.Accounts,Path.GetFullPath(config)).Split(Path.DirectorySeparatorChar);
                if(relative.Length==2&&AccountID.IsMatch(relative[0])&&relative[0]!="default"&&relative[1]=="config")selected=relative[0];
            }
            script=Path.Combine(layout.Code,"cli_channel.py");args=[mode,"-"];
        }
        else throw new InvalidDataException("cli_operation_invalid");
        using var accountLease=AccountLease(layout,selected);
        var result=await Invoke(layout,selected,script,args,input,token,available);
        await Protocol.Write(stream,result,token);
    }
    private static Layout Prepare(string profile)
    {
        var tools=LoadTools();
        var root=Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),"Cage","CLI",profile);
        var accounts=Path.Combine(root,"accounts");var state=Path.Combine(root,"channels");var shared=Path.Combine(root,"shared");
        foreach(var path in new[]{root,accounts,state,shared})PrivateDirectory(path);
        string Read(string resource){using var input=Assembly.GetExecutingAssembly().GetManifestResourceStream(resource)??throw new InvalidDataException("cli_helper_missing");using var reader=new StreamReader(input,Protocol.Utf8);return reader.ReadToEnd();}
        var files=new Dictionary<string,string>{{"cli_channel.py",Read("Cage.CliChannel")},{"cage-claude-account.py",Read("Cage.CliAccount")},{"windows_cli_platform.py",Read("Cage.CliPlatform")},{"windows_ccs_launch.cjs",Read("Cage.CliCCS")},{"mobile_cli_identity.py",Read("Cage.CliIdentity")},{"cli_channel.resume.md",Read("Cage.CliResume")},{"cli_channel.account-resume.md",Read("Cage.CliAccountResume")}};
        var digest=Convert.ToHexString(SHA256.HashData(Protocol.Utf8.GetBytes(string.Join("\n",files.Values)))).ToLowerInvariant();
        var code=Path.Combine(root,"code",digest);PrivateDirectory(code);
        foreach(var file in files){var path=Path.Combine(code,file.Key);if(!File.Exists(path))File.WriteAllText(path,file.Value,Protocol.Utf8);else if(File.ReadAllText(path,Protocol.Utf8)!=file.Value)throw new InvalidDataException("cli_helper_changed");}
        return new(profile,root,accounts,state,shared,code,tools);
    }
    private static readonly Dictionary<string,(long Size,DateTime Written,string Hash)> ToolHashes=new(StringComparer.OrdinalIgnoreCase);
    private static Tools LoadTools()
    {
        var toolsFile=Path.Combine(AppContext.BaseDirectory,"cli-tools.json");Pairing.PrivateFile(toolsFile);
        var tool=JsonNode.Parse(File.ReadAllBytes(toolsFile))as JsonObject??throw new InvalidDataException("cli_tools_invalid");
        return new Tools(CheckTool(tool,"python"),CheckTool(tool,"claude"),CheckTool(tool,"git_bash"),tool["gh"] is null?null:CheckTool(tool,"gh"));
    }
    // Used by ordinary terminals as well as Claude, including before a new login.
    internal static Dictionary<string,string> ToolEnvironment()
    {
        var env=Environment.GetEnvironmentVariables().Cast<System.Collections.DictionaryEntry>().ToDictionary(x=>(string)x.Key,x=>(string)x.Value!,StringComparer.OrdinalIgnoreCase);
        try { AddToolPath(env,LoadTools()); }
        catch(Exception error) when(error is IOException or UnauthorizedAccessException or System.Text.Json.JsonException or InvalidOperationException) { }
        return env;
    }
    private static void AddToolPath(Dictionary<string,string> env,Tools tools)
    {
        var paths=new List<string>{Path.GetDirectoryName(tools.Claude)!,Path.Combine(Path.GetDirectoryName(Path.GetDirectoryName(tools.Bash))!,"cmd")};
        if(tools.GitHub is not null)paths.Add(Path.GetDirectoryName(tools.GitHub)!);
        env["PATH"]=string.Join(';',paths)+";"+env.GetValueOrDefault("PATH");
    }
    // Read only fixed tool receipts, without account preparation or execution.
    internal static JsonObject ComponentStatus()
    {
        var dependencies=new JsonObject{["python"]=false,["claude"]=false,["git_bash"]=false};
        try
        {
            var receipt=Path.Combine(AppContext.BaseDirectory,"cli-tools.json");Pairing.PrivateFile(receipt);
            var info=new FileInfo(receipt);if(info.Length is <1 or >16384)throw new InvalidDataException("cli_tools_invalid");
            var tools=JsonNode.Parse(File.ReadAllBytes(receipt))as JsonObject??throw new InvalidDataException("cli_tools_invalid");
            foreach(var name in new[]{"python","claude","git_bash"})
                try{CheckTool(tools,name);dependencies[name]=true;}catch{ }
        }
        catch{ }
        var installed=dependencies.All(pair=>pair.Value!.GetValue<bool>());
        return new JsonObject{["installed"]=installed,["ready"]=installed,["reason"]=installed?"":"cli_tools_missing_or_invalid",["dependencies"]=dependencies};
    }
    private static string CheckTool(JsonObject tool,string name)
    {
            var path=tool[name]?.GetValue<string>()??"";var hash=tool[name+"_sha256"]?.GetValue<string>()??"";
            if(!Path.IsPathFullyQualified(path)||!Subject.IsMatch(hash)||!File.Exists(path)||new FileInfo(path).Attributes.HasFlag(FileAttributes.ReparsePoint))throw new InvalidDataException("cli_tools_unverified");
            var info=new FileInfo(path);
            lock(Gate)
            {
                if(!ToolHashes.TryGetValue(path,out var known)||known.Size!=info.Length||known.Written!=info.LastWriteTimeUtc||known.Hash!=hash)
                {
                    using var file=File.OpenRead(path);
                    if(Convert.ToHexString(SHA256.HashData(file)).ToLowerInvariant()!=hash)throw new InvalidDataException("cli_tools_unverified");
                    ToolHashes[path]=(info.Length,info.LastWriteTimeUtc,hash);
                }
            }
            return Path.GetFullPath(path);
    }
    private sealed record CCSTools(string Node,string Main);
    private static CCSTools LoadCCSTools()
    {
        var receipt=Path.Combine(AppContext.BaseDirectory,"ccs-tools.json");Pairing.PrivateFile(receipt);
        var value=JsonNode.Parse(File.ReadAllBytes(receipt))as JsonObject??throw new InvalidDataException("ccs_tools_invalid");
        string Check(string name)
        {
            var path=value[name]?.GetValue<string>()??"";var hash=value[name+"_sha256"]?.GetValue<string>()??"";
            if(!Path.IsPathFullyQualified(path)||!Subject.IsMatch(hash)||!File.Exists(path)||new FileInfo(path).Attributes.HasFlag(FileAttributes.ReparsePoint))throw new InvalidDataException("ccs_tools_invalid");
            using var data=File.OpenRead(path);if(Convert.ToHexString(SHA256.HashData(data)).ToLowerInvariant()!=hash)throw new InvalidDataException("ccs_tools_changed");
            return Path.GetFullPath(path);
        }
        var node=Check("node");var main=Check("main");var manifest=Check("manifest");
        if(new FileInfo(manifest).Length>2<<20)throw new InvalidDataException("ccs_manifest_invalid");
        var files=System.Text.Json.JsonSerializer.Deserialize<Dictionary<string,string>>(File.ReadAllBytes(manifest))??throw new InvalidDataException("ccs_manifest_invalid");
        if(files.Count is <1 or >20000)throw new InvalidDataException("ccs_manifest_invalid");
        var root=Path.GetDirectoryName(manifest)!;
        foreach(var item in files)
        {
            var path=Path.GetFullPath(Path.Combine(root,item.Key.Replace('/',Path.DirectorySeparatorChar)));
            if(!path.StartsWith(root+Path.DirectorySeparatorChar,StringComparison.OrdinalIgnoreCase)||!Subject.IsMatch(item.Value)||new FileInfo(path).Attributes.HasFlag(FileAttributes.ReparsePoint))throw new InvalidDataException("ccs_manifest_invalid");
            using var data=File.OpenRead(path);if(Convert.ToHexString(SHA256.HashData(data)).ToLowerInvariant()!=item.Value)throw new InvalidDataException("ccs_tools_changed");
        }
        if(!main.StartsWith(root+Path.DirectorySeparatorChar,StringComparison.OrdinalIgnoreCase))throw new InvalidDataException("ccs_tools_invalid");
        return new(node,main);
    }
    private static Dictionary<string,string> EnvironmentFor(Layout layout,string account)
    {
        var env=Environment.GetEnvironmentVariables().Cast<System.Collections.DictionaryEntry>().ToDictionary(x=>(string)x.Key,x=>x.Value?.ToString()??"",StringComparer.OrdinalIgnoreCase);
        env["CAGE_CLI_BINARY"]=layout.Tools.Claude;env["CAGE_CLI_STATE_ROOT"]=layout.State;env["CAGE_CLI_ACCOUNT_ROOT"]=layout.Accounts;env["CAGE_CLI_SHARED_ROOT"]=layout.Shared;
        env["CAGE_CLI_ACCOUNT_HELPER"]=Path.Combine(layout.Code,"cage-claude-account.py");env["CLAUDE_CODE_GIT_BASH_PATH"]=layout.Tools.Bash;
        env["CAGE_CLI_IDENTITY_HELPER"]=Path.Combine(layout.Code,"mobile_cli_identity.py");
        env["CAGE_CLI_PYTHON"]=layout.Tools.Python;env["CAGE_CLI_SHELL"]=TerminalRpc.PowerShellPath();env["CAGE_CLAUDE_ACCOUNT_ID"]=account;
        AddToolPath(env,layout.Tools);
        if(account!="default")env["CLAUDE_CONFIG_DIR"]=Path.Combine(layout.Accounts,account,"config");
        else env.Remove("CLAUDE_CONFIG_DIR");
        return env;
    }
    private static void ValidatePaths(Layout layout,JsonObject value)
    {
        if(value["workspace"] is JsonValue w&&w.TryGetValue<string>(out var workspace)&&workspace!="")
        {
            var resolved=ProjectRpc.Resolve(value["project_id"]?.GetValue<string>()??"",value["grant"]?.GetValue<string>()??"");
            if(!Path.GetFullPath(workspace).Equals(Path.GetFullPath(resolved),StringComparison.OrdinalIgnoreCase))throw new InvalidDataException("cli_project_unregistered");
        }
        if(value["config"]is JsonValue c&&c.TryGetValue<string>(out var config)&&config!="")
        {
            var path=Path.GetFullPath(config);var home=Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile),".claude");
            var relative=Path.GetRelativePath(layout.Accounts,path).Split(Path.DirectorySeparatorChar);
            var ccs=Path.GetRelativePath(layout.Root,path);
            var registeredCCS=(ccs.StartsWith("ccs-home"+Path.DirectorySeparatorChar,StringComparison.OrdinalIgnoreCase)||ccs.StartsWith("ccs"+Path.DirectorySeparatorChar,StringComparison.OrdinalIgnoreCase))&&Directory.Exists(path);
            if(!path.Equals(home,StringComparison.OrdinalIgnoreCase)&&!(relative.Length==2&&AccountID.IsMatch(relative[0])&&relative[0]!="default"&&relative[1]=="config")&&!registeredCCS)throw new InvalidDataException("cli_config_unregistered");
        }
        if(value["resume_transcript"] is JsonValue t&&t.TryGetValue<string>(out var transcript)&&transcript!="")
        {
            var id=Guid.Parse(value["switch_id"]?.GetValue<string>()??"");
            var expected=Path.Combine(layout.Root,"cli-account-switches",id.ToString("D"),"history.jsonl");
            if(!Path.GetFullPath(transcript).Equals(expected,StringComparison.OrdinalIgnoreCase))throw new InvalidDataException("cli_history_unregistered");
        }
    }
    private static async Task<JsonObject> Invoke(Layout layout,string account,string script,string[] args,JsonNode? input,CancellationToken token,Func<bool> available)
    {
        var start=new ProcessStartInfo(layout.Tools.Python){UseShellExecute=false,CreateNoWindow=true,RedirectStandardInput=true,RedirectStandardOutput=true,RedirectStandardError=true};
        foreach(var arg in new[]{"-X","utf8","-I",script}.Concat(args))start.ArgumentList.Add(arg);
        foreach(var pair in EnvironmentFor(layout,account))start.Environment[pair.Key]=pair.Value;
        start.WorkingDirectory=layout.Root;
        using var child=Process.Start(start)??throw new InvalidDataException("cli_process_failed");
        using var cancel=token.Register(()=>{try{child.Kill(true);}catch{}});
        var stderr=child.StandardError.BaseStream.CopyToAsync(Stream.Null,token);
        try
        {
            if(input is not null)await child.StandardInput.WriteLineAsync(input.ToJsonString().AsMemory(),token);child.StandardInput.Close();
            using var output=new MemoryStream();var buffer=new byte[16384];int size;
            while((size=await child.StandardOutput.BaseStream.ReadAsync(buffer,token))!=0)
            {
                if(output.Length+size>4<<20)throw new InvalidDataException("cli_status_limit");
                output.Write(buffer,0,size);
            }
            var raw=Protocol.Utf8.GetString(output.GetBuffer(),0,(int)output.Length);await child.WaitForExitAsync(token);await stderr;
            if(!available()||child.ExitCode!=0)throw new InvalidDataException("cli_status_unavailable");
            var parsed=JsonNode.Parse(raw);
            return new JsonObject{["result"]=parsed};
        }
        finally
        {
            if(!child.HasExited)try{child.Kill(true);}catch{}
            await child.WaitForExitAsync(CancellationToken.None);
            try{await stderr;}catch(OperationCanceledException){}
        }
    }
    private static void PrivateDirectory(string path)
    {
        var home=Path.GetFullPath(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile));
        if(!Path.GetFullPath(path).StartsWith(home+Path.DirectorySeparatorChar,StringComparison.OrdinalIgnoreCase))throw new InvalidDataException("cli_directory_invalid");
        for(var ancestor=new DirectoryInfo(path);ancestor.FullName!=home;ancestor=ancestor.Parent??throw new InvalidDataException("cli_directory_invalid"))
            if(ancestor.Exists&&ancestor.Attributes.HasFlag(FileAttributes.ReparsePoint))throw new InvalidDataException("cli_directory_invalid");
        Directory.CreateDirectory(path);var info=new DirectoryInfo(path);
        if(info.Attributes.HasFlag(FileAttributes.ReparsePoint))throw new InvalidDataException("cli_directory_invalid");
        using var user=WindowsIdentity.GetCurrent();var acl=new DirectorySecurity();acl.SetOwner(user.User!);acl.SetAccessRuleProtection(true,false);
        foreach(var sid in new[]{user.User!,new SecurityIdentifier("S-1-5-18"),new SecurityIdentifier("S-1-5-32-544")})acl.AddAccessRule(new(sid,FileSystemRights.FullControl,InheritanceFlags.ContainerInherit|InheritanceFlags.ObjectInherit,PropagationFlags.None,AccessControlType.Allow));
        info.SetAccessControl(acl);
    }
    private static byte[] PrivateData(string path)
    {
        var info=new FileInfo(path);
        if(!info.Exists||info.Length is <1 or >16384||info.Attributes.HasFlag(FileAttributes.ReparsePoint))throw new InvalidDataException("cli_credentials_invalid");
        using var user=WindowsIdentity.GetCurrent();var allowed=new HashSet<string>{user.User!.Value,"S-1-5-18","S-1-5-32-544"};
        foreach(FileSystemAccessRule rule in info.GetAccessControl().GetAccessRules(true,true,typeof(SecurityIdentifier)))
            if(rule.AccessControlType==AccessControlType.Allow&&!allowed.Contains(rule.IdentityReference.Value))throw new InvalidDataException("cli_credentials_permissions_invalid");
        return File.ReadAllBytes(path);
    }
    private static IDisposable AccountLease(Layout layout,string account)
    {
        if(account=="default")return new Release(()=>{});
        var folder=Path.Combine(layout.Accounts,account,"config");PrivateDirectory(folder);
        var plain=Path.Combine(folder,".credentials.json");var encrypted=Path.Combine(folder,"credentials.dpapi");
        lock(Gate)
        {
            if(!File.Exists(plain)&&File.Exists(encrypted))
            {
                var restored=Pairing.Crypt(PrivateData(encrypted),GuestContext.VM,GuestContext.SID+":cli:"+layout.Profile+":"+account,false);
                try{using var file=new FileStream(plain,FileMode.CreateNew,FileAccess.Write,FileShare.None);file.Write(restored);file.Flush(true);}finally{CryptographicOperations.ZeroMemory(restored);}
            }
            Active[folder]=Active.GetValueOrDefault(folder)+1;
        }
        return new Release(()=>{lock(Gate){if(--Active[folder]!=0)return;Active.Remove(folder);if(File.Exists(plain)){var bytes=PrivateData(plain);try{var sealedBytes=Pairing.Crypt(bytes,GuestContext.VM,GuestContext.SID+":cli:"+layout.Profile+":"+account,true);var temporary=encrypted+"."+Guid.NewGuid().ToString("N");using(var file=new FileStream(temporary,FileMode.CreateNew,FileAccess.Write,FileShare.None)){file.Write(sealedBytes);file.Flush(true);}File.Move(temporary,encrypted,true);File.Delete(plain);}finally{CryptographicOperations.ZeroMemory(bytes);}}}});
    }
    private sealed class Release(Action action):IDisposable{private Action? close=action;public void Dispose()=>Interlocked.Exchange(ref close,null)?.Invoke();}
    private sealed class PrefixStream(Stream inner,byte[] prefix):Stream
    {
        int position;
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken token=default){if(position<prefix.Length){var n=Math.Min(buffer.Length,prefix.Length-position);prefix.AsMemory(position,n).CopyTo(buffer);position+=n;return n;}return await inner.ReadAsync(buffer,token);}
        public override int Read(byte[] buffer,int offset,int count){if(position<prefix.Length){var n=Math.Min(count,prefix.Length-position);Array.Copy(prefix,position,buffer,offset,n);position+=n;return n;}return inner.Read(buffer,offset,count);}
        public override Task WriteAsync(byte[] buffer,int offset,int count,CancellationToken token)=>inner.WriteAsync(buffer,offset,count,token);
        public override ValueTask WriteAsync(ReadOnlyMemory<byte> buffer,CancellationToken token=default)=>inner.WriteAsync(buffer,token);
        public override void Write(byte[] buffer,int offset,int count)=>inner.Write(buffer,offset,count);public override void Flush()=>inner.Flush();public override Task FlushAsync(CancellationToken token)=>inner.FlushAsync(token);
        public override bool CanRead=>true;public override bool CanWrite=>true;public override bool CanSeek=>false;public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}public override long Seek(long a,SeekOrigin b)=>throw new NotSupportedException();public override void SetLength(long a)=>throw new NotSupportedException();
    }
}
