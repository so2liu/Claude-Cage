#!/usr/bin/python3
"""Private IBus bridge. Text is transient and is never logged or persisted."""
import json
import os
from pathlib import Path
import re
import socket
import sys

directory = (Path('/run/cage-phone-desktop/input') if os.environ.get('CAGE_PHONE_DESKTOP_INPUT') == '1'
             else Path.home() / '.cache' / 'cage-input')
socket_path = directory / 'input.sock'
limit = 32768


def request():
    data = sys.stdin.buffer.readline(limit + 1)
    if not data:
        raise EOFError
    if len(data) > limit or not data.endswith(b'\n'):
        raise ValueError('Invalid input request')
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(3)
        client.connect(str(socket_path))
        client.sendall(data)
        result = client.makefile('rb').readline(limit + 1)
    if len(result) > limit or not result.endswith(b'\n'):
        raise ValueError('Invalid input response')
    sys.stdout.buffer.write(result)
    sys.stdout.buffer.flush()


if __name__ == '__main__' and sys.argv[1:] == ['--request']:
    try:
        request()
    except (OSError, ValueError, EOFError):
        print('{"available":false}')
    sys.exit(0)

if __name__ == '__main__' and sys.argv[1:] == ['--relay']:
    # One authorized caller sends consecutive requests, one line each, without
    # starting a process for every caret poll. Its closed pipe ends the relay.
    while True:
        try:
            request()
        except EOFError:
            break
        except OSError:
            print('{"available":false}', flush=True)
        except ValueError:
            # The stream has no boundary to resume from.
            sys.exit(1)
    sys.exit(0)

import gi
import secrets
import socketserver
import subprocess
import threading
import time

gi.require_version('IBus', '1.0')
from gi.repository import IBus, GLib

IBus.init()
active = None
engines = []
probe_key = 0xffd5  # F24: consumed by this engine to refresh Chromium's caret.


def prepare_probe():
    # xdotool temporarily maps an absent keysym and immediately restores it.
    # GTK can then receive NoSymbol on later probes. Reserve one otherwise
    # empty keycode for this private virtual display's lifetime instead.
    try:
        mapping = subprocess.check_output(['xmodmap', '-pke'], text=True)
        empty = []
        for line in mapping.splitlines():
            match = re.fullmatch(r'keycode\s+(\d+)\s*=\s*(.*)', line)
            if not match:
                continue
            symbols = match[2].split()
            if symbols and symbols[0] == 'F24':
                return True
            if int(match[1]) >= 128 and not symbols:
                empty.append(int(match[1]))
        if not empty:
            return False
        subprocess.run(['xmodmap', '-e', f'keycode {max(empty)} = F24'],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        return True
    except (OSError, subprocess.SubprocessError):
        return False


probe_ready = prepare_probe()


class Engine(IBus.Engine):
    __gtype_name__ = 'CageInputEngine'

    def __init__(self, **args):
        super().__init__(**args)
        self.context = ''
        self.session = ''
        self.sequence = -1
        self.rect = None
        self.purpose = 0
        self.probing = False
        self.probe_seen = False
        self.reachable = False
        self.updated = 0

    def clear(self):
        self.update_preedit_text(IBus.Text.new_from_string(''), 0, False)
        self.session = ''
        self.sequence = -1

    def do_focus_in(self):
        global active
        if active is not self:
            self.context = secrets.token_hex(16)
            self.rect = None
            self.reachable = False
            active = self

    def do_focus_in_id(self, path, client):
        # A single engine can be reused for different application fields.
        global active
        if self.context_path != str(path):
            self.clear()
            self.context = secrets.token_hex(16)
            self.rect = None
        self.context_path = str(path)
        self.do_focus_in()

    context_path = ''

    def do_focus_out(self):
        global active
        self.clear()
        self.rect = None
        self.probing = False
        self.reachable = False
        if active is self:
            active = None

    def do_focus_out_id(self, path):
        self.do_focus_out()

    def do_reset(self):
        self.clear()
        self.context = secrets.token_hex(16)
        self.reachable = False

    def do_set_cursor_location(self, x, y, width, height):
        self.rect = [x, y, width, height]

    def do_set_content_type(self, purpose, hints):
        self.purpose = int(purpose)

    def do_process_key_event(self, keyval, keycode, state):
        if self.probing and keyval == probe_key:
            self.probe_seen = True
            self.reachable = True
            return True
        return False


bus = IBus.Bus()
if not bus.is_connected():
    raise RuntimeError('Private input bus unavailable')


class Factory(IBus.Factory):
    def do_create_engine(self, name):
        options = dict(connection=bus.get_connection(),
                       object_path='/org/freedesktop/IBus/Cage/' + str(len(engines)))
        # Ubuntu 22.04's IBus uses the original per-context focus signals.
        # Newer IBus can reuse engines and supplies the explicit focus ID.
        if IBus.Engine.find_property('has-focus-id') is not None:
            options['has_focus_id'] = True
        engine = Engine(**options)
        engines.append(engine)
        return engine


factory = Factory(connection=bus.get_connection(), object_path=IBus.PATH_FACTORY)
component = IBus.Component(name='org.freedesktop.IBus.Cage', description='Cage host input',
                           version='1', license='MIT', author='Cage', homepage='',
                           command_line='', textdomain='')
component.add_engine(IBus.EngineDesc(name='cage-host', longname='Cage Host Input',
                                   description='Host input transport', language='en',
                                   license='MIT', author='Cage', icon='', layout='us'))
if not bus.register_component(component):
    raise RuntimeError('Private input engine unavailable')


def snapshot(engine):
    if engine is None or engine is not active or not engine.reachable or engine.purpose in (8, 9):
        return {'available': False}
    return {'available': True, 'context': engine.context, 'rect': engine.rect}


def utf16_index(text, offset):
    if type(offset) is not int or offset < 0:
        raise ValueError('Invalid input range')
    data = text.encode('utf-16-le')
    if offset * 2 > len(data):
        raise ValueError('Invalid input range')
    return len(data[:offset * 2].decode('utf-16-le'))


def apply(value):
    engine = active
    mode = value.get('mode')
    if mode in ('phone-status', 'phone-text', 'phone-char') and os.environ.get('CAGE_PHONE_DESKTOP_INPUT') == '1':
        if engine is None or not engine.reachable or not engine.context:
            return {'available': False}
        password = engine.purpose in (8, 9)
        if mode == 'phone-status':
            return {'available': True, 'context': engine.context, 'password': password}
        text = value.get('text', '')
        if engine.context != value.get('context') or not isinstance(text, str) or not text:
            return {'available': False}
        if mode == 'phone-text' and (password or len(text.encode()) > 8192):
            return {'available': False}
        if mode == 'phone-char' and (len(text) != 1 or not (' ' <= text <= '~')):
            return {'available': False}
        engine.commit_text(IBus.Text.new_from_string(text))
        return {'available': True}
    if mode == 'status':
        if engine is not None and engine.session and value.get('session') == engine.session:
            engine.updated = time.monotonic()
        return snapshot(engine)
    session = value.get('session', '')
    sequence = value.get('sequence')
    if not isinstance(session, str) or not re.fullmatch('[a-f0-9]{32}', session):
        raise ValueError('Invalid input session')
    if type(sequence) is not int or sequence < 0:
        raise ValueError('Invalid input sequence')
    if mode == 'cancel':
        # Cleanup can target an older field; it must never clear a newer session.
        for old in engines:
            if old.session == session and sequence > old.sequence:
                old.clear()
        return {'available': False}
    if not snapshot(engine)['available']:
        return {'available': False}
    if mode == 'begin':
        if value.get('context') != engine.context:
            raise ValueError('Input target changed')
        engine.clear()
        engine.session = session
        engine.sequence = sequence
        engine.updated = time.monotonic()
        return snapshot(engine)
    if engine.session != session or sequence <= engine.sequence:
        raise ValueError('Input session expired')
    text = value.get('text', '')
    if not isinstance(text, str) or len(text.encode('utf-8')) > 8192:
        raise ValueError('Input text too large')
    if mode == 'update':
        start = utf16_index(text, value.get('selectionStart', 0))
        end = utf16_index(text, value.get('selectionEnd', 0))
        if end < start:
            raise ValueError('Invalid input range')
        preedit = IBus.Text.new_from_string(text)
        preedit.append_attribute(IBus.AttrType.UNDERLINE, IBus.AttrUnderline.SINGLE, 0, len(text))
        if end > start:
            preedit.append_attribute(IBus.AttrType.BACKGROUND, 0x3584e4, start, end)
            preedit.append_attribute(IBus.AttrType.FOREGROUND, 0xffffff, start, end)
        engine.update_preedit_text_with_mode(preedit, end, bool(text), IBus.PreeditFocusMode.CLEAR)
        engine.sequence = sequence
        engine.updated = time.monotonic()
    elif mode == 'commit':
        engine.clear()
        if text:
            engine.commit_text(IBus.Text.new_from_string(text))
    else:
        raise ValueError('Invalid input mode')
    return snapshot(engine)


class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(3)
        try:
            line = self.rfile.readline(limit + 1)
            if len(line) > limit or not line.endswith(b'\n'):
                raise ValueError('Invalid input request')
            value = json.loads(line)
            if not isinstance(value, dict):
                raise ValueError('Invalid input request')
            done = threading.Event()
            result = {}

            def dispatch():
                try:
                    result.update(apply(value))
                except UnicodeError:
                    result.update(error='Invalid input range')
                except ValueError as error:
                    # All ValueErrors above contain fixed protocol messages.
                    # Never include an exception that can contain input text.
                    result.update(error=str(error))
                finally:
                    done.set()
                return False

            def probe():
                engine = active
                desktop_probe = (os.environ.get('CAGE_PHONE_DESKTOP_INPUT') == '1' and
                                 value.get('mode') in ('phone-status', 'phone-text', 'phone-char'))
                ordinary_probe = value.get('mode') in ('status', 'begin') and engine is not None and engine.purpose not in (8, 9)
                if probe_ready and engine is not None and (desktop_probe or ordinary_probe):
                    engine.probing = True
                    engine.probe_seen = False
                    subprocess.Popen(['xdotool', 'key', '--delay', '0', 'F24'],
                                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

                    def finish():
                        engine.probing = False
                        engine.reachable = engine.probe_seen and engine is active
                        return dispatch()
                    GLib.timeout_add(45, finish)
                else:
                    dispatch()
                return False

            GLib.idle_add(probe)
            if not done.wait(2):
                result = {'error': 'Input bridge timed out'}
            self.wfile.write((json.dumps(result, separators=(',', ':')) + '\n').encode())
        except (OSError, ValueError):
            pass


class Server(socketserver.ThreadingUnixStreamServer):
    daemon_threads = True


directory.mkdir(mode=0o700, parents=True, exist_ok=True)
os.chmod(directory, 0o700)
socket_path.unlink(missing_ok=True)
server = Server(str(socket_path), Handler)
os.chmod(socket_path, 0o600)
threading.Thread(target=server.serve_forever, daemon=True).start()


def activate():
    def finish(source, result):
        if source.set_global_engine_async_finish(result):
            (directory / 'ready').touch(mode=0o600)
    bus.set_global_engine_async('cage-host', 10000, None, finish)
    return False


def expire():
    for engine in engines:
        if engine.session and time.monotonic() - engine.updated > 10:
            engine.clear()
    return True


GLib.timeout_add(200, activate)
GLib.timeout_add_seconds(1, expire)
GLib.MainLoop().run()
