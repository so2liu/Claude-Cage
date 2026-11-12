import importlib.util
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location('display_governor', Path(__file__).with_name('display-governor.py'))
governor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(governor)

try:
    from Xlib import X, display
    from Xlib.ext import xtest
except ImportError:
    display = None


class PolicyTests(unittest.TestCase):
    def test_rate_follows_time_since_input(self):
        self.assertEqual(governor.target_rate(0), 60)
        self.assertEqual(governor.target_rate(2.9), 60)
        self.assertEqual(governor.target_rate(3), 30)
        self.assertEqual(governor.target_rate(179), 30)
        self.assertEqual(governor.target_rate(180), 20)

    def test_wakes_exactly_at_the_next_change(self):
        self.assertAlmostEqual(governor.next_change(1), 2)
        self.assertAlmostEqual(governor.next_change(10), 170)
        self.assertIsNone(governor.next_change(500))

    def test_only_its_own_modes_are_governed(self):
        self.assertTrue(governor.governed('fps30-2339x1368'))
        # Size owners keep, match and delete these names themselves.
        for name in ('cage-2339x1368', 'cage-phone-800x600', '1600x1000', 'fps-1x2', 'fps30-1x', 'fps30', 'fpsx-1x2'):
            self.assertFalse(governor.governed(name), name)

    def test_mode_has_the_requested_refresh_rate(self):
        for rate in (20, 30, 60):
            mode = governor.mode_timing(rate, 2339, 1368)
            self.assertAlmostEqual(mode['dot_clock'] / (mode['h_total'] * mode['v_total']), rate)
            self.assertEqual((mode['width'], mode['height']), (2339, 1368))
            self.assertEqual(mode['name_length'], len(governor.mode_name(rate, 2339, 1368)))

    def test_full_rate_returns_to_the_size_owners_mode(self):
        base = ('cage-800x600', 800, 600)
        passive = ('fps30-800x600', 800, 600)
        self.assertEqual(governor.choose_mode(30, base, None, {base}), ('fps30-800x600', base))
        self.assertEqual(governor.choose_mode(60, passive, base, {base, passive}), ('cage-800x600', base))
        self.assertEqual(governor.choose_mode(60, base, base, {base}), ('cage-800x600', base))

    def test_full_rate_never_restores_another_size(self):
        stale = ('cage-800x600', 800, 600)
        current = ('fps30-1024x768', 1024, 768)
        self.assertEqual(governor.choose_mode(60, current, stale, {stale, current})[0], 'fps60-1024x768')
        # The owner removed its mode while the display was passive.
        self.assertEqual(governor.choose_mode(60, ('fps30-800x600', 800, 600), stale, set())[0], 'fps60-800x600')

    def test_raw_key_reads_the_keycode(self):
        self.assertEqual(governor.raw_key(struct.pack('=HIIHHII', 3, 1234, 250, 3, 0, 0, 0)), 250)
        self.assertIsNone(governor.raw_key(b'short'))
        self.assertIsNone(governor.raw_key(None))


@unittest.skipUnless(display and shutil.which('Xtigervnc') and shutil.which('xrandr'), 'needs Xtigervnc, xrandr and python3-xlib')
class DisplayTests(unittest.TestCase):
    """The governor against a private virtual display."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir='/tmp')
        self.addCleanup(self.temp.cleanup)
        number = next(n for n in range(120, 200) if not os.path.exists(f'/tmp/.X11-unix/X{n}'))
        self.env = dict(os.environ, DISPLAY=f':{number}')
        self.env.pop('XAUTHORITY', None)
        server = subprocess.Popen(['Xtigervnc', f':{number}', '-geometry', '800x600', '-depth', '24', '-rfbport', '-1',
                                   '-rfbunixpath', self.temp.name + '/d.sock', '-nolisten', 'tcp', '-SecurityTypes', 'None'],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.addCleanup(server.wait)
        self.addCleanup(server.terminate)
        for attempt in range(100):
            if os.path.exists(f'/tmp/.X11-unix/X{number}'):
                break
            time.sleep(.05)
        else:
            self.fail('virtual display did not start')
        self.conn = display.Display(f':{number}')
        self.addCleanup(self.conn.close)
        self.root = self.conn.screen().root
        script = (f'import importlib.util, sys\n'
                  f'spec = importlib.util.spec_from_file_location("g", {str(Path(__file__).with_name("display-governor.py"))!r})\n'
                  f'g = importlib.util.module_from_spec(spec); spec.loader.exec_module(g)\n'
                  f'g.ACTIVE_HOLD, g.IDLE_AFTER = .5, 1.5\nsys.exit(g.main())\n')
        process = subprocess.Popen([sys.executable, '-c', script], env=self.env)
        self.addCleanup(process.wait)
        self.addCleanup(process.terminate)
        self.process = process

    def current(self):
        resources = self.root.xrandr_get_screen_resources()
        output = self.conn.xrandr_get_output_info(resources.outputs[0], resources.config_timestamp)
        crtc = self.conn.xrandr_get_crtc_info(output.crtc, resources.config_timestamp)
        position = 0
        for mode in resources.modes:
            name = resources.mode_names[position:position + mode.name_length]
            position += mode.name_length
            if mode.id == crtc.mode:
                return name, round(mode.dot_clock / (mode.h_total * mode.v_total)), (mode.width, mode.height)

    def modes(self):
        return subprocess.check_output(['xrandr', '--query'], env=self.env, text=True)

    def wait(self, rate, size=(800, 600), timeout=4):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            current = self.current()
            if current[1:] == (rate, size):
                return current[0]
            time.sleep(.01)
        self.fail(f'expected {rate} Hz at {size}, display shows {self.current()}')

    def resize(self, width, height):
        # The sequence of the browser and Desktop resize paths.
        name = f'cage-{width}x{height}'
        if f' {name} ' not in self.modes():
            subprocess.check_call(['xrandr', '--newmode', name, f'{(width + 32) * (height + 8) * 60 / 1000000:.3f}',
                                   *map(str, (width, width + 8, width + 16, width + 32, height, height + 1, height + 2, height + 8)),
                                   '+HSync', '-VSync'], env=self.env)
            subprocess.check_call(['xrandr', '--addmode', 'VNC-0', name], env=self.env)
        subprocess.check_call(['xrandr', '--output', 'VNC-0', '--mode', name], env=self.env)

    def test_unattended_display_slows_and_input_restores_it(self):
        original = self.current()[0]
        self.assertEqual(self.wait(30), 'fps30-800x600')
        self.assertEqual(self.wait(20), 'fps20-800x600')
        started = time.monotonic()
        xtest.fake_input(self.conn, X.MotionNotify, x=40, y=40)
        self.conn.sync()
        self.assertEqual(self.wait(60), original)
        self.assertLess(time.monotonic() - started, .5)
        self.wait(30)
        code = self.conn.keysym_to_keycode(0x61)
        xtest.fake_input(self.conn, X.KeyPress, code)
        xtest.fake_input(self.conn, X.KeyRelease, code)
        self.conn.sync()
        self.assertEqual(self.wait(60), original)

    def test_caret_probe_key_is_not_viewer_input(self):
        subprocess.check_call(['xmodmap', '-e', 'keycode 250 = F24'], env=self.env)
        self.wait(20)
        for _ in range(3):
            xtest.fake_input(self.conn, X.KeyPress, 250)
            xtest.fake_input(self.conn, X.KeyRelease, 250)
            self.conn.sync()
            time.sleep(.2)
            self.assertEqual(self.current()[1], 20)

    def test_resize_keeps_its_size_and_removes_earlier_modes(self):
        self.wait(30)
        self.resize(1024, 700)
        self.assertEqual(self.wait(60, (1024, 700)), 'cage-1024x700')
        self.assertEqual(self.wait(30, (1024, 700)), 'fps30-1024x700')
        self.assertNotIn('fps30-800x600', self.modes())
        self.assertNotIn('fps20-800x600', self.modes())
        # A viewer attaching at the same size restores the full rate as well.
        self.resize(1024, 700)
        self.assertEqual(self.wait(60, (1024, 700)), 'cage-1024x700')
        # Each step of a drag stays at the size its owner selected.
        self.wait(30, (1024, 700))
        for width in range(1030, 1100, 10):
            self.resize(width, 700)
            time.sleep(.05)
            self.assertEqual(self.current()[1:], (60, (width, 700)))
        self.assertEqual(self.wait(30, (1090, 700)), 'fps30-1090x700')
        self.assertIsNone(self.process.poll())

    def test_stopping_restores_the_full_rate(self):
        original = self.current()[0]
        self.wait(30)
        self.process.terminate()
        self.assertEqual(self.process.wait(timeout=3), 0)
        self.assertEqual(self.current()[:2], (original, 60))


if __name__ == '__main__':
    unittest.main()
