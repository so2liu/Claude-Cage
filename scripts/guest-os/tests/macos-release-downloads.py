"""Run the shipped media entry points with isolated downloads and OS substitutes."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest


SOURCE = Path(__file__).resolve().parents[1]


@unittest.skipUnless(sys.platform == 'darwin', 'Uses the macOS plutil parser')
class ReleaseDownloads(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix='macos-release-downloads-')
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.source = self.root / 'source'
        self.source.mkdir()
        self.media = self.root / 'media'
        self.media.mkdir()
        self.state = {'code': '2.1.322', 'desktop': '5.1231.2'}
        self.digest = hashlib.sha256(b'fixture').hexdigest()
        for name in ('prepare-macos-cli-kit.sh', 'prepare-macos-media.sh', 'claude-desktop-macos.sh'):
            text = (SOURCE / name).read_text()
            text = re.sub(r'(?<![\w])[a-f0-9]{64}(?![\w])', self.digest, text)
            (self.source / name).write_text(text)
        for name in ('macos-cli.py', 'install-macos-cli.sh', 'cli_channel.py', 'cli_channel.resume.md', 'claude_account_resume.md', 'claude-account.py'):
            (self.source / name).write_text('helper fixture')
        (self.media / 'macfuse.pkg').write_text('already prepared')
        self.helper = self.root / 'host downloader'
        self.helper.write_text('#!' + sys.executable + '\n' + r'''
import hashlib,json,os,sys
from pathlib import Path
a=sys.argv[1:]
assert a[0]=='host-download' and len(a) in (5,7)
state=json.loads(Path(os.environ['FIXTURE_STATE']).read_text())
url,target,name=a[1:4]
with open(os.environ['FIXTURE_REQUESTS'],'a') as log: log.write(json.dumps(a)+'\n')
code=('official code '+state['code']).encode()
if name=='macos-cli-latest': body=state['code'].encode()
elif name=='macos-cli-manifest.json':
 body=json.dumps({'version':state.get('manifest_version',state['code']),'platforms':{'darwin-arm64':{'binary':'claude','checksum':state.get('checksum',hashlib.sha256(code).hexdigest())}}}).encode()
elif name=='macos-cli-claude': body=code
elif name=='claude-release.json':
 body=json.dumps({'version':state['desktop'],'url':state.get('url','https://downloads.claude.ai/releases/darwin/universal/'+state['desktop']+'/Claude-'+'a'*40+'.dmg')}).encode()
elif name=='claude.dmg': body=('official desktop '+state['desktop']).encode()
else: body=b'fixture'
if name not in ('macos-cli-latest','macos-cli-manifest.json','claude-release.json','claude.dmg','chrome.dmg'):
 assert len(a)==7 and hashlib.sha256(body).hexdigest()==a[6]
else: assert len(a)==5
Path(target).write_bytes(body)
''')
        self.helper.chmod(0o755)
        self.environment = self.root / 'environment.sh'
        self.environment.write_text('''
codesign() {
    printf '%s\\n' "$*" >> "$FIXTURE_SIGNATURES"
    [[ ${FIXTURE_REJECT_SIGNATURE:-0} != 1 ]]
}
hdiutil() {
    if [[ $1 == attach ]]; then
        local previous='' argument destination=''
        for argument in "$@"; do
            if [[ $previous == -mountpoint ]]; then destination=$argument; fi
            previous=$argument
        done
        cp -R "$FIXTURE_DESKTOP_APP" "$destination/Claude.app"
    fi
}
''')
        self.env = {**os.environ, 'BASH_ENV': str(self.environment), 'CAGE_HOST_DOWNLOADER': str(self.helper),
                    'FIXTURE_STATE': str(self.root / 'state.json'), 'FIXTURE_REQUESTS': str(self.root / 'requests.jsonl'),
                    'FIXTURE_SIGNATURES': str(self.root / 'signatures.log'), 'FIXTURE_DESKTOP_APP': str(self.root / 'signed.app')}

    def run_media(self, tools=True, reject_signature=False):
        (self.root / 'state.json').write_text(json.dumps(self.state))
        contents = self.root / 'signed.app/Contents'
        contents.mkdir(parents=True, exist_ok=True)
        (contents / 'Info.plist').write_text(json.dumps({'CFBundleIdentifier': 'com.anthropic.claudefordesktop',
                                                      'CFBundleShortVersionString': self.state['desktop'], 'CFBundleVersion': '1'}))
        return subprocess.run(['bash', str(self.source / 'prepare-macos-media.sh'), str(self.media)],
                              env={**self.env, 'CAGE_REFRESH_TOOLS': '1' if tools else '0',
                                   'FIXTURE_REJECT_SIGNATURE': '1' if reject_signature else '0'},
                              text=True, capture_output=True, timeout=30)

    def assert_success(self, result):
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)

    def test_code_refresh_resolves_latest_and_preserves_verified_kit_on_failure(self):
        self.assert_success(self.run_media())
        self.assertEqual((self.media / 'cli/claude').read_bytes(), b'official code 2.1.322')
        self.state['code'] = '2.1.323'
        self.assert_success(self.run_media())
        verified = (self.media / 'cli/claude').read_bytes()
        self.assertEqual(verified, b'official code 2.1.323')
        self.state['checksum'] = '0' * 64
        self.assertNotEqual(self.run_media().returncode, 0)
        self.assertEqual((self.media / 'cli/claude').read_bytes(), verified)
        del self.state['checksum']
        self.assertNotEqual(self.run_media(reject_signature=True).returncode, 0)
        self.assertEqual((self.media / 'cli/claude').read_bytes(), verified)
        requests = [json.loads(line) for line in (self.root / 'requests.jsonl').read_text().splitlines()]
        self.assertEqual(sum(row[3] == 'macos-cli-latest' for row in requests), 4)
        self.assertFalse(any(row[3] in ('claude-release.json', 'claude.dmg') for row in requests))
        self.assertIn('Q6L2SF6YDW', (self.root / 'signatures.log').read_text())

    def test_invalid_version_and_manifest_do_not_publish_a_kit(self):
        self.state['code'] = '2.1.322;touch forbidden'
        self.assertNotEqual(self.run_media().returncode, 0)
        self.assertFalse((self.media / 'cli').exists())
        self.state['code'] = '2.1.322'
        self.state['manifest_version'] = '2.1.321'
        self.assertNotEqual(self.run_media().returncode, 0)
        self.assertFalse((self.media / 'cli').exists())

    def test_desktop_verifies_signed_media_and_preserves_it_on_resolution_or_signature_failure(self):
        self.assert_success(self.run_media(tools=False))
        receipt = json.loads((self.media / 'claude-release.json').read_text())
        original = (self.media / 'claude.dmg').read_bytes()
        self.assertEqual(receipt['sha256'], hashlib.sha256(original).hexdigest())
        self.assertEqual(receipt['version'], '5.1231.2')
        self.state['url'] = 'https://example.invalid/Claude.dmg'
        self.assertNotEqual(self.run_media(tools=False).returncode, 0)
        self.assertEqual((self.media / 'claude.dmg').read_bytes(), original)
        del self.state['url']
        self.state['desktop'] = '5.1231.3'
        self.assertNotEqual(self.run_media(tools=False, reject_signature=True).returncode, 0)
        self.assertEqual((self.media / 'claude.dmg').read_bytes(), original)
        self.assertEqual(json.loads((self.media / 'claude-release.json').read_text()), receipt)


if __name__ == '__main__':
    unittest.main(verbosity=2)
