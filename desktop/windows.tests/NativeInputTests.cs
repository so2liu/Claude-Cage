using System.ComponentModel;
using System.Reflection;
using System.Runtime.ExceptionServices;
using System.Runtime.InteropServices;
using System.Text.Json;
using Cage.Windows;

internal static class NativeInputTests
{
    // SetKeyboardState affects only this thread's input-state table. No SendInput,
    // input-queue attachment, visible window, or keyboard LED changes are used.
    internal static void Run()
    {
        Exception? failure = null;
        var thread = new Thread(() => {
            try { RunOnInputThread(); }
            catch (Exception error) { failure = error; }
        }) { IsBackground = true, Name = "Cage native input regression" };
        thread.SetApartmentState(ApartmentState.STA);
        thread.Start();
        if (!thread.Join(TimeSpan.FromSeconds(15))) throw new TimeoutException("Native input regression did not complete");
        if (failure != null) ExceptionDispatchInfo.Capture(failure).Throw();
    }

    private static void RunOnInputThread()
    {
        var assertions = 0;
        void Check(bool value, string message)
        {
            assertions++;
            if (!value) throw new InvalidOperationException(message);
        }

        var events = new List<Dictionary<string, object?>>();
        using var host = new Form { ShowInTaskbar = false };
        using var web = new Panel();
        using var input = new NativeInput(events.Add);
        host.Controls.Add(web);
        host.Controls.Add(input);
        var owner = Guid.NewGuid().ToString();
        Check(input.ProcessRequest(JsonSerializer.SerializeToElement(new {
            owner, operation = "activate", x = 0, y = 0, height = 24,
        }), web, 1), "native input activation rejected");
        Check(!host.Visible, "regression fixture must remain hidden");

        var down = typeof(NativeInput).GetMethod("OnKeyDown", BindingFlags.Instance | BindingFlags.NonPublic)!;
        var press = typeof(NativeInput).GetMethod("OnKeyPress", BindingFlags.Instance | BindingFlags.NonPublic)!;
        var up = typeof(NativeInput).GetMethod("OnKeyUp", BindingFlags.Instance | BindingFlags.NonPublic)!;
        var original = new byte[256];
        if (!GetKeyboardState(original)) throw new Win32Exception(Marshal.GetLastWin32Error());
        try
        {
            foreach (var capsLock in new[] { false, true })
            foreach (var shift in new[] { false, true })
            {
                var state = new byte[256];
                state[(int)Keys.CapsLock] = (byte)(capsLock ? 1 : 0);
                state[(int)Keys.ShiftKey] = state[(int)Keys.LShiftKey] = (byte)(shift ? 0x80 : 0);
                if (!SetKeyboardState(state)) throw new Win32Exception(Marshal.GetLastWin32Error());
                Check(Control.IsKeyLocked(Keys.CapsLock) == capsLock, "thread CapsLock fixture state was not applied");
                Check(((Control.ModifierKeys & Keys.Shift) != 0) == shift, "thread Shift fixture state was not applied");

                void CheckEvent(int index, string kind, string key, string code, bool textKey)
                {
                    var value = events[index];
                    Check(Equals(value["owner"], owner), "input owner changed");
                    Check(Equals(value["kind"], kind), "input event kind changed");
                    Check(Equals(value["key"], key), "input character or command changed");
                    Check(Equals(value["code"], code), "physical key code changed");
                    Check(Equals(value["textKey"], textKey), "text key classification changed");
                    Check(value.TryGetValue("modifierCapsLock", out var locked) && Equals(locked, capsLock),
                        $"CapsLock={capsLock}, Shift={shift}: {kind} {code} lost CapsLock state");
                    Check(Equals(value["shiftKey"], shift), "Shift state changed");
                    Check(Equals(value["ctrlKey"], false) && Equals(value["altKey"], false) && Equals(value["metaKey"], false),
                        "unpressed modifiers leaked into the event");
                }

                // Invoke the actual production WinForms callbacks, including
                // WM_CHAR's character payload rather than rebuilding Key() here.
                events.Clear();
                var modifiers = shift ? Keys.Shift : Keys.None;
                var character = capsLock ^ shift ? 'A' : 'a';
                down.Invoke(input, [new KeyEventArgs(Keys.A | modifiers)]);
                Check(events.Count == 0, "printable keydown must wait for the character callback");
                press.Invoke(input, [new KeyPressEventArgs(character)]);
                up.Invoke(input, [new KeyEventArgs(Keys.A | modifiers)]);
                Check(events.Count == 2, "printable key must produce one down/up pair");
                CheckEvent(0, "keydown", character.ToString(), "KeyA", true);
                CheckEvent(1, "keyup", "a", "KeyA", false);

                // Commands have no KeyPress callback. CapsLock transitions,
                // navigation and Shift must all carry the same state snapshot.
                foreach (var command in new[] {
                    (Keys.CapsLock, "CapsLock", "CapsLock"),
                    (Keys.Left, "ArrowLeft", "ArrowLeft"),
                    (Keys.ShiftKey, "Shift", "ShiftLeft"),
                })
                {
                    events.Clear();
                    down.Invoke(input, [new KeyEventArgs(command.Item1 | modifiers)]);
                    up.Invoke(input, [new KeyEventArgs(command.Item1 | modifiers)]);
                    Check(events.Count == 2, "command must produce one down/up pair");
                    CheckEvent(0, "keydown", command.Item2, command.Item3, false);
                    CheckEvent(1, "keyup", command.Item2, command.Item3, false);
                }
            }
        }
        finally
        {
            if (!SetKeyboardState(original)) throw new Win32Exception(Marshal.GetLastWin32Error());
        }
        var restored = new byte[256];
        if (!GetKeyboardState(restored)) throw new Win32Exception(Marshal.GetLastWin32Error());
        Check(original.SequenceEqual(restored), "test thread keyboard state was not restored");
        Console.WriteLine($"Windows native input: {assertions} assertions passed; CapsLock on/off, Shift combinations, printable/command down/up, thread state restored.");
    }

    [DllImport("user32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool GetKeyboardState([Out] byte[] state);

    [DllImport("user32.dll", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    private static extern bool SetKeyboardState(byte[] state);
}
