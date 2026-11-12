#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ $(id -u) != 0 ]] || { echo 'Run as an ordinary user in a disposable Ubuntu container.' >&2; exit 2; }
source internal/cage/tool-versions.env
# The installed guest embeds this same Python program in its shell prelude.
downloader="$PWD/internal/cage/tool-download.py"
cage_curl_download() { python3 "$downloader" "$@"; }
source internal/cage/guest-tools.sh
source internal/cage/ubuntu-browser.sh
cage_is_ubuntu || { echo 'This test requires Ubuntu.' >&2; exit 2; }
sudo apt-get update
cage_install_ubuntu_browser
cage_install_ubuntu_novnc
test -r /usr/local/share/cage-novnc/core/rfb.js
sudo python3 internal/cage/novnc-patch.py /usr/local/share/cage-novnc
chromium --version
# Exercise the same pipe transport, sandbox and actual extension as the guest.
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
CAGE_BROWSER_TEST_PROFILE="$stage/profile" CAGE_BROWSER_TEST_EXTENSION="$PWD/internal/cage/split-extension" CAGE_BROWSER_TEST_VERSION="$CAGE_UBUNTU_BROWSER_VERSION" \
  xvfb-run -a node --input-type=module <<'JS'
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
const extension = process.env.CAGE_BROWSER_TEST_EXTENSION;
const id = createHash('sha256').update(extension).digest('hex').slice(0, 32)
  .replace(/./g, c => String.fromCharCode(97 + parseInt(c, 16)));
const browser = spawn('/usr/bin/chromium', [
  `--user-data-dir=${process.env.CAGE_BROWSER_TEST_PROFILE}`, `--load-extension=${extension}`,
  '--remote-debugging-pipe', '--no-first-run', '--no-default-browser-check', 'about:blank',
], { stdio: ['ignore', 'ignore', 'inherit', 'pipe', 'pipe'] });
let sequence = 0, buffer = Buffer.alloc(0), pageLoad;
const pending = new Map();
browser.stdio[4].on('data', chunk => {
  buffer = Buffer.concat([buffer, chunk]);
  let end;
  while ((end = buffer.indexOf(0)) >= 0) {
    const message = JSON.parse(buffer.subarray(0, end).toString());
    buffer = buffer.subarray(end + 1);
    if (pageLoad && message.sessionId === pageLoad.sessionId) {
      if (message.method === 'Page.frameNavigated' && message.params.frame.url === pageLoad.url) pageLoad.navigated = true;
      if (message.method === 'Page.loadEventFired' && pageLoad.navigated) pageLoad.resolve();
    }
    const job = pending.get(message.id);
    if (job) { pending.delete(message.id); message.error ? job.reject(message.error) : job.resolve(message.result); }
  }
});
function cdp(method, params = {}, sessionId) {
  const id = ++sequence;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    browser.stdio[3].write(JSON.stringify({ id, method, params, sessionId }) + '\0');
  });
}
const timeout = setTimeout(() => { browser.kill('SIGKILL'); console.error('Browser pipe test timed out'); process.exitCode = 1; }, 30000);
try {
  const version = await cdp('Browser.getVersion');
  assert.ok(version.product.endsWith('/' + process.env.CAGE_BROWSER_TEST_VERSION));
  const { targetId } = await cdp('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await cdp('Target.attachToTarget', { targetId, flatten: true });
  await cdp('Page.enable', {}, sessionId);
  const url = `chrome-extension://${id}/control.html`;
  const loaded = new Promise(resolve => { pageLoad = { sessionId, url, resolve, navigated: false }; });
  const navigation = await cdp('Page.navigate', { url }, sessionId);
  assert.equal(navigation.errorText, undefined);
  await loaded;
  const result = await cdp('Runtime.evaluate', {
    expression: `({ protocol: location.protocol, extension: globalThis.chrome?.runtime?.id, title: document.title })`,
    returnByValue: true,
  }, sessionId);
  assert.equal(result.result.value.protocol, 'chrome-extension:');
  assert.equal(result.result.value.extension, id);
  console.log(JSON.stringify({ browser: version.product, extension: true, cdpPipe: true, sandboxEnabled: true, noVNC: '1.6.0' }));
  await cdp('Browser.close');
} finally {
  clearTimeout(timeout);
  browser.kill();
}
JS
