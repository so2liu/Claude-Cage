"""Exercise production installers in isolated directories with OS-service substitutes.

The filesystem, file hashes, CLI publication, rollback, and macOS lockf run for
real. Virtual-machine/root checks, signatures, and launchd use explicit doubles.
No command addresses an installed application or an actual guest.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import tempfile
import time
import unittest
import zipfile
from unittest.mock import patch


SOURCE = Path(__file__).resolve().parents[1]
REPO = SOURCE.parents[1]


class NativeMaintenance(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix='cage-native-maintenance-')
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.kit = self.root / 'kit'
        self.kit.mkdir()
        for name in ('Applications', 'usr/local/libexec', 'private/var/tmp', 'var/db', 'var/run', 'Users/cage'):
            (self.root / name).mkdir(parents=True, exist_ok=True)
        self.write('var/db/cage-guest-os', 'macos')
        self.write('Users/cage/account.json', 'account-and-login-marker')
        self.write('Users/cage/project.txt', 'project-marker')

    def write(self, name, data, mode=0o644):
        filename = self.root / name
        filename.parent.mkdir(parents=True, exist_ok=True)
        filename.write_text(data)
        filename.chmod(mode)
        return filename

    def translate(self, source):
        for directory in ('/usr/local/libexec', '/usr/local/bin', '/Applications', '/private/var/tmp', '/var/db', '/var/run', '/Users'):
            source = source.replace(directory, str(self.root) + directory)
        source = source.replace('$EUID == 0', '$EUID == ' + str(os.getuid()))
        return source

    def prepare_installer(self):
        if not Path('/usr/bin/lockf').is_file():
            self.skipTest('The production lockf check requires a Mac host')
        runtime = 'usr/local/libexec/cage-cli-runtime-2.1.311-py3.13.18-git2.56.0/bin'
        for name in ('python3', 'claude', 'git'):
            version = '2.1.311 (Claude Code)' if name == 'claude' else 'fixture version'
            self.write(f'{runtime}/{name}', f'#!/bin/sh\nprintf "{version}\\n"\n', 0o755)
        self.write('usr/local/libexec/cage-cli-2.previous/marker', 'previous-helpers')
        os.symlink(self.root / 'usr/local/libexec/cage-cli-2.previous', self.root / 'usr/local/libexec/cage-cli')
        self.write('usr/local/libexec/cage-sshfs', 'previous-project-helper')
        self.write('Applications/Cage Guest Window.app/Contents/MacOS/CageGuestWindow', 'previous-window')
        helper_sources = ('macos-cli.py', 'cli_channel.py', 'cli_channel.resume.md', 'claude_account_resume.md', 'claude-account.py')
        files = []
        for name in helper_sources:
            self.write('kit/' + name, '#!fixture\ncurrent-' + name)
            target = 'usr/local/libexec/cage-cli/' + name
            if name == 'claude_account_resume.md':
                target = 'usr/local/libexec/cage-cli/cli_channel.account-resume.md'
            if name == 'claude-account.py':
                target = 'usr/local/libexec/cage-cli/bin/cage-claude-account'
            files.append((name, target))
        for app, executable in (('Window', 'CageGuestWindow'), ('Clipboard', 'CageGuestClipboard')):
            name = f'Cage Guest {app}.app/Contents/MacOS/{executable}'
            self.write('kit/' + name, 'current-' + app, 0o755)
            files.append((name, 'Applications/' + name))
        for source, target in (
                ('project/cage-sshfs', 'usr/local/libexec/cage-sshfs'),
                ('native-browser/macos-phone-browser.mjs', 'usr/local/libexec/cage-workbench-browser-4.145.0-pw1.65.0/controller/macos-phone-browser.mjs')):
            self.write('kit/' + source, 'current-' + source)
            files.append((source, target))
        for label, source in (('claude-window', 'register-macos-claude-window.sh'), ('clipboard', 'register-macos-clipboard.sh'),
                              ('browser', 'register-macos-browser.sh')):
            # Source deliberately has mode 0644, as the clipboard source does.
            self.write('kit/' + source, '''#!/bin/sh
if [ -n "${CAGE_TEST_HOLD:-}" ]; then
    touch "$CAGE_TEST_HOLD/ready"
    while [ -d "$CAGE_TEST_HOLD" ]; do sleep 0.05; done
fi
exit "${CAGE_TEST_FAIL_REGISTER:-0}"
''')
            files.append((source, 'usr/local/libexec/cage-register-' + label))
        perl = self.translate((SOURCE / 'native-component-manifest.pl').read_text())
        perl = perl.replace('^/(?:Applications/', '^' + re.escape(str(self.root)) + '/(?:Applications/')
        perl = perl.replace('$> == 0', '$> == ' + str(os.getuid()))
        self.write('kit/native-component-manifest.pl', perl)
        self.write('kit/install-macos-cli.sh', self.translate((SOURCE / 'install-macos-cli.sh').read_text()))
        script = self.translate((SOURCE / 'maintain-macos-workbench.sh').read_text())
        script = script.replace('/usr/libexec/PlistBuddy', str(self.root / 'plist-buddy'))
        script = script.replace('/bin/launchctl', str(self.root / 'launchctl'))
        self.write('kit/maintain-macos-workbench.sh', script)
        self.write('plist-buddy', '#!/bin/sh\necho dev.cage.fixture\n', 0o755)
        self.write('launchctl', '''#!/bin/sh
if [ -n "${CAGE_TEST_LOGIN_WINDOW:-}" ]; then exit 1; fi
if [ "$1" = bootstrap ] && [ -n "${CAGE_TEST_FAIL_ACTIVATION:-}" ] && [ ! -f "$CAGE_TEST_FAIL_ACTIVATION" ]; then
    touch "$CAGE_TEST_FAIL_ACTIVATION"
    exit 1
fi
exit 0
''', 0o755)
        self.write('environment.sh', '''
uname() { if [[ $1 == -s ]]; then printf 'Darwin\\n'; else printf 'Darwin arm64\\n'; fi; }
sysctl() { printf 'VirtualMac2,1\\n'; }
id() { if [[ $1 == -u ]]; then echo 501; else echo staff; fi; }
stat() {
    if [[ $* == *'/dev/console'* ]]; then if [[ -n ${CAGE_TEST_LOGIN_WINDOW:-} ]]; then echo 0; else echo 501; fi
    elif [[ $* == *'/Users/cage/Library/LaunchAgents/'* ]]; then echo 501
    elif [[ $2 == *Lp || $2 == *Op ]]; then echo 755
    else echo 0; fi
}
find() { :; }
codesign() { if [[ $1 == -dv ]]; then echo 'TeamIdentifier=FIXTURE'; fi; }
ditto() { cp -R "$1" "$2"; }
sudo() { shift 2; "$@"; }
ps() { :; }
''')
        entries = []
        for source, target in files:
            contents = (self.kit / source).read_bytes()
            installed = contents
            if source == 'claude-account.py':
                installed = (f'#!{self.root}/{runtime}/python3 -I\n').encode() + contents.split(b'\n', 1)[1]
            entries.append(dict(source=source, target=str(self.root / target), sha256=hashlib.sha256(contents).hexdigest(), installed_sha256=hashlib.sha256(installed).hexdigest()))
        manifest = dict(protocol=1, system='macos', app_version='fixture', revision='a' * 64, files=entries)
        self.write('kit/native-components.json', json.dumps(manifest))
        return manifest

    def run_install(self, fail=False):
        env = {**os.environ, 'BASH_ENV': str(self.root / 'environment.sh'), 'CAGE_TEST_FAIL_REGISTER': '1' if fail else '0'}
        return subprocess.run(['/bin/bash', str(self.kit / 'maintain-macos-workbench.sh'), str(self.kit), 'cage', 'components'],
                              capture_output=True, text=True, env=env, timeout=15)

    def test_cli_tool_update_adds_github_and_preserves_login(self):
        self.prepare_installer()
        script = (self.kit / 'install-macos-cli.sh').read_text()
        # Keep the real installer, ZIP layout, executable and hash checks.
        # Large already-installed runtime packages use small fixed fixtures.
        for name in ('claude', 'uv.tar.gz', 'python.tar.gz', 'git.tar.gz'):
            contents = '#!/bin/sh\nprintf "2.1.322 (Claude Code)\\n"\n' if name == 'claude' else 'runtime-package-fixture'
            package = self.write('kit/' + name, contents)
            checksum = hashlib.sha256(package.read_bytes()).hexdigest()
            script = re.sub(r'check [a-f0-9]{64} ' + re.escape(name) + r'\n', f'check {checksum} {name}\n', script)
            if name == 'claude':
                self.write('kit/claude-manifest.json', json.dumps({'version': '2.1.322', 'platforms': {'darwin-arm64': {'binary': 'claude', 'checksum': checksum}}}))
        archive = self.kit / 'gh.zip'
        with zipfile.ZipFile(archive, 'w') as output:
            info = zipfile.ZipInfo('gh_2.103.3_macOS_arm64/bin/gh')
            info.external_attr = 0o100755 << 16
            output.writestr(info, '#!/bin/sh\nprintf "gh version 2.103.3 (fixture)\\n"\n')
            output.writestr('gh_2.103.3_macOS_arm64/LICENSE', 'license-fixture')
        checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
        script = re.sub(r'check [a-f0-9]{64} gh.zip\n', f'check {checksum} gh.zip\n', script)
        self.write('kit/install-macos-cli.sh', script)
        auth = self.write('Users/cage/.config/gh/hosts.yml', 'login-marker', 0o600)
        env = {**os.environ, 'BASH_ENV': str(self.root / 'environment.sh')}
        for _ in range(2):
            result = subprocess.run(['/bin/bash', str(self.kit / 'install-macos-cli.sh'), str(self.kit)],
                                    capture_output=True, text=True, env=env, timeout=15)
            self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
            gh = self.root / 'usr/local/libexec/cage-cli/bin/gh'
            self.assertIn('gh version 2.103.3', subprocess.check_output([gh], text=True))
            self.assertEqual(auth.read_text(), 'login-marker')
            self.assertEqual((self.root / 'usr/local/libexec/cage-gh-2.103.3/LICENSE').read_text(), 'license-fixture')
            self.assertIn('2.1.322', subprocess.check_output([self.root / 'usr/local/libexec/cage-cli/bin/claude', '--version'], text=True))
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / 'usr/local/libexec/cage-cli/bin/gh').is_file())
        self.assertEqual(auth.read_text(), 'login-marker')
        current = os.readlink(self.root / 'usr/local/libexec/cage-cli')
        older = self.write('kit/claude', '#!/bin/sh\nprintf "2.1.311 (Claude Code)\\n"\n')
        metadata = {'version': '2.1.311', 'platforms': {'darwin-arm64': {'binary': 'claude', 'checksum': hashlib.sha256(older.read_bytes()).hexdigest()}}}
        self.write('kit/claude-manifest.json', json.dumps(metadata))
        result = subprocess.run(['/bin/bash', str(self.kit / 'install-macos-cli.sh'), str(self.kit)],
                                capture_output=True, text=True, env=env, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertIn('2.1.322', subprocess.check_output([self.root / 'usr/local/libexec/cage-cli/bin/claude', '--version'], text=True))
        self.assertEqual(os.readlink(self.root / 'usr/local/libexec/cage-cli'), current)
        metadata['platforms']['darwin-arm64']['checksum'] = '0' * 64
        self.write('kit/claude-manifest.json', json.dumps(metadata))
        result = subprocess.run(['/bin/bash', str(self.kit / 'install-macos-cli.sh'), str(self.kit)],
                                capture_output=True, text=True, env=env, timeout=15)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(os.readlink(self.root / 'usr/local/libexec/cage-cli'), current)
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(os.readlink(self.root / 'usr/local/libexec/cage-cli'), current)
        self.assertEqual(auth.read_text(), 'login-marker')

    def test_desktop_install_keeps_a_newer_signed_application(self):
        self.prepare_installer()
        application = self.root / 'Applications/Claude.app'
        properties = {'CFBundleIdentifier': 'com.anthropic.claudefordesktop', 'CFBundleShortVersionString': '5.1231.3', 'CFBundleVersion': '123'}
        self.write('Applications/Claude.app/Contents/Info.plist', json.dumps(properties))
        self.write('Applications/Claude.app/account-marker', 'keep existing application')
        original = (application / 'Contents/Info.plist').read_bytes()
        older = {**properties, 'CFBundleShortVersionString': '5.1231.2', 'CFBundleVersion': '122'}
        self.write('older.plist', json.dumps(older))
        with (self.root / 'environment.sh').open('a') as environment:
            environment.write('''
hdiutil() {
    if [[ $1 == attach ]]; then
        local previous='' argument destination=''
        for argument in "$@"; do
            if [[ $previous == -mountpoint ]]; then destination=$argument; fi
            previous=$argument
        done
        mkdir -p "$destination/Claude.app/Contents"
        cp "$FIXTURE_DESKTOP_PLIST" "$destination/Claude.app/Contents/Info.plist"
    fi
}
''')
        script = self.write('kit/claude-desktop-macos.sh', self.translate((SOURCE / 'claude-desktop-macos.sh').read_text()))
        dmg = self.write('Claude.dmg', 'media fixture')
        result = subprocess.run(['/bin/bash', str(script), 'install', str(dmg)],
                                env={**os.environ, 'BASH_ENV': str(self.root / 'environment.sh'), 'FIXTURE_DESKTOP_PLIST': str(self.root / 'older.plist')},
                                text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertEqual((application / 'Contents/Info.plist').read_bytes(), original)
        self.assertEqual((application / 'account-marker').read_text(), 'keep existing application')
        self.assertEqual((self.root / 'Users/cage/account.json').read_text(), 'account-and-login-marker')

    def test_existing_guest_failure_rollback_retry_and_new_helpers(self):
        manifest = self.prepare_installer()
        previous = os.readlink(self.root / 'usr/local/libexec/cage-cli')
        failed = self.run_install(fail=True)
        self.assertNotEqual(failed.returncode, 0, failed.stdout)
        self.assertEqual((self.root / 'usr/local/libexec/cage-sshfs').read_text(), 'previous-project-helper')
        self.assertEqual((self.root / 'Applications/Cage Guest Window.app/Contents/MacOS/CageGuestWindow').read_text(), 'previous-window')
        self.assertFalse((self.root / 'Applications/Cage Guest Clipboard.app').exists())
        self.assertEqual(os.readlink(self.root / 'usr/local/libexec/cage-cli'), previous)
        self.assertFalse((self.root / 'var/db/cage-components.json').exists())
        for attempt in range(2):
            result = self.run_install()
            self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
            installed = json.loads((self.root / 'var/db/cage-components.json').read_text())
            self.assertEqual(installed, manifest)
            self.assertEqual((self.root / 'Users/cage/account.json').read_text(), 'account-and-login-marker')
            self.assertEqual((self.root / 'Users/cage/project.txt').read_text(), 'project-marker')
            self.assertTrue(os.access(self.root / 'usr/local/libexec/cage-register-clipboard', os.X_OK))
        old_receipt = (self.root / 'var/db/cage-components.json').read_bytes()
        original_source = (self.kit / 'project/cage-sshfs').read_bytes()
        self.write('kit/project/cage-sshfs', 'next-project-helper')
        changed_manifest = json.loads((self.kit / 'native-components.json').read_text())
        changed_manifest['revision'] = 'b' * 64
        for entry in changed_manifest['files']:
            if entry['source'] == 'project/cage-sshfs':
                entry['sha256'] = entry['installed_sha256'] = hashlib.sha256(b'next-project-helper').hexdigest()
        self.write('kit/native-components.json', json.dumps(changed_manifest))
        failed = self.run_install(fail=True)
        self.assertNotEqual(failed.returncode, 0)
        self.assertEqual((self.root / 'var/db/cage-components.json').read_bytes(), old_receipt)
        self.assertEqual((self.root / 'usr/local/libexec/cage-sshfs').read_bytes(), original_source)
        (self.kit / 'project/cage-sshfs').write_bytes(original_source)
        self.write('kit/native-components.json', json.dumps(manifest))
        # A damaged immutable helper directory must be repairable at the same revision.
        (self.root / 'usr/local/libexec/cage-cli/macos-cli.py').write_text('damaged')
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertIn('current-macos-cli.py', (self.root / 'usr/local/libexec/cage-cli/macos-cli.py').read_text())

    def test_canceled_update_restores_files_and_can_retry(self):
        self.prepare_installer()
        hold = self.root / 'hold'
        hold.mkdir()
        env = {**os.environ, 'BASH_ENV': str(self.root / 'environment.sh'), 'CAGE_TEST_HOLD': str(hold)}
        process = subprocess.Popen(['/bin/bash', str(self.kit / 'maintain-macos-workbench.sh'), str(self.kit), 'cage', 'components'],
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=env, start_new_session=True)
        try:
            deadline = time.monotonic() + 5
            while not (hold / 'ready').exists() and time.monotonic() < deadline and process.poll() is None:
                time.sleep(0.01)
            self.assertTrue((hold / 'ready').exists())
            os.killpg(process.pid, signal.SIGTERM)
            process.communicate(timeout=5)
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate()
        self.assertNotEqual(process.returncode, 0)
        self.assertEqual((self.root / 'usr/local/libexec/cage-sshfs').read_text(), 'previous-project-helper')
        self.assertFalse((self.root / 'var/db/cage-components.json').exists())
        retry = self.run_install()
        self.assertEqual(retry.returncode, 0, retry.stderr)

    def test_logged_out_guest_publishes_without_gui_domain(self):
        manifest = self.prepare_installer()
        with patch.dict(os.environ, {'CAGE_TEST_LOGIN_WINDOW': '1'}):
            result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads((self.root / 'var/db/cage-components.json').read_text()), manifest)

    def test_source_mismatch_is_rejected_before_modification(self):
        self.prepare_installer()
        self.write('kit/project/cage-sshfs', 'unexpected-source')
        result = self.run_install()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.root / 'usr/local/libexec/cage-sshfs').read_text(), 'previous-project-helper')
        self.assertFalse((self.root / 'Applications/Cage Guest Clipboard.app').exists())

    def test_activation_failure_restores_files_before_receipt(self):
        manifest = self.prepare_installer()
        agents = self.root / 'Users/cage/Library/LaunchAgents'
        agents.mkdir(parents=True)
        original = agents / 'dev.cage.GuestClaudeWindow.plist'
        original.write_text('original-socket-lifecycle')
        clipboard = agents / 'dev.cage.GuestClipboard.plist'
        for source, target in (('register-macos-claude-window.sh', original),
                               ('register-macos-clipboard.sh', clipboard)):
            content = '#!/bin/sh\nprintf "migrated-lifecycle\\n" > ' + shlex.quote(str(target)) + '\n'
            self.write('kit/' + source, content)
            entry = next(item for item in manifest['files'] if item['source'] == source)
            entry['sha256'] = entry['installed_sha256'] = hashlib.sha256(content.encode()).hexdigest()
        self.write('kit/native-components.json', json.dumps(manifest))
        env = {**os.environ, 'BASH_ENV': str(self.root / 'environment.sh'), 'CAGE_TEST_FAIL_ACTIVATION': str(self.root / 'activation-failed')}
        result = subprocess.run(['/bin/bash', str(self.kit / 'maintain-macos-workbench.sh'), str(self.kit), 'cage', 'components'],
                                capture_output=True, text=True, env=env, timeout=15)
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.root / 'activation-failed').exists())
        self.assertEqual((self.root / 'Applications/Cage Guest Window.app/Contents/MacOS/CageGuestWindow').read_text(), 'previous-window')
        self.assertFalse((self.root / 'var/db/cage-components.json').exists())
        self.assertEqual(original.read_text(), 'original-socket-lifecycle')
        self.assertFalse(clipboard.exists())
        retry = self.run_install()
        self.assertEqual(retry.returncode, 0, retry.stderr)
        self.assertEqual(original.read_text(), 'migrated-lifecycle\n')
        self.assertEqual(clipboard.read_text(), 'migrated-lifecycle\n')

    def test_actual_snapshot_hashes_installed_files(self):
        manifest = self.prepare_installer()
        result = self.run_install()
        self.assertEqual(result.returncode, 0, result.stderr)
        probe_source = self.translate((REPO / 'internal/workbench/native_component_probe.py').read_text())
        probe_source = probe_source.replace('info.st_uid != 0', 'info.st_uid != ' + str(os.getuid()))
        namespace = {'__name__': 'test_probe'}
        exec(compile(probe_source, 'native_component_probe.py', 'exec'), namespace)
        fake_version = subprocess.CompletedProcess([], 0, 'actual-tool-version\n', '')
        with patch('subprocess.run', return_value=fake_version):
            current = namespace['snapshot']()
            self.assertTrue(current['installed'], current)
            self.assertEqual(current['manifest']['revision'], manifest['revision'])
            self.assertEqual(current['tools']['claude'], 'actual-tool-version')
            self.write('usr/local/libexec/cage-sshfs', 'damaged')
            current = namespace['snapshot']()
            self.assertFalse(current['installed'])
            self.assertEqual(current['manifest']['revision'], '')
            (self.root / 'var/db/cage-components.json').unlink()
            self.assertFalse(namespace['snapshot']()['installed'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
