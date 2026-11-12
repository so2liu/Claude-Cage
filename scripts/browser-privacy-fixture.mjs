// Real production controller; only deployment paths, headless display and a
// private authenticated test-driving CDP route differ from the guest service.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { randomBytes } from 'node:crypto';
import { cp, mkdir, mkdtemp, readdir, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import http from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

export async function createPrivacyController({ preferences, headless = true } = {}) {
  const browserPath = process.env.CAGE_PRIVACY_BROWSER_PATH || process.env.CAGE_TEST_BROWSER_PATH;
  assert.ok(browserPath, 'Set CAGE_PRIVACY_BROWSER_PATH or CAGE_TEST_BROWSER_PATH to Chromium');
  const directory = await realpath(await mkdtemp(join(tmpdir(), 'cage-browser-privacy-')));
  if (preferences) {
    await mkdir(join(directory, 'profile/Default'), { recursive: true });
    await writeFile(join(directory, 'profile/Default/Preferences'), JSON.stringify(preferences));
  }
  const token = randomBytes(32).toString('hex');
  const site = http.createServer((req, res) => {
    if (req.url === '/language-header') {
      res.end(req.headers['accept-language'] || '');
      return;
    }
    res.setHeader('Content-Type', 'text/html');
    res.end('<!doctype html><script>globalThis.webdriverAtLoad = navigator.webdriver; if (parent !== self) parent.postMessage({fixtureIdentity: navigator.webdriver, fixtureGetter: Object.getOwnPropertyDescriptor(Navigator.prototype, "webdriver").get.toString()}, "*")</script><title>Privacy fixture</title><form><input autocomplete="username"><input type="password" autocomplete="current-password"><button>Sign in</button></form>');
  });
  await new Promise(resolve => site.listen(0, '127.0.0.1', resolve));
  const origin = `http://localhost:${site.address().port}`;
  let agent, control, stderr = '';
  const waitFor = async (read, label) => {
    const deadline = Date.now() + 15000;
    while (Date.now() < deadline) {
      if (agent && agent.exitCode !== null) throw new Error(`Controller exited: ${stderr}`);
      const value = await read();
      if (value) return value;
      await delay(50);
    }
    throw new Error(`Timed out: ${label}. ${stderr.slice(-1000)}`);
  };
  const api = async (path, body) => {
    const response = await fetch(control + path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || 'Controller request failed');
    return result;
  };
  const cdp = (id, method, params = {}) => api('/__privacy-test', { id, method, params });
  const evaluate = async (id, expression) => {
    const result = await cdp(id, 'Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    assert.equal(result.exceptionDetails, undefined);
    return result.result.value;
  };
  const stop = async () => {
    if (agent?.exitCode === null) {
      const done = once(agent, 'exit'); agent.kill('SIGTERM');
      const kill = setTimeout(() => agent.kill('SIGKILL'), 5000);
      await done; clearTimeout(kill);
    }
  };
  const close = async () => {
    await stop();
    site.closeAllConnections();
    await new Promise(resolve => site.close(resolve));
    await rm(directory, { recursive: true, force: true });
  };
  try {
    const sourceDirectory = new URL('../internal/cage/', import.meta.url);
    for (const name of await readdir(sourceDirectory))
      if ((name.startsWith('browser-') && name.endsWith('.mjs')) || name === 'native-input.mjs' || name === 'tailnet-client.mjs' || name === 'browser-extension-archive.py')
        await cp(new URL(name, sourceDirectory), join(directory, name));
    await cp(new URL('split-extension', sourceDirectory), join(directory, 'extension'), { recursive: true });
    const source = await readFile(new URL('browser-agent.mjs', sourceDirectory), 'utf8');
    const marker = 'const input = JSON.parse(text || "{}");';
    assert.ok(source.includes(marker));
    const adapted = source.replaceAll('/home/cage-browser', directory)
      .replace('/etc/claude-cage/browser-control.json', join(directory, 'control.json'))
      .replace('/usr/local/lib/cage-split', join(directory, 'extension'))
      .replace('spawn("/usr/bin/chromium", flags', `spawn(${JSON.stringify(browserPath)}, flags`)
      .replace('"--start-fullscreen",', headless ? '"--headless=new", "--disable-background-networking",' : '"--start-fullscreen", "--disable-background-networking",')
      .replace('server.listen(18500, "127.0.0.1");', 'server.listen(0,"127.0.0.1",()=>console.log("PORT="+server.address().port));')
      .replace(marker, `${marker}\nif(req.url === "/__privacy-test") { res.end(JSON.stringify(await cdp(input.method, input.params || {}, input.id ? await session(input.id) : input.sessionId))); return; }`);
    await writeFile(join(directory, 'browser-agent.mjs'), adapted);
    await writeFile(join(directory, 'control.json'), JSON.stringify({ token, workspace: directory }));
    await writeFile(join(directory, 'homepage.json'), JSON.stringify({ url: origin + '/home' }));
    const start = async () => {
      control = ''; stderr = '';
      agent = spawn(process.execPath, [join(directory, 'browser-agent.mjs')], { stdio: ['ignore', 'pipe', 'pipe'] });
      let output = '';
      agent.stdout.on('data', chunk => {
        output += chunk;
        const match = output.match(/PORT=(\d+)/);
        if (match) control = 'http://127.0.0.1:' + match[1];
      });
      agent.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-4000); });
      await waitFor(() => control, 'listener');
      await waitFor(async () => {
        try {
          const response = await fetch(control + '/health', { headers: { Authorization: `Bearer ${token}` } });
          return response.ok;
        } catch { return false; }
      }, 'browser readiness');
    };
    await start();
    const uploadExtension = async bytes => {
      const response = await fetch(control + '/extensions-upload', { method: 'POST', headers: { Authorization: `Bearer ${token}` }, body: bytes });
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || '扩展包读取失败');
      return result;
    };
    const states = signal => fetch(control + '/events', { headers: { Authorization: `Bearer ${token}` }, signal });
    return { api, cdp, evaluate, waitFor, origin, directory, close, uploadExtension, states, restart: async () => { await stop(); await start(); } };
  } catch (error) { await close(); throw error; }
}
