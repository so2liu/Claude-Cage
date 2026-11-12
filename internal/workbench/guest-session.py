"""Run the workbench's control scripts over one SSH connection.

Requests: "R <id> <length>\n" followed by the script, or "C <id>\n" to stop
one. Replies: "D <id> <exit> <stdout length> <stderr length>\n" and both.
"""
import os
import signal
import subprocess
import sys
import threading

# Above the caller's 64 KiB limit, so that it can tell an oversized answer.
OUTPUT_LIMIT = 64 * 1024 + 256
ERROR_LIMIT = 4096
SCRIPT_LIMIT = 1 << 20
requests, replies = sys.stdin.buffer, sys.stdout.buffer
writing = threading.Lock()
running = {}


def stop(child):
    try:
        os.killpg(child.pid, signal.SIGKILL)
    except OSError:
        pass


def keep(pipe, limit, exceeded=None):
    """The first `limit` bytes. The rest is read and dropped, or refused."""
    data = b''
    while True:
        chunk = pipe.read1(65536)
        if not chunk:
            return data
        data += chunk[:limit - len(data)]
        if len(data) == limit and exceeded:
            exceeded()
            exceeded = None


def run(ident, script):
    code, output, errors = 127, b'', []
    try:
        child = subprocess.Popen(['/bin/bash', '--noprofile', '--norc', '-c', script], stdin=subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
    except OSError as error:
        errors.append(str(error).encode()[:ERROR_LIMIT])
    else:
        running[ident] = child
        drain = threading.Thread(target=lambda: errors.append(keep(child.stderr, ERROR_LIMIT)), daemon=True)
        drain.start()
        output = keep(child.stdout, OUTPUT_LIMIT, lambda: stop(child))
        code = child.wait()
        drain.join()
        running.pop(ident, None)
    with writing:
        replies.write(b'D %d %d %d %d\n' % (ident, code, len(output), len(errors[0])) + output + errors[0])
        replies.flush()


replies.write(b'CAGE-GUEST-SESSION 1\n')
replies.flush()
while True:
    fields = requests.readline(64).split()
    try:
        if len(fields) == 3 and fields[0] == b'R' and 0 < int(fields[2]) <= SCRIPT_LIMIT:
            script = requests.read(int(fields[2]))
            if len(script) != int(fields[2]):
                break
            threading.Thread(target=run, args=(int(fields[1]), script.decode()), daemon=True).start()
        elif len(fields) == 2 and fields[0] == b'C':
            child = running.get(int(fields[1]))
            if child:
                stop(child)
        else:
            break
    except ValueError:
        break
# The connection closed: nothing is left to receive an answer.
for child in list(running.values()):
    stop(child)
