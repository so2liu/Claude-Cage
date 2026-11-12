"""Read the installed macOS component receipt and verify its recorded files."""
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess


def snapshot(receipt=Path('/var/db/cage-components.json')):
    result = {'manifest': None, 'tools': {}, 'installed': False, 'missing': []}
    try:
        info = receipt.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022 or info.st_size > 4 << 20:
            raise ValueError('invalid receipt')
        value = json.loads(receipt.read_text())
        if (value.get('protocol') != 1 or value.get('system') != 'macos'
                or not isinstance(value.get('files'), list) or not 0 < len(value['files']) <= 10000):
            raise ValueError('invalid manifest')
        for item in value['files']:
            target = item.get('target', '')
            if not target:
                continue
            if ('..' in Path(target).parts or not
                    (target.startswith('/Applications/Cage') or target.startswith('/usr/local/libexec/cage-'))):
                raise ValueError('invalid component target')
            file = Path(target)
            try:
                with file.open('rb') as stream:
                    current = hashlib.file_digest(stream, 'sha256').hexdigest()
                if current != item['installed_sha256'] and len(result['missing']) < 20:
                    result['missing'].append(target)
            except OSError:
                if len(result['missing']) < 20:
                    result['missing'].append(target)
        result['manifest'] = {key: value.get(key) for key in ('protocol', 'revision', 'app_version')}
        if result['missing']:
            result['manifest']['revision'] = ''
        result['installed'] = not result['missing']
    except (OSError, ValueError, KeyError, TypeError):
        result['error'] = '客机尚无可核验的组件记录，请同步环境组件。'
    # Launching code-server, even with --version, can create a user config
    # and print its path before the version on first use.
    editor = Path('/usr/local/libexec/cage-workbench-browser-4.145.0-pw1.65.0/editor')
    result['tools']['code_server'] = ''
    try:
        package = json.loads((editor / 'package.json').read_text())
        if package.get('name') == 'code-server' and isinstance(package.get('version'), str) and os.access(editor / 'bin/code-server', os.X_OK):
            result['tools']['code_server'] = package['version'][:200]
    except (OSError, ValueError, AttributeError):
        pass
    commands = {
        'chromium': ['/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '--version'],
        'claude': ['/usr/local/libexec/cage-cli/bin/claude', '--version'],
        'python': ['/usr/local/libexec/cage-cli/bin/python3', '--version'],
        'git': ['/usr/local/libexec/cage-cli/bin/git', '--version'],
        'gh': ['/usr/local/libexec/cage-cli/bin/gh', '--version'],
        'node': ['/usr/local/libexec/cage-node-v24.22.2/bin/node', '--version'],
    }
    for name, command in commands.items():
        try:
            response = subprocess.run(command, stdin=subprocess.DEVNULL, capture_output=True, timeout=3,
                                      text=True, env={**os.environ, 'DISABLE_AUTOUPDATER': '1'})
            lines = response.stdout.strip().splitlines()
            result['tools'][name] = lines[0][:200] if response.returncode == 0 and lines else ''
        except (OSError, subprocess.SubprocessError):
            result['tools'][name] = ''
    return result


if __name__ == '__main__':
    print(json.dumps(snapshot(), ensure_ascii=False))
