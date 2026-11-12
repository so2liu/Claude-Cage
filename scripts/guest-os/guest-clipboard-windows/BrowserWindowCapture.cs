using System.Diagnostics;
using System.Drawing.Drawing2D;
using System.Drawing.Imaging;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text;
using System.Text.Json.Nodes;
using InputLease = Cage.GuestClipboard.NativeInputCoordinator.Lease;

namespace Cage.GuestClipboard;

// Only the current Node child's Chrome UI is eligible. Public requests never
// supply a PID, HWND, screen rectangle or executable path.
internal static class BrowserWindowCapture
{
    private static readonly SemaphoreSlim Captures = new(1, 1);
    private static readonly Dictionary<int, Pin> Windows = new();
    private static View? last;
    private static string publicTarget="";
    private sealed record OverlayCache(byte[] Pixels, Rectangle Crop, Rectangle Destination, JsonObject Image);
    private static OverlayCache? cachedOverlay;
    private static JsonObject? lastFrame;
    private static string lastSequence = "";
    private static readonly HashSet<InputLease> browserLeases = [];
    internal static InputLease OpenLease(){var lease=NativeGuestInput.OpenLease();lock(browserLeases)browserLeases.Add(lease);return lease;}
    internal static void CloseLease(InputLease lease){lock(browserLeases)browserLeases.Remove(lease);NativeGuestInput.CloseLease(lease);}
    internal static bool ReleaseInput(InputLease lease,JsonObject input)
    {
        if(input["kind"]?.GetValue<string>()=="key"&&input["down"]?.GetValue<bool>()==false)
        {
            if(lease.Symbols.Remove(input["keysym"]?.GetValue<uint>()??0,out var strokes))NativeGuestInput.ReleaseInputs(lease,strokes);
            return true;
        }
        var buttons=NativeGuestInput.PressedButtons(lease);
        if(buttons!=0&&input["kind"]?.GetValue<string>()=="pointer"&&input["mask"]?.GetValue<int>()==0)
        {
            NativeGuestInput.ReleaseInputs(lease,new[]{1,2,4}.Where(bit=>(buttons&bit)!=0).Select(bit=>InputStroke.Button(bit,true)).ToArray());return true;
        }
        return false;
    }
    internal static bool FocusRenderer(Process node,JsonObject proof)
    {var view=Resolve(node,proof);if(view.NativeUI)return false;Focus(view);return true;}
    private sealed record Pin(Process Process, DateTime Started, string Executable, IntPtr Window);
    private sealed record Popup(IntPtr Window, Rectangle Bounds, Rectangle Client);
    private sealed record View(Pin Pin, Rectangle Window, Rectangle Client, Rectangle Surface, Popup[] Popups, bool NativeUI, bool InlineUI,
        string NodeTarget, string Document, string ID, string Token, int X, int Y, int Width, int Height);
    [StructLayout(LayoutKind.Sequential)] private struct Rect { internal int Left, Top, Right, Bottom; internal readonly Rectangle Box => Rectangle.FromLTRB(Left, Top, Right, Bottom); }
    [StructLayout(LayoutKind.Sequential)] private struct Point { internal int X,Y; }
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] private struct ProcessEntry { internal uint Size,Usage,ID;internal nuint Heap;internal uint Module,Threads,Parent;internal int Priority;internal uint Flags;[MarshalAs(UnmanagedType.ByValTStr,SizeConst=260)]internal string File; }
    [StructLayout(LayoutKind.Sequential)] private struct GuiThread { internal uint Size,Flags;internal IntPtr Active,Focus,Capture,MenuOwner,MoveSize,Caret;internal Rect CaretRect; }
    private delegate bool Enumerate(IntPtr window, IntPtr data);

    private static void ValidateNode(Process node)
    { if (!SessionGuard.Available() || node.HasExited || node.SessionId != Process.GetCurrentProcess().SessionId) throw new InvalidDataException("browser_target_changed"); }
    private static void Validate(Pin pin)
    {
        if (!SessionGuard.Available() || pin.Process.HasExited || pin.Process.StartTime.ToUniversalTime() != pin.Started || !IsWindowVisible(pin.Window) || IsIconic(pin.Window) ||
            GetWindowThreadProcessId(pin.Window,out var pid)==0 || pid!=pin.Process.Id || !string.Equals(pin.Process.MainModule?.FileName,pin.Executable,StringComparison.OrdinalIgnoreCase)) throw new InvalidDataException("browser_target_changed");
    }
    private static Process Chrome(Process node)
    {
        ValidateNode(node);
        var choices = new[] {
            Path.Combine(AppContext.BaseDirectory,"chrome","chrome.exe"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles),"Google","Chrome","Application","chrome.exe"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86),"Google","Chrome","Application","chrome.exe"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles),"Microsoft","Edge","Application","msedge.exe"),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86),"Microsoft","Edge","Application","msedge.exe"),
        };
        var snapshot=CreateToolhelp32Snapshot(2,0);
        if(snapshot==new IntPtr(-1))throw new InvalidDataException("browser_process_unavailable");
        var found=new List<Process>();
        try
        {
            var entry=new ProcessEntry{Size=(uint)Marshal.SizeOf<ProcessEntry>(),File=""};
            if(Process32First(snapshot,ref entry))do
            {
                if(entry.Parent!=node.Id)continue;
                Process? process=null;
                try
                {
                    process=Process.GetProcessById((int)entry.ID);
                    var executable=process.MainModule?.FileName;
                    if(process.SessionId!=node.SessionId || process.StartTime<node.StartTime || executable is null || !choices.Contains(executable,StringComparer.OrdinalIgnoreCase))continue;
                    if(!SessionGuard.OwnsProcess(process))continue;
                    found.Add(process);process=null;
                }
                catch { }
                finally {process?.Dispose();}
            }while(Process32Next(snapshot,ref entry));
        }
        finally {CloseHandle(snapshot);}
        if(found.Count!=1){foreach(var p in found)p.Dispose();throw new InvalidDataException("browser_process_unavailable");}
        return found[0];
    }
    private static Rectangle Client(IntPtr window)
    {
        if(!GetClientRect(window,out var client))throw new InvalidDataException("browser_target_changed");
        var point=new Point();if(!ClientToScreen(window,ref point))throw new InvalidDataException("browser_target_changed");
        return new Rectangle(point.X,point.Y,client.Right-client.Left,client.Bottom-client.Top);
    }
    private static string Class(IntPtr window){var text=new StringBuilder(128);return GetClassName(window,text,text.Capacity)>0?text.ToString():"";}
    private static Rectangle RendererSurface(IntPtr window,Rectangle client)
    {
        // Chrome can retain visible renderer HWNDs for background documents.
        // Hit testing follows native child z-order; the largest old rectangle
        // is not evidence that its document is the one on screen.
        var screen=new Point{X=client.X+client.Width/2,Y=client.Y+client.Height/2};
        var parent=window;Rectangle surface=default;
        for(var depth=0;depth<32;depth++)
        {
            var point=screen;if(!ScreenToClient(parent,ref point))throw new InvalidDataException("browser_target_changed");
            // Chromium's legacy renderer HWND can be transparent/disabled
            // while still representing the visible compositor surface.
            var child=ChildWindowFromPointEx(parent,point,1);
            if(child==IntPtr.Zero||child==parent)break;
            if(!IsWindowVisible(child)||GetAncestor(child,2)!=window)throw new InvalidDataException("browser_target_changed");
            // CDP can render a viewport larger than the native window. Keep
            // that complete surface for coordinate conversion and clip only
            // the native pixels we actually capture below.
            if(Class(child)=="Chrome_RenderWidgetHostHWND"&&GetWindowRect(child,out var rect))surface=rect.Box;
            parent=child;
        }
        if(surface.Width<1||surface.Height<1)throw new InvalidDataException("browser_capture_surface_unavailable");
        return surface;
    }
    private static bool Matches(IntPtr window,JsonObject bounds)
    {
        if(!GetWindowRect(window,out var rectangle))return false;
        var box=rectangle.Box;
        foreach(var scale in new[]{1.0,GetDpiForWindow(window)/96.0})
        {
            int left=(int)Math.Round((bounds["left"]?.GetValue<double>()??double.NaN)*scale),top=(int)Math.Round((bounds["top"]?.GetValue<double>()??double.NaN)*scale);
            int width=(int)Math.Round((bounds["width"]?.GetValue<double>()??0)*scale),height=(int)Math.Round((bounds["height"]?.GetValue<double>()??0)*scale);
            if(Math.Abs(box.Left-left)<=12 && Math.Abs(box.Top-top)<=12 && Math.Abs(box.Width-width)<=24 && Math.Abs(box.Height-height)<=24)return true;
        }
        return false;
    }
    private static View Resolve(Process node,JsonObject proof)
    {
        ValidateNode(node);
        var old=SetThreadDpiAwarenessContext(new IntPtr(-4));
        try
        {
            var windowInfo=proof["window"] as JsonObject??throw new InvalidDataException("browser_window_proof_invalid");
            var id=windowInfo["windowId"]?.GetValue<int>()??0;
            var bounds=windowInfo["bounds"] as JsonObject??throw new InvalidDataException("browser_window_proof_invalid");
            Pin? pin=null;
            if(Windows.TryGetValue(id,out var existing))
            {
                try {Validate(existing);if(Matches(existing.Window,bounds))pin=existing;else{Windows.Remove(id);existing.Process.Dispose();}}
                catch {Windows.Remove(id);existing.Process.Dispose();}
            }
            if(pin is null)
            {
                var process=Chrome(node);var candidates=new List<IntPtr>();
                EnumWindows((window,_)=>{if(GetWindowThreadProcessId(window,out var pid)!=0 && pid==process.Id && IsWindowVisible(window) && !IsIconic(window) && GetWindow(window,4)==IntPtr.Zero && Class(window)=="Chrome_WidgetWin_1" && Matches(window,bounds))candidates.Add(window);return true;},IntPtr.Zero);
                var foreground=GetAncestor(GetForegroundWindow(),3);
                if(proof["documentFocused"]?.GetValue<bool>()==true && candidates.Contains(foreground))candidates=[foreground];
                if(candidates.Count!=1){process.Dispose();throw new InvalidDataException("browser_window_unavailable");}
                pin=new Pin(process,process.StartTime.ToUniversalTime(),process.MainModule!.FileName,candidates[0]);Windows[id]=pin;
            }
            Validate(pin);
            if(!GetWindowRect(pin.Window,out var outside))throw new InvalidDataException("browser_target_changed");
            var client=Client(pin.Window);var surface=RendererSurface(pin.Window,client);
            var popups=new List<Popup>();
            EnumWindows((window,_)=>{if(window!=pin.Window && GetWindowThreadProcessId(window,out var pid)!=0 && pid==pin.Process.Id && GetAncestor(window,3)==pin.Window && IsWindowVisible(window) && !IsIconic(window) && GetWindowRect(window,out var rect))
                {var c=Client(window);if(c.Width>0&&c.Height>0&&(long)c.Width*c.Height<=40_000_000)popups.Add(new Popup(window,rect.Box,c));}return true;},IntPtr.Zero);
            var thread=GetWindowThreadProcessId(pin.Window,out _);var info=new GuiThread{Size=(uint)Marshal.SizeOf<GuiThread>()};
            var inlineUI=GetGUIThreadInfo(thread,ref info) && info.Focus!=IntPtr.Zero && GetAncestor(info.Focus,2)==pin.Window && proof["documentFocused"]?.GetValue<bool>()!=true && Class(info.Focus)!="Chrome_RenderWidgetHostHWND";
            var ui=popups.Count>0 || inlineUI;
            var target=proof["target"]?.GetValue<string>()??"";var document=proof["document"]?.GetValue<string>()??"";
            var signature=new JsonArray(target,document,id,pin.Window.ToInt64(),outside.Left,outside.Top,outside.Right,outside.Bottom,surface.X,surface.Y,surface.Width,surface.Height,ui,inlineUI,
                new JsonArray(popups.Select(p=>(JsonNode)new JsonArray(p.Window.ToInt64(),p.Bounds.X,p.Bounds.Y,p.Bounds.Width,p.Bounds.Height)).ToArray())).ToJsonString();
            var opaque=Convert.ToHexString(SHA256.HashData(Protocol.Utf8.GetBytes(signature))).ToLowerInvariant()[..32];
            return new View(pin,outside.Box,client,surface,popups.ToArray(),ui,inlineUI,target,document,proof["id"]?.GetValue<string>()??"",opaque,
                proof["x"]?.GetValue<int>()??0,proof["y"]?.GetValue<int>()??0,proof["width"]?.GetValue<int>()??0,proof["height"]?.GetValue<int>()??0);
        }
        finally {if(old!=IntPtr.Zero)SetThreadDpiAwarenessContext(old);}
    }
    internal static async Task Decorate(Process node,JsonObject frame,Func<Task<JsonObject>> currentProof)
    {
        var proof=frame["native"] as JsonObject??throw new InvalidDataException("browser_window_proof_invalid");
        if(proof["target"]?.GetValue<string>()!=frame["target"]?.GetValue<string>())throw new InvalidDataException("browser_target_changed");
        var view=Resolve(node,proof);
        var overlays=new JsonArray();
        if(view.NativeUI)
        {
            await Captures.WaitAsync();
            try
            {
                // An independent popup has its own validated capture below.
                // Keep the underlying CDP page instead of duplicating Edge's
                // window controls and tab strip inside the Cage browser pane.
                if (view.Popups.Length == 0)
                {
                    var bytes=await GuestWindowCapture.Capture(view.Pin.Window,view.Pin.Process.Id,view.Client.Width,view.Client.Height,
                        ()=>Validate(view.Pin),allowSolid:proof["blankDocument"]?.GetValue<bool>()==true);
                    var visible = Rectangle.Intersect(view.Surface, view.Client);
                    var crop = new Rectangle(visible.X - view.Client.X, visible.Y - view.Client.Y, visible.Width, visible.Height);
                    var sx = (double)view.Width / view.Surface.Width;
                    var sy = (double)view.Height / view.Surface.Height;
                    overlays.Add(Image(bytes, crop,
                        view.X + (int)Math.Round((visible.X - view.Surface.X) * sx),
                        view.Y + (int)Math.Round((visible.Y - view.Surface.Y) * sy),
                        (int)Math.Round(visible.Width * sx), (int)Math.Round(visible.Height * sy)));
                }
                foreach(var popup in view.Popups.Reverse())
                {
                    void Check(){Validate(view.Pin);if(GetWindowThreadProcessId(popup.Window,out var pid)==0||pid!=view.Pin.Process.Id||GetAncestor(popup.Window,3)!=view.Pin.Window||!GetWindowRect(popup.Window,out var rect)||rect.Box!=popup.Bounds||Client(popup.Window)!=popup.Client)throw new InvalidDataException("browser_target_changed");}
                    var png=await GuestWindowCapture.Capture(popup.Window,view.Pin.Process.Id,popup.Client.Width,popup.Client.Height,Check,preserveAlpha:true);
                    var sx=(double)view.Width/view.Surface.Width;var sy=(double)view.Height/view.Surface.Height;
                    var destination=new Rectangle(view.X+(int)Math.Round((popup.Client.X-view.Surface.X)*sx),view.Y+(int)Math.Round((popup.Client.Y-view.Surface.Y)*sy),(int)Math.Round(popup.Client.Width*sx),(int)Math.Round(popup.Client.Height*sy));
                    var clipped=Rectangle.Intersect(destination,new Rectangle(0,0,frame["width"]!.GetValue<int>(),frame["height"]!.GetValue<int>()));
                    if(clipped.Width<1||clipped.Height<1)continue;
                    var sectionX=(int)Math.Round((clipped.X-destination.X)/sx);var sectionY=(int)Math.Round((clipped.Y-destination.Y)/sy);
                    var section=new Rectangle(sectionX,sectionY,Math.Min(popup.Client.Width-sectionX,(int)Math.Round(clipped.Width/sx)),Math.Min(popup.Client.Height-sectionY,(int)Math.Round(clipped.Height/sy)));
                    overlays.Add(Image(png,section,clipped.X,clipped.Y,clipped.Width,clipped.Height));
                }
                var after=Resolve(node,await currentProof());if(after.Token!=view.Token)throw new InvalidDataException("browser_target_changed");
            }
            finally {Captures.Release();}
        }
        last=view;publicTarget=view.Token;frame["target"]=view.Token;frame.Remove("native");if(overlays.Count>0)frame["overlays"]=overlays;
    }
    internal static JsonObject FrameReply(JsonObject frame, string? after)
    {
        frame.Remove("sequence");
        if (lastFrame is null || !JsonNode.DeepEquals(lastFrame, frame))
        {
            var bytes = System.Text.Json.JsonSerializer.SerializeToUtf8Bytes(frame);
            if (bytes.Length > Protocol.WireLimit) throw new InvalidDataException("browser_frame_limit");
            lastSequence = Convert.ToHexString(SHA256.HashData(bytes));
            lastFrame = (JsonObject)frame.DeepClone();
        }
        var sequence = lastSequence;
        if (sequence == after) return new JsonObject { ["unchanged"] = true, ["sequence"] = sequence };
        frame["sequence"] = sequence;
        return frame;
    }

    private static JsonObject Image(byte[] pixels,Rectangle region,int x,int y,int width,int height)
    {
        var destination = new Rectangle(x, y, width, height);
        if (cachedOverlay is { } cached && ReferenceEquals(cached.Pixels, pixels) && cached.Crop == region && cached.Destination == destination)
            return (JsonObject)cached.Image.DeepClone();
        using var input = new MemoryStream(pixels, false);
        using var source = new Bitmap(input);
        if(width<1||height<1||(long)width*height>40_000_000||region.X<0||region.Y<0||region.Right>source.Width||region.Bottom>source.Height)throw new InvalidDataException("browser_frame_limit");
        using var output=new Bitmap(width,height,PixelFormat.Format32bppArgb);using(var g=Graphics.FromImage(output)){g.InterpolationMode=InterpolationMode.HighQualityBicubic;g.DrawImage(source,new Rectangle(0,0,width,height),region,GraphicsUnit.Pixel);}
        using var bytes=new MemoryStream();output.Save(bytes,ImageFormat.Png);if(bytes.Length>Protocol.PngLimit)throw new InvalidDataException("browser_frame_limit");
        var image = new JsonObject{["png"]=Convert.ToBase64String(bytes.ToArray()),["x"]=x,["y"]=y,["width"]=width,["height"]=height};
        cachedOverlay = new OverlayCache(pixels, region, destination, (JsonObject)image.DeepClone());
        return image;
    }
    internal static bool HandleInput(Process node,InputLease lease,JsonObject input,JsonObject proof,bool pagePointerHeld=false)
    {
        var prior=SetThreadDpiAwarenessContext(new IntPtr(-4));
        try{return HandleInputCore(node,lease,input,proof,pagePointerHeld);}finally{if(prior!=IntPtr.Zero)SetThreadDpiAwarenessContext(prior);}
    }
    private static bool HandleInputCore(Process node,InputLease lease,JsonObject input,JsonObject proof,bool pagePointerHeld)
    {
        var before=last??throw new InvalidDataException("browser_target_changed");var now=Resolve(node,proof);
        if(pagePointerHeld && now.InlineUI && input["kind"]?.GetValue<string>()=="pointer")throw new InvalidDataException("browser_target_changed");
        if(proof["focusAmbiguous"]?.GetValue<bool>()==true)throw new InvalidDataException("browser_target_changed");
        if(!now.NativeUI&&proof["documentFocused"]?.GetValue<bool>()!=true)throw new InvalidDataException("browser_target_changed");
        var mouseMask=NativeGuestInput.PressedButtons(lease);
        var released=input["kind"]?.GetValue<string>()=="key"&&input["down"]?.GetValue<bool>()==false || mouseMask!=0&&input["kind"]?.GetValue<string>()=="pointer"&&input["mask"]?.GetValue<int>()==0;
        if(input["target"]?.GetValue<string>()!=publicTarget || now.Token!=before.Token && !released)throw new InvalidDataException("browser_target_changed");
        input["target"]=now.NodeTarget;
        if(input["kind"]?.GetValue<string>()=="pointer")
        {
            var x=input["x"]?.GetValue<int>()??-1;var y=input["y"]?.GetValue<int>()??-1;
            if(x<now.X||x>=now.X+now.Width||y<now.Y||y>=now.Y+now.Height)
            {
                if(mouseMask!=0){input["x"]=Math.Clamp(x,now.X,now.X+now.Width-1);input["y"]=Math.Clamp(y,now.Y,now.Y+now.Height-1);}
                else
                {
                    // A click into another split pane first dismisses our own
                    // chrome UI, then lets CDP activate that existing webpage.
                    if(now.NativeUI){Focus(now);Tap(now,[InputStroke.Key(27,false),InputStroke.Key(27,true)]);}
                    var modifiers=(NativeGuestInput.PressedKey(18)?1:0)|(NativeGuestInput.PressedKey(17)?2:0)|(NativeGuestInput.PressedKey(91)||NativeGuestInput.PressedKey(92)?4:0)|(NativeGuestInput.PressedKey(16)?8:0);
                    input["modifiers"]=modifiers;return false;
                }
            }
        }
        Focus(now);
        if(input["kind"]?.GetValue<string>()=="pointer" && !now.InlineUI && mouseMask==0)
        {
            var x=input["x"]?.GetValue<int>()??-1;
            var y=input["y"]?.GetValue<int>()??-1;
            var screenX=now.Surface.X+(int)Math.Round((x-now.X)*(double)now.Surface.Width/now.Width);
            var screenY=now.Surface.Y+(int)Math.Round((y-now.Y)*(double)now.Surface.Height/now.Height);
            if(pagePointerHeld || !now.Popups.Any(p=>p.Client.Contains(screenX,screenY)))
            {
                // CDP page coordinates remain valid beyond the guest desktop.
                input["modifiers"]=(NativeGuestInput.PressedKey(18)?1:0)|(NativeGuestInput.PressedKey(17)?2:0)|(NativeGuestInput.PressedKey(91)||NativeGuestInput.PressedKey(92)?4:0)|(NativeGuestInput.PressedKey(16)?8:0);
                return false;
            }
        }
        if(input["kind"]?.GetValue<string>()=="pointer")Pointer(now,lease,input);
        else if(input["kind"]?.GetValue<string>()=="key")Keyboard(now,lease,input);
        else throw new InvalidDataException("browser_input_invalid");
        return true;
    }
    internal static bool SelectPane(Process node,InputLease lease,JsonObject input,JsonObject proof)
    {
        if(input["kind"]?.GetValue<string>()!="pointer"||NativeGuestInput.PressedButtons(lease)!=0||((input["mask"]?.GetValue<int>()??0)&7)==0)return false;
        var view=Resolve(node,proof);if(last is null||input["target"]?.GetValue<string>()!=publicTarget||view.Token!=last.Token)throw new InvalidDataException("browser_target_changed");
        var x=input["x"]?.GetValue<int>()??-1;
        if(x>=view.X&&x<view.X+view.Width)return false;
        if(view.NativeUI){Focus(view);Tap(view,[InputStroke.Key(27,false),InputStroke.Key(27,true)]);}
        return true;
    }
    internal static void Transition(Process node,JsonObject proof)
    {
        var view=Resolve(node,proof);
        if(last is not null&&last.NodeTarget==view.NodeTarget&&last.Pin.Window==view.Pin.Window&&last.Pin.Started==view.Pin.Started)last=view;
    }
    internal static bool Edit(Process node,JsonObject proof,string action,string text="")
    {
        var view=Resolve(node,proof);if(!view.NativeUI)return false;Focus(view);
        if(action=="insert-text")
        {if(Protocol.Utf8.GetByteCount(text)>8192)throw new InvalidDataException("browser_input_invalid");Tap(view,text.SelectMany(c=>new[]{InputStroke.Key(c,false,true),InputStroke.Key(c,true,true)}).ToArray());}
        else
        {
            ushort key=action switch{"paste"=>86,"copy"=>67,"cut"=>88,"select-all"=>65,_=>throw new InvalidDataException("browser_input_invalid")};
            Tap(view,[InputStroke.Key(17,false),InputStroke.Key(key,false),InputStroke.Key(key,true),InputStroke.Key(17,true)]);
        }
        return true;
    }
    internal static async Task<JsonObject?> Ime(Process node,JsonObject proof,JsonObject input,CancellationToken token)
    {
        var view=Resolve(node,proof);if(!view.NativeUI)return null;Focus(view);
        var result=await WindowsIme.Handle(input,view.Pin.Process.Id,view.Token,view.Surface.X,view.Surface.Y,
            text=>Tap(view,text.SelectMany(c=>new[]{InputStroke.Key(c,false,true),InputStroke.Key(c,true,true)}).ToArray()),token);
        if(result["rect"] is JsonArray rect && rect.Count==4)
        {
            var sx=(double)view.Width/view.Surface.Width;var sy=(double)view.Height/view.Surface.Height;
            result["rect"]=new JsonArray(view.X+rect[0]!.GetValue<double>()*sx,view.Y+rect[1]!.GetValue<double>()*sy,rect[2]!.GetValue<double>()*sx,rect[3]!.GetValue<double>()*sy);
        }
        return result;
    }
    private static void Focus(View view)
    {
        Validate(view.Pin);if(GetAncestor(GetForegroundWindow(),3)!=view.Pin.Window)SetForegroundWindow(view.Pin.Window);
        if(GetAncestor(GetForegroundWindow(),3)!=view.Pin.Window)throw new InvalidDataException("browser_focus_required");
    }
    private static Func<bool> ReleaseAllowed(View view)
    {
        var pid=view.Pin.Process.Id;var started=view.Pin.Started;var executable=view.Pin.Executable;var window=view.Pin.Window;var session=view.Pin.Process.SessionId;
        return ()=>
        {
            if(!SessionGuard.Available())return false;
            try
            {
                using var process=Process.GetProcessById(pid);
                if(process.HasExited)return true;
                if(process.StartTime.ToUniversalTime()!=started||process.SessionId!=session||!string.Equals(process.MainModule?.FileName,executable,StringComparison.OrdinalIgnoreCase))return false;
                return !IsWindow(window)||GetWindowThreadProcessId(window,out var owner)!=0&&owner==pid;
            }
            catch(ArgumentException){return true;} // The verified owning process has exited.
            catch(InvalidOperationException){return true;}
            catch{return false;}
        };
    }
    private static void Emit(View view,InputLease lease,InputStroke[] strokes)
    {
        NativeGuestInput.Emit(lease,strokes,()=>{Validate(view.Pin);if(GetAncestor(GetForegroundWindow(),3)!=view.Pin.Window)throw new InvalidDataException("browser_focus_required");},ReleaseAllowed(view));
    }
    private static void Tap(View view,InputStroke[] strokes)
    {
        var lease=NativeGuestInput.OpenLease();try{Emit(view,lease,strokes);}finally{NativeGuestInput.CloseLease(lease);}
    }
    private static void Pointer(View view,InputLease lease,JsonObject input)
    {
        var x=input["x"]?.GetValue<int>()??-1;var y=input["y"]?.GetValue<int>()??-1;var mask=input["mask"]?.GetValue<int>()??-1;
        if(mask<0||mask>127||x<0||y<0)throw new InvalidDataException("browser_input_invalid");
        var screenX=view.Surface.X+(int)Math.Round((x-view.X)*(double)view.Surface.Width/view.Width);var screenY=view.Surface.Y+(int)Math.Round((y-view.Y)*(double)view.Surface.Height/view.Height);
        if(!view.Client.Contains(screenX,screenY)&&!view.Popups.Any(p=>p.Client.Contains(screenX,screenY)))throw new InvalidDataException("browser_input_invalid");
        if(!SetCursorPos(screenX,screenY))throw new InvalidDataException("browser_input_failed");
        var mouseMask=NativeGuestInput.PressedButtons(lease);var inputs=new List<InputStroke>();
        foreach(var bit in new[]{1,2,4})if((mask&bit)!=(mouseMask&bit))inputs.Add(InputStroke.Button(bit,(mask&bit)==0));
        if(input["deltaX"] is not null||input["deltaY"] is not null)
        {
            // A viewer that sends the scrolled distance gets that distance in
            // native menus as well: 100 pixels are one wheel notch.
            var deltaX=input["deltaX"]?.GetValue<int>()??0;var deltaY=input["deltaY"]?.GetValue<int>()??0;
            if(Math.Abs(deltaX)>2048||Math.Abs(deltaY)>2048)throw new InvalidDataException("browser_input_invalid");
            if(deltaY!=0)inputs.Add(InputStroke.Wheel(-deltaY*120/100));
            if(deltaX!=0)inputs.Add(InputStroke.Wheel(deltaX*120/100,true));
        }
        else
        {
            if((mask&24)!=0)inputs.Add(InputStroke.Wheel((mask&8)!=0?120:-120));
            if((mask&96)!=0)inputs.Add(InputStroke.Wheel((mask&32)!=0?-120:120,true));
        }
        if(inputs.Count>0)Emit(view,lease,inputs.ToArray());
    }
    private static void Keyboard(View view,InputLease lease,JsonObject input)
    {
        var sym=input["keysym"]?.GetValue<uint>()??0;var down=input["down"]?.GetValue<bool>()??false;
        var special=new Dictionary<uint,ushort>{{0xff08,8},{0xff09,9},{0xff0d,13},{0xff1b,27},{0xff50,36},{0xff51,37},{0xff52,38},{0xff53,39},{0xff54,40},{0xff55,33},{0xff56,34},{0xff57,35},{0xffff,46},{0xffe1,16},{0xffe2,16},{0xffe3,17},{0xffe4,17},{0xffe9,18},{0xffea,18},{0xffe7,91},{0xffe8,92},{0xffeb,91},{0xffec,92}};
        if(sym>=0xffbe&&sym<=0xffc9)special[sym]=(ushort)(112+sym-0xffbe);
        if(!down)
        {
            if(lease.Symbols.Remove(sym,out var prior))Emit(view,lease,prior.Select(stroke=>stroke with{Up=true}).ToArray());
            return;
        }
        if(lease.Symbols.TryGetValue(sym,out var repeated)){Emit(view,lease,repeated);return;}
        if(special.TryGetValue(sym,out var key)){var strokes=new[]{InputStroke.Key(key,false)};lease.Symbols[sym]=strokes;Emit(view,lease,strokes);return;}
        var cp=(sym&0xff000000)==0x01000000?sym&0xffffff:sym;
        if(cp<32||cp>0x10ffff||cp is >=0xd800 and <=0xdfff)throw new InvalidDataException("browser_input_invalid");
        InputStroke[] characters;
        if(cp<=0xffff && (NativeGuestInput.PressedKey(17)||NativeGuestInput.PressedKey(18)))
        {var mapped=VkKeyScan((char)cp);if(mapped<0)throw new InvalidDataException("browser_input_invalid");characters=[InputStroke.Key((ushort)(mapped&255),false)];}
        else characters=char.ConvertFromUtf32((int)cp).Select(c=>InputStroke.Key(c,false,true)).ToArray();
        lease.Symbols[sym]=characters;Emit(view,lease,characters);
    }
    internal static void Close(){InputLease[] leases;lock(browserLeases)leases=browserLeases.ToArray();foreach(var lease in leases)NativeGuestInput.ResetLease(lease);foreach(var p in Windows.Values)p.Process.Dispose();Windows.Clear();last=null;publicTarget="";cachedOverlay=null;lastFrame=null;lastSequence="";}

    [DllImport("kernel32.dll",SetLastError=true)]private static extern IntPtr CreateToolhelp32Snapshot(uint flags,uint process);
    [DllImport("kernel32.dll",CharSet=CharSet.Unicode,EntryPoint="Process32FirstW")]private static extern bool Process32First(IntPtr snapshot,ref ProcessEntry entry);
    [DllImport("kernel32.dll",CharSet=CharSet.Unicode,EntryPoint="Process32NextW")]private static extern bool Process32Next(IntPtr snapshot,ref ProcessEntry entry);
    [DllImport("kernel32.dll")]private static extern bool CloseHandle(IntPtr handle);
    [DllImport("user32.dll")]private static extern bool EnumWindows(Enumerate callback,IntPtr data);
    [DllImport("user32.dll")]private static extern bool EnumChildWindows(IntPtr window,Enumerate callback,IntPtr data);
    [DllImport("user32.dll")]private static extern uint GetWindowThreadProcessId(IntPtr window,out uint process);
    [DllImport("user32.dll")]private static extern bool GetWindowRect(IntPtr window,out Rect rectangle);
    [DllImport("user32.dll")]private static extern bool GetClientRect(IntPtr window,out Rect rectangle);
    [DllImport("user32.dll")]private static extern bool ClientToScreen(IntPtr window,ref Point point);
    [DllImport("user32.dll")]private static extern bool ScreenToClient(IntPtr window,ref Point point);
    [DllImport("user32.dll")]private static extern IntPtr ChildWindowFromPointEx(IntPtr parent,Point point,uint flags);
    [DllImport("user32.dll",CharSet=CharSet.Unicode,EntryPoint="GetClassNameW")]private static extern int GetClassName(IntPtr window,StringBuilder name,int length);
    [DllImport("user32.dll")]private static extern IntPtr GetWindow(IntPtr window,uint command);
    [DllImport("user32.dll")]private static extern IntPtr GetAncestor(IntPtr window,uint flags);
    [DllImport("user32.dll")]private static extern bool IsWindowVisible(IntPtr window);
    [DllImport("user32.dll")]private static extern bool IsIconic(IntPtr window);
    [DllImport("user32.dll")]private static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")]private static extern bool SetForegroundWindow(IntPtr window);
    [DllImport("user32.dll")]private static extern uint GetDpiForWindow(IntPtr window);
    [DllImport("user32.dll")]private static extern IntPtr SetThreadDpiAwarenessContext(IntPtr context);
    [DllImport("user32.dll")]private static extern bool GetGUIThreadInfo(uint thread,ref GuiThread info);
    [DllImport("user32.dll")]private static extern bool SetCursorPos(int x,int y);
    [DllImport("user32.dll")]private static extern bool IsWindow(IntPtr window);
    [DllImport("user32.dll",CharSet=CharSet.Unicode,EntryPoint="VkKeyScanW")]private static extern short VkKeyScan(char key);
}
