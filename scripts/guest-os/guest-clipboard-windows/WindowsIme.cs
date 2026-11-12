using System.Security.Cryptography;
using System.Text.Json.Nodes;
using System.Windows.Automation;
using System.Runtime.InteropServices;

namespace Cage.GuestClipboard;

// The Claude window owner supplies a verified process and a Unicode committer.
// UI Automation is read-only here; it never chooses an arbitrary application.
internal static class WindowsIme
{
    [StructLayout(LayoutKind.Sequential)] private struct CursorInfo { public int Size, Flags; public IntPtr Handle; public System.Drawing.Point Point; }
    [DllImport("user32.dll")] private static extern bool GetCursorInfo(ref CursorInfo info);
    [DllImport("user32.dll")] private static extern IntPtr LoadCursorW(IntPtr instance, IntPtr name);
    private static string CursorShape(int process)
    {
        var info = new CursorInfo { Size = Marshal.SizeOf<CursorInfo>() };
        if (!GetCursorInfo(ref info)) return "default";
        if (info.Handle == LoadCursorW(IntPtr.Zero, (IntPtr)32513)) return "text";
        if (info.Handle == LoadCursorW(IntPtr.Zero, (IntPtr)32649)) return "pointer";
        // Chromium may use its own I-beam handle. Read only the owned
        // application element under the guest pointer in that case.
        var element = AutomationElement.FromPoint(new System.Windows.Point(info.Point.X, info.Point.Y));
        if (element?.Current.ProcessId == process && element.Current.IsEnabled && !element.Current.IsOffscreen &&
            element.Current.ControlType == ControlType.Edit) return "text";
        return "default";
    }
    private sealed record Focus(string Context, double[] Rect, bool Password);
    private sealed record Composition(string ID, string Context, int Sequence, DateTime Updated);
    private static readonly SemaphoreSlim Probes = new(1, 1);
    private static Composition? composition;

    private static Focus? Probe(int process, string windowTarget, double x, double y)
    {
        var element = AutomationElement.FocusedElement;
        if (element is null || element.Current.ProcessId != process || !element.Current.IsEnabled || element.Current.IsOffscreen)
            return null;
        bool editable = element.TryGetCurrentPattern(ValuePattern.Pattern, out var value) && !((ValuePattern)value).Current.IsReadOnly;
        if (!editable && element.TryGetCurrentPattern(TextPattern.Pattern, out var text))
        {
            var readOnly = ((TextPattern)text).DocumentRange.GetAttributeValue(TextPattern.IsReadOnlyAttribute);
            editable = readOnly is bool b && !b;
        }
        if (!editable) return null;
        var bounds = element.Current.BoundingRectangle;
        var rect = new double[] { bounds.X-x, bounds.Y-y, Math.Max(1,bounds.Width), Math.Max(1,bounds.Height) };
        if (element.TryGetCurrentPattern(TextPattern.Pattern, out var caret))
        {
            var ranges = ((TextPattern)caret).GetSelection();
            var geometry = ranges.Length == 1 ? ranges[0].GetBoundingRectangles() : [];
            if (geometry.Length != 0) rect = [geometry[0].X-x,geometry[0].Y-y,Math.Max(1,geometry[0].Width),Math.Max(1,geometry[0].Height)];
        }
        var identity = windowTarget + ":" + string.Join(',',element.GetRuntimeId());
        var context = Convert.ToHexString(SHA256.HashData(Protocol.Utf8.GetBytes(identity)).AsSpan(0,16)).ToLowerInvariant();
        return new Focus(context,rect,element.Current.IsPassword);
    }

    internal static async Task RequirePhoneFocus(JsonObject input, int process, string windowTarget, CancellationToken token)
    {
        if (!input.ContainsKey("phone_focus")) return;
        var requested = input["phone_focus"]?.GetValue<string>() ?? "";
        var focus = await Handle(new JsonObject { ["mode"] = "status" }, process, windowTarget, 0, 0,
            _ => throw new InvalidDataException("desktop_phone_focus_changed"), token);
        if (requested.Length == 0 || focus["available"]?.GetValue<bool>() != true ||
            focus["password"]?.GetValue<bool>() == true || focus["context"]?.GetValue<string>() != requested)
            throw new InvalidDataException("desktop_phone_focus_changed");
    }

    internal static async Task<JsonObject> Handle(JsonObject input, int process, string windowTarget, double x, double y, Action<string> commit, CancellationToken token)
    {
        var mode = input["mode"]?.GetValue<string>() ?? "";
        var id = input["session"]?.GetValue<string>() ?? "";
        var sequence = input["sequence"]?.GetValue<int>() ?? 0;
        if (mode is not ("status" or "begin" or "update" or "commit" or "cancel")) throw new InvalidDataException("input_mode_invalid");
        if (mode != "status" && (id.Length != 32 || id.Any(c => !char.IsAsciiHexDigit(c)) || sequence < 0)) throw new InvalidDataException("input_session_invalid");
        if (mode == "cancel")
        {
            if (composition?.ID == id && sequence > composition.Sequence) composition = null;
            return new JsonObject { ["available"] = false };
        }
        if (!await Probes.WaitAsync(0,token)) return new JsonObject { ["available"] = false };
        var cursor = "default";
        var probe = Task.Run(() => {
            try {
                try { cursor = CursorShape(process); } catch { }
                return Probe(process,windowTarget,x,y);
            } finally { Probes.Release(); }
        });
        Focus? focus;
        try { focus = await probe.WaitAsync(TimeSpan.FromSeconds(2),token); }
        catch (Exception e) when (e is not OperationCanceledException) { return new JsonObject { ["available"] = false }; }
        if (focus is null || !SessionGuard.Available()) { composition = null; return new JsonObject { ["available"] = false, ["cursor"] = cursor }; }
        if (composition is not null && (composition.Context != focus.Context || DateTime.UtcNow-composition.Updated > TimeSpan.FromSeconds(30))) composition = null;
        JsonObject Snapshot() => new() { ["available"] = true, ["cursor"] = cursor, ["context"] = focus.Context, ["rect"] = new JsonArray(focus.Rect.Select(v => (JsonNode?)JsonValue.Create(v)).ToArray()), ["preedit_supported"] = false, ["password"] = focus.Password };
        if (mode == "status") { if (composition?.ID == id) composition = composition with { Updated = DateTime.UtcNow }; return Snapshot(); }
        if (mode == "begin")
        {
            if (input["context"]?.GetValue<string>() != focus.Context) return new JsonObject { ["available"] = false };
            composition = new Composition(id,focus.Context,sequence,DateTime.UtcNow); return Snapshot();
        }
        if (composition?.ID != id || sequence <= composition.Sequence) return new JsonObject { ["available"] = false };
        var content = input["text"]?.GetValue<string>() ?? "";
        if (Protocol.Utf8.GetByteCount(content) > 8192) throw new InvalidDataException("input_text_limit");
        if (mode == "commit") { commit(content); composition = null; }
        else composition = composition with { Sequence = sequence, Updated = DateTime.UtcNow };
        return Snapshot();
    }
}
