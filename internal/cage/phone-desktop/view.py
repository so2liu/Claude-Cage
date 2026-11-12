#!/usr/bin/python3
"""Window-only capture and shared input for the existing Claude Desktop."""
import base64
import fcntl
import hashlib
import io
import json
import math
import os
from pathlib import Path
import secrets
import signal
import socket
import socketserver
import subprocess
import sys
import time

from PIL import Image
from Xlib import X, XK, Xutil, display, error
from Xlib.ext import composite, xtest


class Failure(Exception):
    def __init__(self, status):
        self.status = status


def run(*args):
    return subprocess.check_output(args, stdin=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=3).decode()


layout_file = Path('/run/cage-phone-desktop/layout.json')


def display_scale():
    path = Path('/run/cage-desktop/display-scale')
    value = float(path.read_text()) if path.exists() else 2
    # A computer showing more of the application on a 1x screen goes below 1.
    if not math.isfinite(value) or not .75 <= value <= 8:
        raise Failure(503)
    return value


def set_display_scale(value):
    dpi = round(value * 96 * 1024)
    config = Path('/run/cage-desktop/xsettings.conf')
    temporary = config.with_suffix('.new')
    temporary.write_text(f'Xft/DPI {dpi}\nGdk/WindowScalingFactor 1\nGdk/UnscaledDPI {dpi}\nGtk/DecorationLayout ":"\n')
    temporary.replace(config)
    pid = int(Path('/run/cage-desktop/xsettings.pid').read_text())
    if Path(f'/proc/{pid}/comm').read_text().strip() != 'xsettingsd':
        raise Failure(503)
    os.kill(pid, signal.SIGHUP)
    Path('/run/cage-desktop/display-scale').write_text(str(value) + '\n')


def encode_frame(image):
    # Preserve fine text and colored edges at native resolution. Detailed
    # images fall back within the existing mobile response-size limit.
    while True:
        for quality in (92, 82, 68, 50):
            buffer = io.BytesIO()
            image.save(buffer, format='JPEG', quality=quality, subsampling=0)
            if buffer.tell() <= 1900000:
                return buffer.getvalue(), image.size
        ratio = min(.85, math.sqrt(1900000 / buffer.tell()) * .95)
        image = image.resize((max(1, round(image.width * ratio)), max(1, round(image.height * ratio))),
                             Image.Resampling.LANCZOS)


def save_layout(layout):
    temporary = layout_file.with_suffix('.new')
    temporary.write_text(json.dumps(layout))
    temporary.replace(layout_file)


def display_size(width, height):
    name = f'cage-phone-{width}x{height}'
    modes = run('xrandr', '--query')
    if name not in modes:
        run('xrandr', '--newmode', name, f'{(width+32)*(height+8)*60/1000000:.3f}',
            *[str(v) for v in (width, width+8, width+16, width+32, height, height+1, height+2, height+8)], '+HSync', '-VSync')
        run('xrandr', '--addmode', 'VNC-0', name)
    run('xrandr', '--output', 'VNC-0', '--mode', name)
    for line in modes.splitlines():
        fields = line.split()
        if fields and fields[0].startswith('cage-phone-') and fields[0] != name:
            run('xrandr', '--delmode', 'VNC-0', fields[0])
            run('xrandr', '--rmmode', fields[0])


def restore_layout():
    if not layout_file.exists():
        return
    with open('/run/cage-desktop/resize.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        layout = json.loads(layout_file.read_text())
        # A computer that resized meanwhile left its latest size and density.
        pending = Path('/run/cage-desktop/desktop-size')
        latest = pending.read_text().split() if pending.exists() else []
        if len(latest) > 2:
            layout['scale'] = float(latest[2])
        if 'scale' in layout and Path('/run/cage-desktop/xsettings.pid').exists():
            set_display_scale(layout['scale'])
        conn = display.Display()
        for wid, saved in layout['minimums'].items():
            try:
                win = conn.create_resource_object('window', int(wid))
                pid = win.get_full_property(conn.intern_atom('_NET_WM_PID'), X.AnyPropertyType)
                hints = win.get_wm_normal_hints()
                if pid is not None and list(pid.value) == [saved['pid']] and hints:
                    values = dict(hints._data)
                    values.update(min_width=saved['width'], min_height=saved['height'])
                    win.set_wm_normal_hints(**values)
            except error.XError:
                # A closed window has no minimum size to restore.
                continue
        conn.sync()
        width, height = layout['width'], layout['height']
        if len(latest) > 1:
            width, height = int(latest[0]), int(latest[1])
        display_size(width, height)
        layout_file.unlink()
        conn.close()


def fitted_window(size, window):
    """Phone CSS rectangle actually occupied by the remote window."""
    ratio = min(size[0] / window.width, size[1] / window.height)
    return ratio, (size[0] - window.width * ratio) / 2, (size[1] - window.height * ratio) / 2


def native_input(payload):
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(2)
        conn.connect(str(Path.home() / '.cache/cage-input/input.sock'))
        conn.sendall(json.dumps(payload).encode() + b'\n')
        return json.loads(conn.makefile('rb').readline(32769))


def token(*parts):
    return hashlib.sha256(json.dumps(parts, separators=(',', ':')).encode()).hexdigest()


class View:
    def __init__(self):
        self.conn = display.Display()
        self.root = self.conn.screen().root
        self.id = os.environ['CAGE_PHONE_ID']
        self.generation = secrets.token_hex(16)
        self.redirected = set()
        self.size = None
        self.pixels = None
        self.scale = display_scale()
        self.dynamic_scale = Path('/run/cage-desktop/xsettings.pid').exists()
        self.requested_scale = self.scale
        self.scale_ready = 0
        self.resize(int(os.environ['CAGE_PHONE_WIDTH']), int(os.environ['CAGE_PHONE_HEIGHT']),
                    float(os.environ.get('CAGE_PHONE_PIXEL_RATIO', '0')))
        self.last = time.monotonic()

    def resize(self, w, h, pixel_ratio=0):
        if type(w) is not int or type(h) is not int or not (240 <= w <= 1440 and 160 <= h <= 1600):
            raise Failure(400)
        self.size = (w, h)
        if type(pixel_ratio) not in (int, float) or not math.isfinite(pixel_ratio) or (pixel_ratio != 0 and not 1 <= pixel_ratio <= 8):
            raise Failure(400)
        if pixel_ratio:
            self.requested_scale = pixel_ratio
        self.fit_display()

    def fit_display(self):
        with open('/run/cage-desktop/resize.lock', 'a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            geometry = self.root.get_geometry()
            current = (geometry.width, geometry.height)
            if layout_file.exists():
                layout = json.loads(layout_file.read_text())
                # Older computer clients can still resize directly. Preserve
                # their latest size for restoration when the phone detaches.
                if self.pixels is not None and current != self.pixels:
                    layout.update(width=current[0], height=current[1])
                    save_layout(layout)
            else:
                minimums = {}
                for wid in self.values(self.root, '_NET_CLIENT_LIST'):
                    win = self.window(wid)
                    if self.role(win) != 'desktop':
                        continue
                    hints = win.get_wm_normal_hints()
                    if hints and hints.flags & Xutil.PMinSize:
                        minimums[str(win.id)] = {'width':hints.min_width, 'height':hints.min_height,
                                                'pid':int(self.values(win, '_NET_WM_PID')[0])}
                save_layout({'width':current[0], 'height':current[1], 'scale':self.scale, 'minimums':minimums})
            scale = min(self.requested_scale, 8192 / max(self.size)) if self.dynamic_scale else self.scale
            if scale != self.scale:
                set_display_scale(scale)
                self.scale = scale
                self.scale_ready = time.monotonic() + .2
            pixels = tuple(round(value * self.scale) for value in self.size)
            if current != pixels:
                display_size(*pixels)
            self.pixels = pixels

    def fit_window(self, win, geometry):
        width, height = self.pixels
        if geometry.width <= width and geometry.height <= height:
            return
        hints = win.get_wm_normal_hints()
        if not hints or not hints.flags & Xutil.PMinSize or win.get_wm_transient_for():
            return
        if hints.min_width <= width and hints.min_height <= height:
            return
        layout = json.loads(layout_file.read_text())
        wid = str(win.id)
        if wid not in layout['minimums']:
            layout['minimums'][wid] = {'width':hints.min_width, 'height':hints.min_height,
                                       'pid':int(self.values(win, '_NET_WM_PID')[0])}
            save_layout(layout)
        values = dict(hints._data)
        values.update(min_width=min(width, hints.min_width), min_height=min(height, hints.min_height))
        win.set_wm_normal_hints(**values)
        self.conn.sync()
        raise Failure(409)

    def values(self, win, name):
        result = win.get_full_property(self.conn.intern_atom(name), X.AnyPropertyType)
        return result.value if result is not None else []

    def window(self, wid):
        return self.conn.create_resource_object('window', int(wid))

    def role(self, win):
        pid = self.values(win, '_NET_WM_PID')
        if len(pid) != 1 or win.get_attributes().map_state != X.IsViewable:
            return ''
        process = Path('/proc') / str(int(pid[0]))
        try:
            if process.stat().st_uid != os.getuid():
                return ''
            exe = os.readlink(process / 'exe')
        except OSError:
            return ''
        klass = {s.lower() for s in (win.get_wm_class() or ())}
        if klass & {'com.anthropic.claude','claude','claude-desktop'}:
            if exe == '/usr/lib/claude-desktop/claude-desktop':
                return 'desktop'
        return ''

    def active(self):
        ids = self.values(self.root, '_NET_ACTIVE_WINDOW')
        if not len(ids) or not ids[0]:
            raise Failure(409)
        win = self.window(ids[0])
        role = self.role(win)
        if not role:
            raise Failure(409)
        g = win.get_geometry()
        self.fit_window(win, g)
        document = token(self.generation, win.id, role, *self.size, g.width, g.height)
        return win, role, g, document

    def focus(self, win, role):
        try:
            value = native_input({'mode': 'status'})
        except (OSError, ValueError):
            value = {}
        ctx = value.get('context', '')
        if not value.get('available') or not ctx:
            return '', False, ''
        return token(self.generation, win.id, ctx), False, ctx

    def snapshot(self):
        self.fit_display()
        if time.monotonic() < self.scale_ready:
            raise Failure(409)
        win, role, g, document = self.active()
        if not (0 < g.width <= 8192 and 0 < g.height <= 8192):
            raise Failure(409)
        # An offscreen pixmap contains only the verified application's window.
        # A foreign tooltip, dialog or another app cannot appear in the frame.
        if win.id not in self.redirected:
            win.composite_redirect_window(composite.RedirectAutomatic)
            self.redirected.add(win.id)
            self.conn.sync()
        pixmap = win.composite_name_window_pixmap()
        try:
            raw = pixmap.get_image(0, 0, g.width, g.height, X.ZPixmap, 0xffffffff)
            if raw.depth not in (24, 32):
                raise Failure(503)
            image = Image.frombytes('RGB', (g.width, g.height), raw.data, 'raw', 'BGRX')
        finally:
            pixmap.free()
        ratio, left, top = fitted_window(self.size, g)
        density = min(self.scale, 1 / ratio)
        canvas = Image.new('RGB', (max(1, round(self.size[0] * density)),
                                   max(1, round(self.size[1] * density))), '#181818')
        image.thumbnail((max(1, round(g.width * ratio * density)), max(1, round(g.height * ratio * density))),
                        Image.Resampling.LANCZOS)
        canvas.paste(image, (round(left * density), round(top * density)))
        encoded, encoded_size = encode_frame(canvas)
        focus, password, _ = self.focus(win, role)
        if document != self.active()[3]:
            raise Failure(409)
        return {'id': self.id, 'width': self.size[0], 'height': self.size[1], 'document': document,
                'image': base64.b64encode(encoded).decode(), 'focus': focus, 'password': password,
                'pixel_width':encoded_size[0], 'pixel_height':encoded_size[1],
                'pixel_ratio':self.scale, 'requested_pixel_ratio':self.requested_scale,
                'pixel_ratio_control':self.dynamic_scale,
                'window': role, 'title': 'Claude Desktop', 'shared': True}

    def switch(self, role):
        for wid in reversed(self.values(self.root, '_NET_CLIENT_LIST_STACKING')):
            if self.role(self.window(wid)) == role:
                run('xdotool', 'windowactivate', '--sync', str(int(wid)))
                return
        raise Failure(409)

    def action(self, req):
        kind = req.get('type')
        if kind == 'resize':
            self.resize(req.get('width'), req.get('height'), req.get('pixel_ratio', 0))
            return
        win, role, geometry, document = self.active()
        if document != req.get('document'):
            raise Failure(409)
        if kind in ('desktop', 'login'):
            self.switch(kind)
            return
        key = req.get('key', '')
        ctx = None
        if kind == 'text' or (kind == 'key' and (req.get('focus') or key == 'Enter' or len(key) == 1)):
            focus, password, ctx = self.focus(win, role)
            if not focus or req.get('focus') != focus or (password and kind == 'text'):
                raise Failure(409)
        if kind == 'text' or (kind == 'key' and len(key) == 1):
            value = req.get('text') if kind == 'text' else key
            if not isinstance(value, str) or not value or len(value.encode()) > 8192:
                raise Failure(400)
            if kind == 'key' and (role != 'login' or not (' ' <= key <= '~')):
                raise Failure(400)
            if document != self.active()[3]:
                raise Failure(409)
            session = secrets.token_hex(16)
            result = native_input({'mode': 'begin', 'context': ctx, 'session': session, 'sequence': 0})
            if result.get('available'):
                result = native_input({'mode': 'commit', 'session': session, 'sequence': 1, 'text': value})
            if not result.get('available'):
                raise Failure(409)
            return
        # Hold the server only for short X11 checks and event dispatch. Do not
        # invoke subprocesses or IBus while the server is grabbed.
        self.conn.grab_server()
        try:
            if document != self.active()[3]:
                raise Failure(409)
            if kind == 'key':
                names = {'Enter':'Return', 'Tab':'Tab', 'Escape':'Escape', 'Backspace':'BackSpace', 'Delete':'Delete',
                         'ArrowLeft':'Left', 'ArrowRight':'Right', 'ArrowUp':'Up', 'ArrowDown':'Down'}
                if key not in names:
                    raise Failure(400)
                code = self.conn.keysym_to_keycode(XK.string_to_keysym(names[key]))
                if not code:
                    raise Failure(400)
                xtest.fake_input(self.conn, X.KeyPress, code)
                xtest.fake_input(self.conn, X.KeyRelease, code)
                return
            if kind not in ('tap', 'scroll'):
                raise Failure(400)
            x, y = req.get('x'), req.get('y')
            if not all(type(n) in (int, float) and math.isfinite(n) and 0 <= n < bound for n, bound in zip((x,y), self.size)):
                raise Failure(400)
            ratio, left, top = fitted_window(self.size, geometry)
            wx, wy = (x - left) / ratio, (y - top) / ratio
            if not (0 <= wx < geometry.width and 0 <= wy < geometry.height):
                raise Failure(409)
            p = self.root.translate_coords(win, int(wx), int(wy))
            xtest.fake_input(self.conn, X.MotionNotify, x=p.x, y=p.y)
            current = self.root
            while current.id != win.id:
                current = current.query_pointer().child
                if not current:
                    raise Failure(409)
            buttons = [1]
            if kind == 'scroll':
                dx, dy = req.get('dx'), req.get('dy')
                if not all(type(n) in (int, float) and math.isfinite(n) and abs(n) <= 3200 for n in (dx,dy)):
                    raise Failure(400)
                buttons = []
                for delta, neg, pos in [(dx,6,7), (dy,4,5)]:
                    if delta:
                        buttons.extend([neg if delta < 0 else pos] * min(20, max(1, round(abs(delta)/50))))
            for button in buttons:
                xtest.fake_input(self.conn, X.ButtonPress, button)
                xtest.fake_input(self.conn, X.ButtonRelease, button)
        finally:
            self.conn.ungrab_server()
            self.conn.sync()


class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.request.settimeout(7)
        try:
            req = json.loads(self.rfile.readline(65537))
            if req.get('id') != view.id:
                raise Failure(410)
            view.last = time.monotonic()
            if req.get('op') == 'frame':
                result = view.snapshot()
            elif req.get('op') == 'action':
                view.action(req)
                result = {'id': view.id}
            else:
                raise Failure(400)
        except Failure as error:
            result = {'status': error.status}
        except Exception:
            result = {'status': 409}
        self.wfile.write(json.dumps(result, separators=(',', ':')).encode() + b'\n')


if __name__ == '__main__':
    if sys.argv[1:] == ['--restore']:
        restore_layout()
        sys.exit(0)
    view = View()
    with socketserver.UnixStreamServer(str(Path(os.environ['XDG_RUNTIME_DIR'])/'view.sock'), Handler) as server:
        server.timeout = 3
        while time.monotonic() - view.last < 120:
            server.handle_request()
