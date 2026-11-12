using System.Diagnostics;
using System.Runtime.CompilerServices;
using System.Security.AccessControl;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text.Json.Nodes;

namespace Cage.GuestClipboard;

internal static class GuestVersions
{
    [ModuleInitializer]
    internal static void Register() => GuestOperations.Register("versions", Read);

    private static async Task Read(Stream stream, CancellationToken token, Func<bool> available)
    {
        var result = Snapshot();
        var tools = new JsonObject();
        tools["node"] = await Version(Path.Combine(AppContext.BaseDirectory, "node.exe"), token);
        tools["code_server"] = await Version(Path.Combine(AppContext.BaseDirectory, "vscode-cli", "code.exe"), token);
        var chrome = new[] {
            Path.Combine(AppContext.BaseDirectory, "chrome", "chrome.exe"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Google", "Chrome", "Application", "chrome.exe"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86), "Microsoft", "Edge", "Application", "msedge.exe")
        }.FirstOrDefault(File.Exists);
        tools["chromium"] = chrome is null ? "" : FileVersionInfo.GetVersionInfo(chrome).ProductVersion ?? "";
        try
        {
            var receipt = Path.Combine(AppContext.BaseDirectory, "cli-tools.json");
            Pairing.PrivateFile(receipt);
            var cli = JsonNode.Parse(File.ReadAllBytes(receipt)) as JsonObject;
            foreach (var name in new[] { "python", "claude", "git_bash", "gh" })
            {
                var filename = cli?[name]?.GetValue<string>();
                var expected = cli?[name + "_sha256"]?.GetValue<string>();
                if (filename is null || expected is null || !Path.IsPathRooted(filename)) continue;
                using (var file = File.OpenRead(filename))
                    if (!Convert.ToHexString(SHA256.HashData(file)).Equals(expected, StringComparison.OrdinalIgnoreCase)) continue;
                if (name == "git_bash")
                    tools["git"] = await Version(Path.Combine(Path.GetDirectoryName(Path.GetDirectoryName(filename))!, "cmd", "git.exe"), token);
                else tools[name] = await Version(filename, token);
            }
        }
        catch (IOException) { }
        catch (UnauthorizedAccessException) { }
        catch (System.Text.Json.JsonException) { }
        result["tools"] = tools;
        await Protocol.Write(stream, result, token);
    }

    internal static JsonObject Snapshot()
    {
        var result = new JsonObject { ["manifest"] = null, ["installed"] = false, ["missing"] = new JsonArray() };
        try
        {
            var path = Path.Combine(AppContext.BaseDirectory, "native-components.json");
            CheckReceipt(path);
            var manifest = JsonNode.Parse(File.ReadAllText(path)) as JsonObject;
            if (manifest?["protocol"]?.GetValue<int>() != 1 || manifest["system"]?.GetValue<string>() != "windows-11" || manifest["files"] is not JsonArray files || files.Count > 10000)
                throw new InvalidDataException("component_manifest_invalid");
            var missing = new JsonArray();
            foreach (var entry in files.OfType<JsonObject>())
            {
                var target = entry["target"]?.GetValue<string>() ?? "";
                if (target.Length == 0) continue;
                var filename = Target(target);
                try
                {
                    using var file = File.OpenRead(filename);
                    var hash = Convert.ToHexString(SHA256.HashData(file));
                    if (!hash.Equals(entry["installed_sha256"]?.GetValue<string>(), StringComparison.OrdinalIgnoreCase) && missing.Count < 20) missing.Add(target);
                }
                catch (IOException) { if (missing.Count < 20) missing.Add(target); }
            }
            result["manifest"] = new JsonObject {
                ["protocol"] = 1, ["revision"] = missing.Count == 0 ? manifest["revision"]?.DeepClone() : "",
                ["app_version"] = manifest["app_version"]?.DeepClone()
            };
            result["installed"] = missing.Count == 0;
            result["missing"] = missing;
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or System.Text.Json.JsonException or InvalidOperationException)
        {
            result["error"] = "客机尚无可核验的组件记录，请同步环境组件。";
        }
        return result;
    }

    private static void CheckReceipt(string path)
    {
        var file = new FileInfo(path);
        if (!file.Exists || file.Length is < 1 or > 4 << 20 || file.Attributes.HasFlag(FileAttributes.ReparsePoint))
            throw new InvalidDataException("component_manifest_invalid");
        for (var parent = file.Directory; parent is not null; parent = parent.Parent)
            if (parent.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("component_manifest_invalid");
        var acl = file.GetAccessControl();
        var administrators = new HashSet<string> { "S-1-5-18", "S-1-5-32-544" };
        var owner = acl.GetOwner(typeof(SecurityIdentifier))?.Value;
        if (!acl.AreAccessRulesProtected || owner is null || !administrators.Contains(owner))
            throw new InvalidDataException("component_permissions_invalid");
        var read = FileSystemRights.ReadAndExecute | FileSystemRights.Synchronize;
        foreach (FileSystemAccessRule rule in acl.GetAccessRules(true, true, typeof(SecurityIdentifier)))
        {
            if (rule.AccessControlType != AccessControlType.Allow || administrators.Contains(rule.IdentityReference.Value)) continue;
            if (rule.IdentityReference.Value != GuestContext.SID || (rule.FileSystemRights & ~read) != 0)
                throw new InvalidDataException("component_permissions_invalid");
        }
    }

    private static string Target(string target)
    {
        var parts = target.Split('/');
        if (parts.Length < 2 || parts.Any(part => part.Length == 0 || part is "." or ".." || part.Contains('\\') || part.Contains(':')))
            throw new InvalidDataException("component_target_invalid");
        var root = parts[0] switch {
            "bridge" => AppContext.BaseDirectory,
            "network" => Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Cage Guest Network", GuestContext.VM.ToString("D")),
            _ => throw new InvalidDataException("component_target_invalid")
        };
        return Path.Combine([root, .. parts.Skip(1)]);
    }

    private static async Task<string> Version(string executable, CancellationToken parent)
    {
        if (!File.Exists(executable)) return "";
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(parent);
        timeout.CancelAfter(TimeSpan.FromSeconds(3));
        var start = new ProcessStartInfo(executable) { UseShellExecute = false, CreateNoWindow = true, RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true };
        start.ArgumentList.Add("--version");
        start.Environment["DISABLE_AUTOUPDATER"] = "1";
        Process? launched;
        try { launched = Process.Start(start); }
        catch (Exception error) when (error is System.ComponentModel.Win32Exception or IOException) { return ""; }
        using var process = launched;
        if (process is null) return "";
        process.StandardInput.Close();
        using var cancel = timeout.Token.Register(() => {
            try { process.Kill(true); }
            catch (Exception error) when (error is InvalidOperationException or System.ComponentModel.Win32Exception) { }
        });
        try
        {
            var errors = process.StandardError.BaseStream.CopyToAsync(Stream.Null, timeout.Token);
            var output = new char[512];
            var count = await process.StandardOutput.ReadAsync(output, timeout.Token);
            await process.StandardOutput.BaseStream.CopyToAsync(Stream.Null, timeout.Token);
            await process.WaitForExitAsync(timeout.Token);
            await errors;
            var line = new string(output, 0, count).Split('\n')[0].Trim();
            return process.ExitCode == 0 ? line[..Math.Min(line.Length, 200)] : "";
        }
        catch (OperationCanceledException) { return ""; }
    }
}
