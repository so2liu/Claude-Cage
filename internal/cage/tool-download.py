"""Run curl with resumable ranges; stdout remains curl's HTTP status code."""
import concurrent.futures
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time

artifact = Path(sys.argv[1])
arguments = sys.argv[2:]
idle_timeout = 60
name = artifact.name.removesuffix('.part').removesuffix('.mirror')
show_progress = os.environ.get('CAGE_SETUP_PROGRESS') == '1'


def size(path):
    try:
        return path.stat().st_size
    except FileNotFoundError:
        return 0


def report(count):
    if show_progress:
        print(f'CAGE_TOOL_BYTES {name} {count}', file=sys.stderr, flush=True)


def stop(signum, frame):
    raise SystemExit(128 + signum)


signal.signal(signal.SIGTERM, stop)


def terminate(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def headers(path):
    try:
        text = path.read_text(encoding='iso-8859-1')
    except FileNotFoundError:
        return {}
    # Ignore CONNECT and redirect headers. Only complete header blocks count.
    blocks = text.replace('\r\n', '\n').split('\n\n')
    result = {}
    for block in blocks[:-1]:
        lines = block.splitlines()
        if lines and lines[0].startswith('HTTP/'):
            if lines[0].lower().endswith('connection established'):
                continue
            result = {'status': lines[0].split()[1]}
            for line in lines[1:]:
                key, sep, value = line.partition(':')
                if sep:
                    result[key.lower()] = value.strip()
    return result


# Keep all caller options, including TLS and proxy handling, but replace output
# and resume options for bounded requests. No custom headers identify this tool.
base = []
i = 0
while i < len(arguments):
    if arguments[i] in ('-o', '--continue-at', '--write-out'):
        i += 2
    else:
        base.append(arguments[i])
        i += 1


def parallel():
    # HEAD does not consume a body and avoids restarting an active transfer.
    # A missing length, unsupported HEAD, or small file uses the original path.
    with tempfile.TemporaryDirectory(prefix='download-') as temporary:
        head = Path(temporary) / 'headers'
        probe = subprocess.Popen(['curl', *base, '--silent', '--show-error', '--head',
                                  '--max-time', '15', '--dump-header', str(head), '-o', os.devnull],
                                 stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                 stderr=subprocess.DEVNULL)
        try:
            status = probe.wait()
        finally:
            terminate(probe)
        info = headers(head)
        if status or info.get('status') != '200' or not info.get('content-length', '').isdigit():
            return False
        total = int(info['content-length'])
        offset = size(artifact)
        root = Path(str(artifact) + '.ranges')
        if (total <= offset or (total - offset < 8 * 1024 * 1024 and not root.is_dir())
                or info.get('content-encoding', 'identity') != 'identity'):
            return False
        root.mkdir(mode=0o700, exist_ok=True)
        if root.is_symlink():
            raise RuntimeError('Invalid download cache directory')
        parts = []
        width = (total + 3) // 4
        for index in range(4):
            start, end = max(offset, index * width), min(total, (index + 1) * width) - 1
            if start <= end:
                part = root / f'{start}-{end}'
                if part.is_symlink() or (part.exists() and not part.is_file()):
                    raise RuntimeError('Invalid download cache file')
                parts.append((start, end, part))
        print('Downloading with up to 4 connections.', file=sys.stderr)

        canceled = threading.Event()
        cache_lock = threading.Lock()

        def fetch(spec):
            start, end, part = spec
            saved = size(part)
            if saved > end - start + 1:
                part.unlink()
                saved = 0
            if saved == end - start + 1:
                return True
            incoming = Path(temporary) / part.name
            response_headers = Path(str(incoming) + '.headers')
            process = subprocess.Popen(['curl', *base, '--silent', '--show-error', '--no-buffer',
                                        '--range', f'{start + saved}-{end}', '--dump-header', str(response_headers),
                                        '-o', str(incoming)], stdin=subprocess.DEVNULL,
                                       stdout=subprocess.DEVNULL)
            last_size, last_activity = 0, time.monotonic()
            try:
                while not canceled.is_set() and process.poll() is None:
                    current = size(incoming)
                    if current > last_size:
                        last_activity = time.monotonic()
                    last_size = current
                    response = headers(response_headers)
                    code = response.get('status')
                    if code and code not in ('200', '206', '301', '302', '303', '307', '308'):
                        break
                    if code == '200':
                        break  # A server/proxy ignored Range; never append its body.
                    if code == '206' and (response.get('content-range') != f'bytes {start + saved}-{end}/{total}'
                                          or response.get('content-encoding', 'identity') != 'identity'):
                        break
                    if current > end - start + 1 - saved:
                        break
                    if time.monotonic() - last_activity >= idle_timeout:
                        break
                    time.sleep(0.05)
            finally:
                terminate(process)
                response = headers(response_headers)
                valid = (response.get('status') == '206'
                         and response.get('content-range') == f'bytes {start + saved}-{end}/{total}'
                         and response.get('content-encoding', 'identity') == 'identity'
                         and size(incoming) <= end - start + 1 - saved)
                if valid and incoming.exists():
                    with cache_lock:
                        with part.open('ab') as destination, incoming.open('rb') as source:
                            shutil.copyfileobj(source, destination)
                        incoming.unlink()
            return valid and size(part) == end - start + 1

        # Cancellation must join all workers before merging or returning. Threads
        # see the shared event and terminate curl; they cannot outlive the lock.
        def run(spec):
            if canceled.is_set():
                return False
            return fetch(spec)

        pool = concurrent.futures.ThreadPoolExecutor(max_workers=4)
        futures = []
        try:
            futures = [pool.submit(run, spec) for spec in parts]
            while not all(f.done() for f in futures):
                with cache_lock:
                    report(offset + sum(size(p) + size(Path(temporary) / p.name) for _, _, p in parts))
                time.sleep(0.1)
            success = all(f.result() for f in futures)
        finally:
            canceled.set()
            pool.shutdown(wait=True)
            # Only concatenate the contiguous prefix. Later completed ranges
            # remain separately cached, never as holes inside the resume file.
            with artifact.open('ab') as destination:
                for start, end, part in parts:
                    if size(artifact) != start:
                        break
                    if part.exists():
                        with part.open('rb') as source:
                            shutil.copyfileobj(source, destination)
                        destination.flush()
                        part.unlink()
                    if size(artifact) != end + 1:
                        break
            report(size(artifact))
        if success:
            # All bytes will still pass the caller's fixed hash before use.
            print('200', end='')
            shutil.rmtree(root)
            return True
        print('Range download unavailable or interrupted; continuing with one connection.', file=sys.stderr)
        return False


if parallel():
    sys.exit(0)

received = size(artifact)
report(received)
last_received = time.monotonic()
meter = ['--silent', '--show-error'] if show_progress else []
process = subprocess.Popen(['curl', '--no-buffer', *meter, *arguments], stdin=subprocess.DEVNULL)
try:
    while True:
        try:
            status = process.wait(timeout=min(1, idle_timeout))
            report(size(artifact))
            break
        except subprocess.TimeoutExpired:
            current = size(artifact)
            if current > received:
                report(current)
                last_received = time.monotonic()
            received = current
            if time.monotonic() - last_received >= idle_timeout:
                print('Download received no data for 60 seconds; retrying with saved partial data.', file=sys.stderr)
                status = 28
                break
finally:
    terminate(process)
sys.exit(status if status >= 0 else 128 - status)
