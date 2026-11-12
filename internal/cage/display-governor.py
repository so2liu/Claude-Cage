#!/usr/bin/python3
"""Pace a Cage virtual display by what its viewer is doing.

Chromium and Electron render in software here, and take their frame interval
from the display's RandR refresh rate. Rendering, capture, encoding, transport
and host decoding therefore all scale with that one number. Input restores the
full rate within a few frames; an unattended animation runs at a lower one.
"""
import os
import select
import signal
import struct
import sys
import time

ACTIVE_RATE = 60
PASSIVE_RATE = 30
IDLE_RATE = 20
ACTIVE_HOLD = 3.0
IDLE_AFTER = 180.0
# Size owners name their modes cage-<w>x<h> and cage-phone-<w>x<h>, and remove
# only those. This prefix keeps the two sets of modes apart.
PREFIX = 'fps'
PROBE_KEYSYM = 0xffd5  # F24, injected by the input bridge to refresh a caret.


def target_rate(idle):
    if idle < ACTIVE_HOLD:
        return ACTIVE_RATE
    if idle < IDLE_AFTER:
        return PASSIVE_RATE
    return IDLE_RATE


def next_change(idle):
    """Seconds until the target rate changes without further input."""
    if idle < ACTIVE_HOLD:
        return ACTIVE_HOLD - idle
    if idle < IDLE_AFTER:
        return IDLE_AFTER - idle
    return None


def mode_name(rate, width, height):
    return f'{PREFIX}{rate}-{width}x{height}'


def governed(name):
    rate, separator, size = name[len(PREFIX):].partition('-')
    width, by, height = size.partition('x')
    return name.startswith(PREFIX) and bool(separator and by) and rate.isdigit() and width.isdigit() and height.isdigit()


def mode_timing(rate, width, height):
    # The same blanking the resize paths use for their 60 Hz modes.
    return {
        'id': 0, 'width': width, 'height': height, 'dot_clock': (width + 32) * (height + 8) * rate,
        'h_sync_start': width + 8, 'h_sync_end': width + 16, 'h_total': width + 32, 'h_skew': 0,
        'v_sync_start': height + 1, 'v_sync_end': height + 2, 'v_total': height + 8,
        'name_length': len(mode_name(rate, width, height)), 'flags': 1 | 8,  # +HSync -VSync
    }


def raw_key(data):
    """Keycode of an XInput2 raw key event: deviceid, time, then detail."""
    return struct.unpack_from('=I', data, 6)[0] if isinstance(data, (bytes, bytearray)) and len(data) >= 10 else None


def choose_mode(rate, current, base, available):
    """Mode to show `rate` at the current size, and the remembered base mode.

    The full rate returns to the mode its size owner selected, so an attended
    display is configured exactly as it is without this governor.
    """
    name, width, height = current
    if not governed(name):
        base = current
    if rate == ACTIVE_RATE and base is not None and base[1:] == (width, height) and base in available:
        return base[0], base
    return mode_name(rate, width, height), base


class Governor:
    def __init__(self, connection, output='VNC-0'):
        from Xlib.ext import randr, xinput
        self.conn = connection
        self.root = connection.screen().root
        self.output_name = output
        self.xinput = xinput
        self.expected = None
        self.base = None
        self.probe_codes = None
        # Requests without a reply report their errors here. A mode another
        # client still uses is simply left in place.
        connection.set_error_handler(lambda *_: None)
        self.root.xrandr_select_input(randr.RRScreenChangeNotifyMask)
        self.root.xinput_select_events([(xinput.AllMasterDevices,
                                         xinput.RawKeyPressMask | xinput.RawButtonPressMask | xinput.RawMotionMask)])
        connection.sync()

    def state(self):
        resources = self.root.xrandr_get_screen_resources()
        modes, position = {}, 0
        for mode in resources.modes:
            modes[mode.id] = (resources.mode_names[position:position + mode.name_length], mode.width, mode.height)
            position += mode.name_length
        for output in resources.outputs:
            info = self.conn.xrandr_get_output_info(output, resources.config_timestamp)
            if info.name == self.output_name and info.crtc:
                crtc = self.conn.xrandr_get_crtc_info(info.crtc, resources.config_timestamp)
                if crtc.mode in modes:
                    return resources, output, info, crtc, modes
        return None

    def ensure_mode(self, rate, width, height):
        state = self.state()
        if state is None:
            return
        _, output, info, _, modes = state
        name = mode_name(rate, width, height)
        mode = next((key for key, value in modes.items() if value[0] == name), None)
        if mode is None:
            mode = self.root.xrandr_create_mode(mode_timing(rate, width, height), name).mode
        if mode not in info.modes:
            self.conn.xrandr_add_output_mode(output, mode)
        self.conn.sync()

    def apply(self, rate):
        """Show `rate` at the current size and report 'shown' or 'failed'.

        Reports 'foreign', leaving the display alone, when another client
        changed the mode since the previous call: a viewer attached or resized.
        """
        from Xlib import error
        created = False
        while True:
            # A size owner may change the mode at any moment. Deciding and
            # switching inside one server grab cannot restore a stale size.
            self.conn.grab_server()
            try:
                state = self.state()
                if state is None:
                    return 'failed'
                resources, output, info, crtc, modes = state
                current = modes[crtc.mode]
                foreign = self.expected is not None and current != self.expected
                wanted, self.base = choose_mode(rate, current, self.base, set(modes.values()))
                if foreign:
                    self.expected = current
                    return 'foreign'
                target = next((key for key, value in modes.items() if value[0] == wanted and key in info.modes), None)
                if wanted == current[0] or target is not None:
                    if wanted != current[0]:
                        reply = self.conn.xrandr_set_crtc_config(info.crtc, resources.config_timestamp, crtc.x, crtc.y,
                                                                 target, crtc.rotation, crtc.outputs)
                        if reply.status != 0:
                            return 'failed'
                    self.expected = (wanted,) + current[1:]
                    for mode, (name, width, height) in modes.items():
                        # Modes of earlier sizes. Those of this size are reused.
                        if governed(name) and (width, height) != current[1:]:
                            if mode in info.modes:
                                self.conn.xrandr_delete_output_mode(output, mode)
                            self.conn.xrandr_destroy_mode(mode)
                    return 'shown'
            except error.XError:
                return 'failed'
            finally:
                self.conn.ungrab_server()
                self.conn.sync()
            if created:
                return 'failed'
            try:
                self.ensure_mode(rate, *current[1:])
            except error.XError:
                return 'failed'
            created = True

    def probe_key(self, code):
        if self.probe_codes is None:
            first = self.conn.display.info.min_keycode
            count = self.conn.display.info.max_keycode - first + 1
            self.probe_codes = {first + offset for offset, symbols in
                                enumerate(self.conn.get_keyboard_mapping(first, count))
                                if len(symbols) and symbols[0] == PROBE_KEYSYM}
        return code in self.probe_codes

    def events(self):
        """Whether the viewer acted, and whether the screen was reconfigured."""
        from Xlib import X
        from Xlib.ext import ge
        acted = changed = False
        while self.conn.pending_events():
            event = self.conn.next_event()
            if event.type == X.MappingNotify:
                self.probe_codes = None
            elif event.type != ge.GenericEventCode:
                changed = True
            elif event.evtype != self.xinput.RawKeyPress or not self.probe_key(raw_key(event.data)):
                acted = True
        return acted, changed

    def run(self, clock=time.monotonic):
        last_input, shown = clock(), None
        while True:
            acted, changed = self.events()
            if acted:
                last_input = clock()
            rate = target_rate(clock() - last_input)
            # Continuous input at the full rate needs no display request.
            if changed or rate != shown:
                result = self.apply(rate)
                if result == 'foreign':
                    last_input, rate = clock(), ACTIVE_RATE
                    result = self.apply(rate)
                shown = rate if result == 'shown' else None
            if acted:
                # Input arrives in bursts; one wake-up covers each of them.
                time.sleep(.25)
                continue
            if self.conn.pending_events():
                continue
            wait = next_change(clock() - last_input) if shown is not None else 5
            select.select([self.conn.fileno()], [], [], None if wait is None else wait + .01)


def main():
    try:
        from Xlib import display
    except ImportError:
        # Optional: the display keeps its full rate until python3-xlib arrives.
        print('display governor: python3-xlib is not installed', file=sys.stderr)
        return 0
    for attempt in range(40):
        try:
            connection = display.Display()
            break
        except Exception:
            # Started together with its display, which may not listen yet.
            if attempt == 39:
                raise
            time.sleep(.25)
    if not connection.has_extension('RANDR') or not connection.has_extension('XInputExtension'):
        # Without both extensions the display simply keeps its full rate.
        print('display governor: RandR or XInput2 unavailable', file=sys.stderr)
        return 0
    connection.xinput_query_version()
    governor = Governor(connection, os.environ.get('CAGE_DISPLAY_OUTPUT', 'VNC-0'))
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    try:
        governor.run()
    finally:
        # A stopped governor leaves the display at its full rate.
        try:
            if governor.apply(ACTIVE_RATE) == 'foreign':
                governor.apply(ACTIVE_RATE)
        except Exception:
            pass
    return 0


if __name__ == '__main__':
    sys.exit(main())
