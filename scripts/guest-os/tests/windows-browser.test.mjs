import assert from "node:assert/strict";
import { test } from "node:test";
import http from "node:http";
import net from "node:net";
import { once } from "node:events";
import { createInterface } from "node:readline";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";
const { applyWindowsEditorHTMLDefault, checkedBrowserURL, checkedLoopbackProxy, createWindowsBrowserService, validWindowsBrowserFileName } = await import(process.env.CAGE_BROWSER_TEST_CONTROLLER ? pathToFileURL(process.env.CAGE_BROWSER_TEST_CONTROLLER) : new URL("../windows-browser.mjs", import.meta.url));
import { fixtureZip } from "./windows-browser-fixtures.mjs";
import { serveMacOSBrowser } from "../macos-browser-service.mjs";
const moduleURL = file => process.env.CAGE_BROWSER_TEST_MODULE_DIRECTORY ? pathToFileURL(path.join(process.env.CAGE_BROWSER_TEST_MODULE_DIRECTORY, file)) : new URL("../../../internal/cage/" + file, import.meta.url);
const { BrowserClipboard } = await import(moduleURL("browser-clipboard.mjs"));
const { UploadManager } = await import(moduleURL("browser-uploads.mjs"));
const { DownloadManager } = await import(moduleURL("browser-downloads.mjs"));
const { browserCookies } = await import(moduleURL("browser-cookies.mjs"));
const { BrowserExtensions } = await import(moduleURL("browser-extensions.mjs"));
const { BrowserVault } = await import(moduleURL("browser-vault.mjs"));

test("Windows controller accepts only loopback proxy and HTTP pages without credentials", () => {
  assert.equal(checkedLoopbackProxy("http://127.0.0.1:18501"), "http://127.0.0.1:18501");
  for (const value of ["http://localhost:18501", "https://127.0.0.1:18501", "http://user:secret@127.0.0.1:18501", "http://127.0.0.1:18501/control", "http://192.168.1.1:18501", "http://127.0.0.1/"])
    assert.throws(() => checkedLoopbackProxy(value), { code: "network_unavailable" });
  for (const value of ["file:///C:/private", "javascript:alert(1)", "http://user:secret@example.test/", "chrome://settings/"])
    assert.throws(() => checkedBrowserURL(value), { code: "invalid_url" });
  for (const name of ["../secret", "a\\b", "C:secret", "CON.txt", "lpt1", "a.", "a ", "a\0b"])
    assert.equal(validWindowsBrowserFileName(name), false);
  assert.equal(validWindowsBrowserFileName("参考🙂.txt"), true);
});
test("Cage editor changes only its fixed low-priority trust default in the official metadata", () => {
  const data = { configurationDefaults: { "editor.fontSize": 19 }, remoteAuthority: "fixture.localhost", other: "keep & < > ' \" 中文" };
  const encoded = JSON.stringify(data).replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  const html = `<meta id="vscode-workbench-web-configuration" data-settings="${encoded}"><meta id="vscode-workbench-auth-session" data-settings="untouched">`;
  const result = applyWindowsEditorHTMLDefault(html, false);
  const metadata = result.match(/id="vscode-workbench-web-configuration" data-settings="([^"]*)"/)[1].replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, "<").replace(/&gt;/g, ">").replace(/&amp;/g, "&");
  assert.deepEqual(JSON.parse(metadata), { ...data, configurationDefaults: { ...data.configurationDefaults, "security.workspace.trust.enabled": false } });
  assert(result.endsWith('<meta id="vscode-workbench-auth-session" data-settings="untouched">'));
  assert.equal(applyWindowsEditorHTMLDefault(html, undefined), html);
  assert.throws(() => applyWindowsEditorHTMLDefault("missing official metadata", false), { code: "editor_unavailable" });
});

const playwrightPath = process.env.CAGE_BROWSER_TEST_PLAYWRIGHT;
const executablePath = process.env.CAGE_BROWSER_TEST_CHROMIUM;
const integration = playwrightPath && executablePath ? test : test.skip;
async function fixture(t, configured = {}, overrides = {}) {
  const root = await mkdtemp(path.join(tmpdir(), "cage-windows-browser-"));
  const payload = Buffer.alloc((128 << 10) + 37);
  for (let i = 0; i < payload.length; i++) payload[i] = i % 251;
  const diagnosticCookies = [];
  const server = http.createServer((req, res) => {
    if (req.url === "http://198.18.0.254/egress") { diagnosticCookies.push(req.headers.cookie || ""); res.writeHead(503, { "Content-Type": "application/json" }); res.end(JSON.stringify({ ip: "198.51.100.44" })); return; }
    if (req.url === "/download") { res.setHeader("Content-Disposition", 'attachment; filename="reference.bin"'); res.end(payload); return; }
    res.setHeader("Content-Type", "text/html; charset=utf-8");
    const editorMetadata = req.url.startsWith("/?editor=") ? `<meta id="vscode-workbench-web-configuration" data-settings="${JSON.stringify({ remoteAuthority: req.headers.host, configurationDefaults: { "editor.fontSize": 19 } }).replace(/"/g, "&quot;")}">` : "";
    if (req.url.startsWith("/editor-")) res.setHeader("Set-Cookie", "editor_fixture=" + req.url.slice(8) + "; Path=/; SameSite=Lax");
    res.end(`<!doctype html><title>${req.url}</title>${editorMetadata}<style>body{margin:20px}textarea,input,button{display:block;margin:10px;width:300px;height:45px}#drop{height:100px;width:400px;border:1px solid}</style>
      <textarea id="first"></textarea><textarea id="second"></textarea><input id="file" type="file" multiple>
      <button id="copy">Copy actual clipboard</button><a id="download" href="/download">Download</a><div id="drop" tabindex="0">Drop files</div>
      <script>copy.onclick=()=>navigator.clipboard.writeText('实际复制🙂');
      window.selectionEvents=[]; document.addEventListener('selectionchange',()=>selectionEvents.push(String(getSelection())));
      window.dropped=[]; drop.ondragover=e=>e.preventDefault(); drop.ondrop=async e=>{e.preventDefault();window.dropped=await Promise.all([...e.dataTransfer.files].map(async f=>({name:f.name,bytes:[...new Uint8Array(await f.arrayBuffer())]})))};</script>`);
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const url = `http://127.0.0.1:${server.address().port}`;
  const chromium = (await import(pathToFileURL(playwrightPath))).chromium;
  let browserContext, launchOptions;
  const options = {
    platform: process.env.CAGE_BROWSER_TEST_PLATFORM,
    testing: true, headless: process.env.CAGE_BROWSER_TEST_HEADED !== "1", directory: path.join(root, "controller"), downloadDirectory: path.join(root, "saved"), executablePath,
    chromium: { launchPersistentContext: async (directory, options) => { launchOptions = options; return browserContext = await chromium.launchPersistentContext(directory, options); } },
    modules: { BrowserClipboard, UploadManager, DownloadManager, browserCookies, BrowserVault, ...(process.env.CAGE_BROWSER_TEST_EXTENSIONS === "1" ? { BrowserExtensions } : {}) }, displayScale: 2,
    splitDirectory: process.env.CAGE_BROWSER_TEST_SPLIT_DIRECTORY,
    editor: async workspace => ({ url: `http://${workspace.slice(0, 16)}.cage-editor.localhost:${server.address().port}/editor-` + workspace }),
    ...overrides,
  };
  let service = createWindowsBrowserService(options);
  const settings = { proxy: configured.diagnostic ? url : "http://127.0.0.1:18501", ...configured };
  await service.request({ operation: "configure", input: settings });
  t.after(async () => { await service.close(); await new Promise(resolve => server.close(resolve)); await rm(root, { recursive: true, force: true }); });
  const call = (route, input = {}, method = "POST") => service.request({ method, path: "/" + route, input });
  async function open(route = "/one", privateTab = false) {
    const { id } = await call("new", { url: url + route, incognito: privateTab });
    const page = browserContext.browser().contexts().flatMap(context => context.pages()).find(p => p.url() === url + route);
    if (page) await page.waitForLoadState("domcontentloaded");
    return { id, page };
  }
  async function click(id, page, selector) {
    const box = await page.locator(selector).boundingBox();
    const size = page.viewportSize();
    await call("click", { id, x: (box.x + box.width / 2) / size.width, y: (box.y + box.height / 2) / size.height });
  }
  return { root, payload, url, call, open, click, diagnosticCookies, launchOptions: () => launchOptions, privateRequest: (request, options) => service.request(request, options), releaseInput: signal => service.releaseInput(signal), context: () => browserContext, stop: () => service.close(), restart: async () => {
    await service.close(); service = createWindowsBrowserService(options);
    await service.request({ operation: "configure", input: settings });
    return call("tabs", {}, "GET");
  } };
}

integration("interactive browser identity is native in normal and private documents", async t => {
  const f = await fixture(t);
  for (const incognito of [false, true]) {
    const { page } = await f.open("/identity", incognito);
    assert.deepEqual(await page.evaluate(() => ({
      automated: navigator.webdriver,
      getter: Object.getOwnPropertyDescriptor(Navigator.prototype, "webdriver").get.toString(),
    })), { automated: false, getter: "function get webdriver() { [native code] }" });
  }
  assert.ok(f.launchOptions().ignoreDefaultArgs.includes("--enable-automation"));
  assert.equal(f.launchOptions().chromiumSandbox, true);
});

integration("closed browser tabs release their controller sessions", async t => {
  const f = await fixture(t);
  const original = (await f.call("health", {}, "GET")).resources;
  for (let index = 0; index < 12; index++) {
    const { id, page } = await f.open("/lifecycle-" + index);
    await f.call("close", { id });
    assert.equal(page.isClosed(), true);
    assert.deepEqual((await f.call("health", {}, "GET")).resources, original, "closed tabs must release their sessions");
  }
});

integration("removed browser frames release clipboard and controller sessions", async t => {
  const f = await fixture(t);
  const { id, page } = await f.open("/frame-lifecycle");
  const original = (await f.call("health", {}, "GET")).resources;
  for (let index = 0; index < 5; index++) {
    await page.evaluate(async url => {
      const frame = document.createElement("iframe");
      frame.src = url;
      const loaded = new Promise(resolve => { frame.onload = resolve; });
      document.body.append(frame);
      await loaded;
    }, f.url.replace("127.0.0.1", "localhost") + "/child-" + index);
    await page.waitForTimeout(100);
    const attached = (await f.call("health", {}, "GET")).resources;
    assert.ok(attached.sessions > original.sessions);
    await page.evaluate(() => document.querySelector("iframe").remove());
    await page.waitForTimeout(100);
    assert.deepEqual((await f.call("health", {}, "GET")).resources, original, "detached frames must release clipboard state");
    await f.call("copy-context", { id, context: "a".repeat(32) });
  }
});

integration("actual Chromium tabs restore and project switching retains unsaved editor buffers", async t => {
  const f = await fixture(t);
  const one = await f.open("/one"), two = await f.open("/two");
  await f.call("activate", { id: one.id });
  await f.click(one.id, one.page, "#first");
  await f.call("insert-text", { id: one.id, text: "网页未保存内容" });
  await f.call("activate", { id: two.id });
  await f.call("activate", { id: one.id });
  assert.equal(await one.page.locator("#first").inputValue(), "网页未保存内容");
  await one.page.evaluate(() => { document.cookie = "cage_fixture=opaque-value; SameSite=Lax"; });
  const cookies = await f.call("cookies", { id: one.id, mode: "list", scope: "site" });
  const cookie = cookies.cookies.find(c => c.name === "cage_fixture"); assert(cookie); assert.equal(cookie.value, undefined);
  const detail = await f.call("cookies", { id: one.id, mode: "details", scope: "site", cookieKey: cookie.key }); assert.equal(detail.value, "opaque-value");
  const projectA = "a".repeat(24), projectB = "b".repeat(24);
  const editorA = await f.call("editor", { workspace: projectA });
  const pageA = f.context().pages().find(p => p.url().endsWith(projectA)); await pageA.waitForLoadState();
  await f.click(editorA.id, pageA, "#first"); await f.call("insert-text", { id: editorA.id, text: "未保存编辑器缓冲区" });
  const editorB = await f.call("editor", { workspace: projectB });
  const returned = await f.call("editor", { workspace: projectA });
  assert.equal(returned.id, editorA.id); assert.notEqual(editorB.id, editorA.id);
  assert.equal(await pageA.locator("#first").inputValue(), "未保存编辑器缓冲区");
  const cookieA = await f.context().cookies(pageA.url());
  assert.equal(cookieA.find(c => c.name === "editor_fixture").value, projectA);
  const privateTab = await f.open("/private", true);
  const restored = await f.restart();
  assert(restored.tabs.some(t => t.id === one.id && t.url.endsWith("/one")));
  assert(restored.tabs.some(t => t.id === two.id && t.url.endsWith("/two")));
  assert(!restored.tabs.some(t => t.id === privateTab.id || t.incognito || t.url.endsWith("/private")));
  // Full Chrome can restore editor pages from its own profile. The controller
  // keeps those buffers visible with their project access blocked on restart.
  for (const editor of restored.tabs.filter(t => t.kind === "editor")) {
    assert.equal(editor.blocked, true);
    await assert.rejects(f.call("insert-text", { id: editor.id, text: "迟到输入" }), { code: "project_grant_unavailable" });
  }
});
(process.env.CAGE_BROWSER_TEST_PLATFORM === "darwin" ? test.skip : integration)("Windows private editor connection applies only the issued alias's HTML defaults through loopback", async t => {
  const f = await fixture(t), project = "d".repeat(16), connection = "e".repeat(32);
  const url = new URL(f.url); url.hostname = project + ".cage-editor.localhost"; url.search = "?editor=" + project;
  const input = { workspace: project, connection, url: url.href, folder: f.root, trust_default: false };
  await f.call("new", { url: f.url + "/one" });
  const serviceConnection = { operation: "editor-connect", input };
  // The fixture exposes the same private service seam used only by C#.
  await f.privateRequest(serviceConnection);
  const opened = await f.call("editor", { workspace: project });
  const page = f.context().pages().find(page => page.url() === url.href); await page.waitForLoadState();
  const metadata = await page.locator('#vscode-workbench-web-configuration').getAttribute('data-settings');
  assert.deepEqual(JSON.parse(metadata), { remoteAuthority: url.host, configurationDefaults: { "editor.fontSize": 19, "security.workspace.trust.enabled": false } });
  await assert.rejects(f.privateRequest({ operation: "editor-connect", input: { ...input, trust_default: { arbitrary: false } } }), /默认策略/);
  await f.privateRequest({ operation: "editor-revoke", input: { workspace: project, connection } });
  await assert.rejects(f.call("frame", {}, "GET"), /授权/);
  assert(opened.id);
});

const actualFocus = playwrightPath && executablePath && process.env.CAGE_BROWSER_TEST_HEADED === "1" ? test : test.skip;
actualFocus("a native page activation updates the private document proof and the next frame", async t => {
  const f = await fixture(t), one = await f.open("/one"), two = await f.open("/two");
  await f.call("activate", { id: one.id });
  const before = await f.call("frame", {}, "GET"); assert.equal(before.id, one.id);
  // Direct browser activation bypasses Cage's action route, as native Ctrl+Tab
  // does. The proof must select the actual known document, not the old UI id.
  await two.page.bringToFront();
  const frame = await f.call("frame", {}, "GET");
  assert.equal(frame.id, two.id); assert.equal(frame.native.id, two.id);
  assert.equal(frame.native.documentFocused, true);
  assert.equal((await f.call("tabs", {}, "GET")).active, two.id);
  assert.notEqual(frame.target, before.target);
});

integration("private window popups are tracked and closing one tab retains its sibling", async t => {
  const f = await fixture(t), one = await f.open("/private-one", true);
  const created = one.page.context().waitForEvent("page");
  await one.page.evaluate(url => window.open(url, "_blank"), f.url + "/private-two");
  const two = await created; await two.waitForLoadState();
  let snapshot;
  for(let i=0;i<30;i++){snapshot=await f.call("tabs",{},"GET");if(snapshot.tabs.some(t=>t.url.endsWith("/private-two")))break;await new Promise(r=>setTimeout(r,20));}
  const second=snapshot.tabs.find(t=>t.url.endsWith("/private-two"));assert(second?.incognito);
  await f.call("close",{id:one.id});
  assert.equal(two.isClosed(),false);assert.equal((await f.call("tabs",{},"GET")).tabs.some(t=>t.id===second.id),true);
});

integration("native page copy completion carries current context and ignores a synthetic copy", async t => {
  const f = await fixture(t), one = await f.open();
  await f.context().grantPermissions(["clipboard-read", "clipboard-write"], { origin: f.url });
  const context = "c".repeat(32);
  await f.call("copy-context", { id: one.id, context });
  await f.click(one.id, one.page, "#copy");
  await one.page.waitForFunction(() => navigator.clipboard.readText().then(v => v === "实际复制🙂"));
  let result;
  for (let i = 0; i < 20; i++) {
    result = await f.call("events", {}, "GET");
    if (result.events.some(e => e.type === "clipboard")) break;
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  assert(result.events.some(e => e.type === "clipboard" && e.value.text === "实际复制🙂" && e.value.context === context));
  const after = result.sequence;
  await one.page.evaluate(() => { const data = new DataTransfer(); data.setData("text/plain", "forged"); document.dispatchEvent(new ClipboardEvent("copy", { clipboardData: data })); });
  assert.equal((await f.call("events", { after }, "GET")).events.filter(e => e.type === "clipboard").length, 0);
});

integration("64 KiB file blocks reach native chooser/drop, reject switched targets, and download unchanged bytes", async t => {
  const f = await fixture(t), one = await f.open();
  await f.click(one.id, one.page, "#file");
  let chooser;
  for (let i = 0; i < 20; i++) {
    chooser = (await f.call("tabs", {}, "GET")).uploads[0];
    if (chooser) break;
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  assert.equal(chooser.kind, "chooser");
  async function transfer(id, bytes, name = "参考🙂.bin") {
    const opened = await f.call("file-open", { direction: "upload", id, name, length: bytes.length });
    let sequence = 0;
    for (let offset = 0; offset < bytes.length; offset += 64 << 10) {
      const ack = await f.call("file-chunk", { transfer: opened.transfer, sequence, data: bytes.subarray(offset, offset + (64 << 10)).toString("base64") });
      assert.equal(ack.ack, ++sequence);
    }
    await f.call("file-finish", { transfer: opened.transfer, sequence });
  }
  await transfer(chooser.id, f.payload);
  await f.call("upload-finish", { id: chooser.id });
  const selected = await one.page.locator("#file").evaluate(async e => ({ name: e.files[0].name, bytes: [...new Uint8Array(await e.files[0].arrayBuffer())] }));
  assert.equal(selected.name, "参考🙂.bin"); assert.deepEqual(Buffer.from(selected.bytes), f.payload);
  const box = await one.page.locator("#drop").boundingBox(), size = one.page.viewportSize();
  const drop = await f.call("upload-start", { id: one.id, kind: "drop", x: (box.x + 20) / size.width, y: (box.y + 20) / size.height });
  await transfer(drop.id, Buffer.from("dropped"), "drop.txt"); await f.call("upload-finish", { id: drop.id });
  await one.page.waitForFunction(() => window.dropped.length === 1);
  assert.equal(await one.page.evaluate(() => window.dropped[0].name), "drop.txt");
  const stale = await f.call("upload-start", { id: one.id, kind: "paste" });
  const opened = await f.call("file-open", { direction: "upload", id: stale.id, name: "late.txt", length: 4 });
  await f.open("/other");
  await assert.rejects(f.call("file-chunk", { transfer: opened.transfer, sequence: 0, data: Buffer.from("late").toString("base64") }), /切换/);
  await f.call("activate", { id: one.id }); await f.click(one.id, one.page, "#download");
  let record;
  for (let i = 0; i < 100; i++) {
    record = (await f.call("tabs", {}, "GET")).downloads[0];
    if (record?.state === "saved") break;
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  assert.equal(record.state, "saved");
  const read = await f.call("file-open", { direction: "download", id: record.id });
  assert.equal(read.size, f.payload.length);
  const chunks = []; let sequence = 0;
  while (true) {
    const chunk = await f.call("file-read", { transfer: read.transfer, sequence });
    assert.equal(chunk.sequence, ++sequence); chunks.push(Buffer.from(chunk.data, "base64")); if (chunk.eof) break;
  }
  assert.deepEqual(Buffer.concat(chunks), f.payload);
  assert.deepEqual(await readFile(path.join(f.root, "saved", "reference.bin")), f.payload);
});

integration("page pointer release preserves drag position and does not reach a navigated document", async t => {
  const f = await fixture(t), one = await f.open();
  await f.call("resize", { width: 2384, height: 1676 });
  await one.page.evaluate(() => {
    window.pointerEvents = [];
    for (const name of ["pointerdown", "pointermove", "pointerup"])
      addEventListener(name, event => window.pointerEvents.push({ name, x: event.clientX, y: event.clientY, buttons: event.buttons }));
  });
  let frame = await f.call("frame", {}, "GET");
  await f.call("input", { target: frame.target, kind: "pointer", x: 1400, y: 1000, mask: 1 });
  await f.call("input", { target: frame.target, kind: "pointer", x: 2300, y: 1580, mask: 1 });
  await f.privateRequest({ operation: "input-release-pointer" });
  const events = await one.page.evaluate(() => window.pointerEvents);
  assert(events.some(event => event.name === "pointermove" && event.x === 1150 && event.y === 790 && event.buttons === 1));
  assert.deepEqual(events.at(-1), { name: "pointerup", x: 1150, y: 790, buttons: 0 });
  await f.privateRequest({ operation: "input-release-pointer" });
  assert.equal(await one.page.evaluate(() => window.pointerEvents.length), events.length);

  await f.call("input", { target: frame.target, kind: "pointer", x: 1400, y: 1000, mask: 1 });
  await f.call("navigate", { id: one.id, url: f.url + "/next" });
  await one.page.waitForLoadState("domcontentloaded");
  await one.page.evaluate(() => { window.releases = 0; addEventListener("pointerup", () => window.releases++); });
  await f.privateRequest({ operation: "input-release-pointer" });
  assert.equal(await one.page.evaluate(() => window.releases), 0);
  frame = await f.call("frame", {}, "GET");
  await f.call("input", { target: frame.target, kind: "pointer", x: 1400, y: 1000, mask: 1 });
  await f.privateRequest({ operation: "input-release-pointer" });
  assert.equal(await one.page.evaluate(() => window.releases), 1);
});

integration("physical frame geometry, RFB key/pointer and composition reject late page/field input", async t => {
  const f = await fixture(t), one = await f.open();
  await f.call("resize", { width: 1400, height: 1000 });
  const frame = await f.call("frame", {}, "GET");
  assert.equal(frame.width, 1400); assert.equal(frame.height, 1000); assert.equal(frame.cssWidth, 700); assert.equal(frame.scale, 2);
  assert(Buffer.from(frame.jpeg, "base64").subarray(0, 2).equals(Buffer.from([255, 216])));
  const box = await one.page.locator("#first").boundingBox();
  const input = { target: frame.target, kind: "pointer", x: (box.x + 10) * 2, y: (box.y + 10) * 2 };
  await f.call("input", { ...input, mask: 1 }); await f.call("input", { ...input, mask: 0 });
  for (const keysym of [97, 98]) { await f.call("input", { target: frame.target, kind: "key", keysym, down: true }); await f.call("input", { target: frame.target, kind: "key", keysym, down: false }); }
  assert.equal(await one.page.locator("#first").inputValue(), "ab");
  const status = await f.call("ime", { id: one.id, target: "browser", mode: "status" }); assert(status.available); assert(status.rect[3] > 0);
  const session = "d".repeat(32), common = { id: one.id, target: "browser", session };
  for (const value of [
    { mode: "begin", sequence: 0, context: status.context },
    { mode: "update", sequence: 1, text: "nihao", selectionStart: 5, selectionEnd: 5 },
    { mode: "commit", sequence: 2, text: "你好🙂" },
  ]) {
    const reply = await f.call("ime", { ...common, ...value });
    assert.equal(reply.available, true, "the viewer requires an explicit focused-input receipt");
    assert.equal(reply.context, status.context);
  }
  assert.equal(await one.page.locator("#first").inputValue(), "ab你好🙂");
  const next = await f.call("ime", { id: one.id, target: "browser", mode: "status" });
  await f.call("ime", { ...common, mode: "begin", sequence: 3, context: next.context });
  await f.click(one.id, one.page, "#second");
  await assert.rejects(f.call("ime", { ...common, mode: "commit", sequence: 4, text: "迟到" }), /target changed/);
  assert.equal(await one.page.locator("#second").inputValue(), "");
  await f.call("resize", { width: 1200, height: 900 });
  await assert.rejects(f.call("input", { ...input, mask: 1 }), /target changed/);
  const later = await f.open("/after-resize");
  const resized = await f.call("frame", {}, "GET");
  const pixels = await later.page.evaluate(async jpeg => {
    const image = new Image(); image.src = "data:image/jpeg;base64," + jpeg; await image.decode();
    return [image.naturalWidth, image.naturalHeight, innerWidth, innerHeight];
  }, resized.jpeg);
  assert.deepEqual(pixels, [resized.width, resized.height, 600, 450]);
  assert.deepEqual([resized.width, resized.height], [1200, 900]);
});

integration("split panes resize actual pages and route physical input to the clicked pane", async t => {
  const f = await fixture(t), left = await f.open("/left"), right = await f.open("/right");
  await f.call("resize", { width: 1400, height: 1000 });
  await f.call("split", { left: left.id, right: right.id, id: left.id, fraction: 0.5 });
  assert.equal(left.page.viewportSize().width, 350); assert.equal(right.page.viewportSize().width, 350);
  const frame = await f.call("frame", {}, "GET");
  assert.equal(frame.width, 1400); assert.equal(frame.panes.length, 2); assert.equal(frame.panes[1].x, 700);
  assert.equal(frame.panes[0].width, 700); assert.equal(frame.panes[1].width, 700);
  const box = await right.page.locator("#first").boundingBox();
  const input = { target: frame.target, kind: "pointer", x: 700 + (box.x + 10) * 2, y: (box.y + 10) * 2 };
  await f.call("input", { ...input, mask: 1 }); await f.call("input", { ...input, mask: 0 });
  assert.equal((await f.call("tabs", {}, "GET")).active, right.id);
  await f.call("input", { target: frame.target, kind: "key", keysym: 97, down: true }); await f.call("input", { target: frame.target, kind: "key", keysym: 97, down: false });
  assert.equal(await right.page.locator("#first").inputValue(), "a"); assert.equal(await left.page.locator("#first").inputValue(), "");
  await f.call("navigate", { id: left.id, url: f.url + "/replacement" });
  await assert.rejects(f.call("input", { ...input, mask: 1 }), /target changed/);
  await f.call("split", { id: right.id }); assert.equal(right.page.viewportSize().width, 700);
});

integration("screencast retains HiDPI pixels, skips static frames and resumes after its viewer leaves", async t => {
  const f = await fixture(t, {}, { continuousFrames: false }), one = await f.open("/still");
  await one.page.evaluate(() => { document.body.style.background = "rgb(245, 240, 230)"; });
  const screenshot = one.page.screenshot.bind(one.page);
  one.page.screenshot = async options => {
    // A slow Windows capture can take longer than the idle viewer timeout.
    one.page.screenshot = screenshot;
    await new Promise(resolve => setTimeout(resolve, 1700));
    return screenshot(options);
  };
  let frame = await f.call("frame", {}, "GET");
  const dimensions = await one.page.evaluate(async jpeg => {
    const image = new Image(); image.src = "data:image/jpeg;base64," + jpeg; await image.decode();
    return [image.naturalWidth, image.naturalHeight, innerWidth, devicePixelRatio];
  }, frame.jpeg);
  assert.deepEqual(dimensions, [2280, 1400, 1140, 2]);
  const before = (await f.call("health", {}, "GET")).capture;
  let unchanged = 0;
  for (let i = 0; i < 16; i++) {
    const next = await f.call("frame", { after: frame.sequence, nativeProof: i >= 8 }, "GET");
    if (next.unchanged) {
      unchanged++;
      assert.equal(next.jpeg, undefined);
      if (i >= 8) {
        assert.equal(next.native.id, one.id);
        assert.equal(next.native.target, frame.target);
      } else assert.equal(next.native, undefined);
    }
    else frame = next;
    await new Promise(resolve => setTimeout(resolve, 40));
  }
  assert(unchanged >= 12, "static page was repeatedly captured");
  if (before) {
    const after = (await f.call("health", {}, "GET")).capture;
    assert(after.screenshots - before.screenshots <= 4, "static polling repeatedly invoked a HiDPI screenshot");
  }
  await one.page.evaluate(() => { document.body.style.background = "rgb(30, 120, 220)"; });
  let updated;
  for (let i = 0; i < 20; i++) {
    const next = await f.call("frame", { after: frame.sequence }, "GET");
    if (!next.unchanged) { updated = next; break; }
    await new Promise(resolve => setTimeout(resolve, 40));
  }
  assert(updated?.jpeg && updated.sequence !== frame.sequence);
  await new Promise(resolve => setTimeout(resolve, 1700));
  const resumed = await f.call("frame", { after: updated.sequence }, "GET");
  assert(resumed.jpeg && resumed.sequence !== updated.sequence);
});

integration("a scrolled browser viewport stays visible while a web font is loading", async t => {
  const f = await fixture(t, { displayScale: 1.5 });
  const { page } = await f.open();
  const initial = await f.call("frame", {}, "GET");
  let releaseFont;
  const pending = new Promise(resolve => { releaseFont = resolve; });
  await page.route("**/pending-font.woff2", async route => { await pending; await route.abort(); });
  const requested = page.waitForRequest("**/pending-font.woff2");
  await page.evaluate(() => {
    document.body.innerHTML = '<main style="height:90vh;overflow:auto"><div style="height:200vh;background:red"></div><div style="height:120vh;background:lime">waiting</div></main>';
    const font = new FontFace("pending", "url(/pending-font.woff2)");
    document.fonts.add(font);
    document.body.style.fontFamily = "pending, sans-serif";
    const pane = document.querySelector("main");
    pane.scrollTop = pane.scrollHeight;
  });
  await requested;
  const timer = setTimeout(releaseFont, 4000);
  try {
    let after = initial.sequence;
    for (;;) {
      const frame = await f.call("frame", { after }, "GET");
      assert.equal(await page.evaluate(() => document.fonts.status), "loading", "scrolled frame must arrive before font loading ends");
      if (!frame.unchanged) {
        const pixel = await page.evaluate(async ({ jpeg, width, height }) => {
          const image = new Image(); image.src = "data:image/jpeg;base64," + jpeg; await image.decode();
          const ctx = document.createElement("canvas").getContext("2d");
          ctx.drawImage(image, width / 2, height / 2, 1, 1, 0, 0, 1, 1);
          return { rgb: [...ctx.getImageData(0, 0, 1, 1).data], size: [image.width, image.height] };
        }, frame);
        assert.deepEqual(pixel.size, [frame.width, frame.height]);
        assert.equal(frame.scale, 1.5);
        if (pixel.rgb[1] > 245 && pixel.rgb[0] < 10 && pixel.rgb[2] < 10) break;
        after = frame.sequence;
      }
      await new Promise(resolve => setTimeout(resolve, 30));
    }
  } finally { clearTimeout(timer); releaseFont(); }
});

integration("pixel wheel preserves distance and still rejects stale targets and invalid bounds", async t => {
  const f = await fixture(t), { page } = await f.open();
  await page.evaluate(() => { document.body.style.height = "6000px"; document.documentElement.style.scrollBehavior = "auto"; });
  const frame = await f.call("frame", {}, "GET");
  const wheel = { kind: "pointer", target: frame.target, x: 600, y: 600, mask: 16, deltaX: 0, deltaY: 720 };
  await f.call("input", wheel);
  await page.waitForFunction(() => scrollY >= 700);
  assert.equal(await page.evaluate(() => scrollY), 720);
  await f.call("input", { ...wheel, mask: 8, deltaY: -360 });
  await page.waitForFunction(() => scrollY === 360);
  await assert.rejects(f.call("input", { ...wheel, target: "stale" }), /target changed/);
  await assert.rejects(f.call("input", { ...wheel, deltaY: 2049 }), /滚动距离/);
  await assert.rejects(f.call("input", { ...wheel, mask: 17 }), /滚动距离/);
  assert.equal(await page.evaluate(() => scrollY), 360);
});

integration("continuous HiDPI frames preserve pixels, click coordinates and resize without screenshot polling", async t => {
  const f = await fixture(t, {}, { continuousFrames: true }), { id, page } = await f.open();
  await page.evaluate(() => {
    document.body.innerHTML = '<style>body{margin:0}section{height:240px;border-bottom:1px solid #aaa}button{margin:24px;width:200px;height:40px}</style>' +
      Array.from({ length: 40 }, (_, i) => `<section style="background:${i % 2 ? '#cef' : '#fed'}"><button onclick="this.textContent='clicked'">Marker ${i}</button></section>`).join('');
    scrollTo(0, 1100);
  });
  const reference = (await page.screenshot({ type: 'jpeg', quality: 85, caret: 'initial' })).toString('base64');
  let frame = await f.call('frame', {}, 'GET');
  // The first compositor event may already be in flight during the surface
  // resize. Compare the settled image, rather than that transition frame.
  await new Promise(resolve => setTimeout(resolve, 100));
  frame = await f.call('frame', {}, 'GET');
  if (process.env.CAGE_CAPTURE_DIAG_DIR) {
    await writeFile(path.join(process.env.CAGE_CAPTURE_DIAG_DIR, 'stream-test.jpg'), Buffer.from(frame.jpeg, 'base64'));
    await writeFile(path.join(process.env.CAGE_CAPTURE_DIAG_DIR, 'reference-test.jpg'), Buffer.from(reference, 'base64'));
  }
  const comparison = await page.evaluate(async ({ actual, reference }) => {
    const images = await Promise.all([actual, reference].map(async jpeg => { const image = new Image(); image.src = 'data:image/jpeg;base64,' + jpeg; await image.decode(); return image; }));
    const pixels = images.map(image => { const canvas = document.createElement('canvas'); canvas.width = image.width; canvas.height = image.height; const ctx = canvas.getContext('2d'); ctx.drawImage(image, 0, 0); return ctx.getImageData(0, 0, canvas.width, canvas.height).data; });
    let total = 0; for (let i = 0; i < pixels[0].length; i += 4) for (let j = 0; j < 3; j++) total += Math.abs(pixels[0][i+j] - pixels[1][i+j]);
    return { sizes: images.map(i => [i.width, i.height]), error: total / (pixels[0].length / 4 * 3), dpr: devicePixelRatio, scroll: scrollY };
  }, { actual: frame.jpeg, reference });
  assert.deepEqual(comparison.sizes, [[2280, 1400], [2280, 1400]]);
  assert.equal(comparison.dpr, 2); assert.equal(comparison.scroll, 1100);
  assert(comparison.error < 3, JSON.stringify(comparison));
  const button = await page.evaluate(() => { const b = [...document.querySelectorAll('button')].find(b => b.getBoundingClientRect().y > 30); const r = b.getBoundingClientRect(); return { x: r.x + r.width/2, y: r.y + r.height/2 }; });
  for (const mask of [1, 0]) await f.call('input', { kind: 'pointer', target: frame.target, x: button.x*2, y: button.y*2, mask });
  assert.equal(await page.evaluate(() => !![...document.querySelectorAll('button')].find(b => b.textContent === 'clicked')), true);
  await f.call('resize', { width: 2000, height: 1300 });
  await assert.rejects(f.call('input', { kind: 'pointer', target: frame.target, x: 100, y: 100, mask: 1 }), /target changed/);
  frame = await f.call('frame', {}, 'GET');
  assert.equal(frame.width, 2000); assert.equal(frame.height, 1300);
  await f.call('input', { kind: 'pointer', target: frame.target, x: 300, y: 400, mask: 16, deltaX: 0, deltaY: 120 });
  await page.waitForFunction(() => scrollY === 1220);
  const stats = (await f.call('health', {}, 'GET')).capture;
  assert.equal(stats.screenshots, 0); assert(stats.streamed_frames > 0);
  await new Promise(resolve => setTimeout(resolve, 1700));
  const resumed = await f.call('frame', {}, 'GET');
  assert.equal(resumed.width, 2000);
  assert.equal((await f.call('health', {}, 'GET')).capture.screenshots, 0);
  t.diagnostic(JSON.stringify({ imageMeanError: comparison.error, screenshots: stats.screenshots, streamedFrames: stats.streamed_frames }));
});

integration("audit reads configured browser state separately from expected values and preserves actual HTTP failures", async t => {
  const f = await fixture(t, { timezone: "Pacific/Honolulu", language: "ja-JP", diagnostic: true }), one = await f.open();
  await f.context().addCookies([{ name: "private_fixture", value: "test-only", domain: "198.18.0.254", path: "/" }]);
  const before = await f.call("tabs", {}, "GET");
  const result = await f.call("audit", { timezone: "Asia/Tokyo", network: true });
  assert.equal(result.timezone, "Pacific/Honolulu"); assert.equal(result.expected_timezone, "Asia/Tokyo"); assert.equal(result.language, "ja-JP");
  assert.equal(result.network.status, 503); assert.equal(result.network.ip, "198.51.100.44"); assert.deepEqual(f.diagnosticCookies, [""]);
  assert.equal((await f.call("tabs", {}, "GET")).active, one.id); assert.equal((await f.call("tabs", {}, "GET")).tabs.length, before.tabs.length);
});

integration("native image focus proof stays stable for a field and changes before a different field receives input", async t => {
  const f=await fixture(t),one=await f.open();
  await f.click(one.id,one.page,"#first");
  const original=await f.call("focus-proof",{},"GET");
  assert.equal((await f.call("focus-proof",{},"GET")).focus,original.focus);
  await f.click(one.id,one.page,"#second");
  assert.notEqual((await f.call("focus-proof",{},"GET")).focus,original.focus);
});

const macImages = playwrightPath && executablePath && process.env.CAGE_BROWSER_TEST_PLATFORM === "darwin" ? test : test.skip;
macImages("Mac browser service keeps actual Chrome input when a viewer disconnects", async t => {
  const f = await fixture(t);
  const directory = await mkdtemp(path.join(tmpdir(), "cage-bs-"));
  const service = await serveMacOSBrowser({ directory, dispatch: f.privateRequest, disconnect: f.releaseInput, close: f.stop });
  const connections = [];
  const connect = async () => {
    const socket = net.createConnection(path.join(directory, "browser.sock"));
    connections.push(socket);
    await once(socket, "connect");
    const input = createInterface({ input: socket });
    return { socket, call: async request => {
      const result = once(input, "line");
      socket.write(JSON.stringify(request) + "\n");
      return JSON.parse((await result)[0]);
    } };
  };
  try {
    const first = await connect();
    const opened = await first.call({ method: "POST", path: "/new", input: { url: f.url + "/retained" } });
    const page = f.context().pages().find(page => page.url().endsWith("/retained"));
    await page.locator("#first").waitFor();
    await f.click(opened.id, page, "#first");
    await first.call({ method: "POST", path: "/insert-text", input: { id: opened.id, text: "unsaved work survives its viewer" } });
    await page.evaluate(() => { window.releasedControl = false; document.addEventListener("keyup", event => { if (event.key === "Control") window.releasedControl = true; }); });
    const frame = await first.call({ method: "GET", path: "/frame" });
    await first.call({ method: "POST", path: "/input", input: { kind: "key", target: frame.target, keysym: 0xffe3, down: true } });
    first.socket.destroy();
    const second = await connect();
    const state = await second.call({ method: "GET", path: "/tabs" });
    assert.equal(state.service.persistent, true);
    assert.equal(state.active, opened.id);
    assert.equal(await page.locator("#first").inputValue(), "unsaved work survives its viewer");
    await page.waitForFunction(() => window.releasedControl === true);
    await service.close();
    assert.equal(page.isClosed(), true);
  } finally {
    for (const socket of connections) socket.destroy();
    await service.close();
    await rm(directory, { recursive: true, force: true });
  }
});

macImages("Mac image preparation rejects a changed field before accepting bytes", async t => {
  const f = await fixture(t), one = await f.open();
  await f.click(one.id, one.page, "#first");
  assert.equal((await f.call("tabs", {}, "GET")).capabilities.systemClipboard, false);
  const prepared = await f.call("image-start", { id: one.id });
  const png = await one.page.screenshot({ type: "png" });
  await f.click(one.id, one.page, "#second");
  await assert.rejects(f.call("image-chunk", { transfer: prepared.transfer, sequence: 0, length: png.length, data: png.toString("base64") }), /target changed/i);
  await assert.rejects(f.call("image-finish", { transfer: prepared.transfer, sequence: 0 }), /已关闭|顺序/);
  const cancelled = await f.call("image-start", { id: one.id });
  await f.call("image-cancel", { transfer: cancelled.transfer });
  await assert.rejects(f.call("image-finish", { transfer: cancelled.transfer, sequence: 0 }), /已关闭|顺序/);
});

macImages("Mac clipboard text uses one bounded transfer and one final insertion", async t => {
  const f = await fixture(t), one = await f.open();
  await one.page.evaluate(() => {
    document.querySelector("#first").addEventListener("paste", event => {
      window.textPaste = { trusted: event.isTrusted, text: event.clipboardData.getData("text/plain") };
    });
  });
  await f.click(one.id, one.page, "#first");
  assert.equal(f.launchOptions().ignoreDefaultArgs.includes("--mute-audio"), true);
  const text = "中".repeat(Math.floor((2 << 20) / 3)) + "ab", bytes = Buffer.from(text);
  assert.equal(bytes.length, 2 << 20);
  const prepared = await f.call("paste-start", { id: one.id });
  let sequence = 0;
  for (let offset = 0; offset < bytes.length; offset += 65536) {
    const reply = await f.call("paste-chunk", { transfer: prepared.transfer, sequence, length: bytes.length, mime: "text/plain", data: bytes.subarray(offset, offset + 65536).toString("base64") });
    assert.equal(reply.ack, ++sequence);
  }
  assert.equal(await one.page.locator("#first").inputValue(), "");
  assert.equal((await f.call("paste-finish", { transfer: prepared.transfer, sequence })).ok, true);
  const actual = await one.page.locator("#first").inputValue();
  assert.equal(Buffer.byteLength(actual), bytes.length);
  assert.equal(createHash("sha256").update(actual).digest("hex"), createHash("sha256").update(text).digest("hex"));
  const pasted = await one.page.evaluate(() => window.textPaste);
  assert.equal(pasted.trusted, true);
  assert.equal(pasted.text, text);
  const tooLarge = await f.call("paste-start", { id: one.id });
  await assert.rejects(f.call("paste-chunk", { transfer: tooLarge.transfer, sequence: 0, length: bytes.length + 1, mime: "text/plain", data: "YQ==" }), /大小/);
});

const macImagePaste = playwrightPath && executablePath && process.env.CAGE_BROWSER_TEST_PLATFORM === "darwin" && process.env.CAGE_BROWSER_TEST_PNG_PASTE === "1" ? test : test.skip;
integration("native paste preparation rejects a field switch inside a child frame", async t => {
  const f = await fixture(t), one = await f.open();
  for (const origin of [f.url, f.url.replace("127.0.0.1", "localhost")]) {
    await one.page.evaluate(url => {
      document.querySelector("#child")?.remove();
      const frame = document.createElement("iframe"); frame.id = "child"; frame.src = url;
      document.body.append(frame);
    }, origin + "/child");
    const child = one.page.frameLocator("#child");
    await child.locator("#first").click();
    const before = await f.call("focus-proof", {}, "GET");
    const prepared = await f.call("paste-start", { id: one.id });
    await child.locator("#second").focus();
    const after = await f.call("focus-proof", {}, "GET");
    assert.notEqual(after.focus, before.focus);
    await assert.rejects(f.call("paste-chunk", { transfer: prepared.transfer, sequence: 0, mime: "text/plain", length: 1, data: "YQ==" }), /target changed/i);
  }
});

macImagePaste("Mac headless image transfer delivers an actual PNG paste event", async t => {
  const f = await fixture(t), one = await f.open();
  await one.page.evaluate(() => {
    document.querySelector("#first").addEventListener("paste", async event => {
      event.preventDefault();
      const file = event.clipboardData.files[0];
      window.imagePaste = { trusted: event.isTrusted, type: file?.type, bytes: file ? [...new Uint8Array(await file.arrayBuffer())] : [] };
    });
  });
  await f.click(one.id, one.page, "#first");
  const prepared = await f.call("image-start", { id: one.id });
  const png = await one.page.screenshot({ type: "png", clip: { x: 0, y: 0, width: 16, height: 16 } });
  let sequence = 0;
  for (let offset = 0; offset < png.length; offset += 65536) {
    const reply = await f.call("image-chunk", { transfer: prepared.transfer, sequence, length: png.length, data: png.subarray(offset, offset + 65536).toString("base64") });
    assert.equal(reply.ack, ++sequence);
  }
  assert.equal((await f.call("image-finish", { transfer: prepared.transfer, sequence })).ok, true);
  await one.page.waitForFunction(() => window.imagePaste?.bytes.length > 0);
  const pasted = await one.page.evaluate(() => window.imagePaste);
  assert.equal(pasted.trusted, true);
  assert.equal(pasted.type, "image/png");
  assert.deepEqual(Buffer.from(pasted.bytes).subarray(0, 8), png.subarray(0, 8));
  await assert.rejects(f.call("image-finish", { transfer: prepared.transfer, sequence }), /已关闭|顺序/);
});

async function readClipboardReference(f, copied) {
  assert.match(copied.clipboard.id, /^[a-f0-9-]{36}$/);
  const opened = await f.call("file-open", { direction: "download", id: copied.clipboard.id, tab: copied.id, context: copied.context, document: copied.document });
  assert.equal(opened.mime, copied.clipboard.mime);
  assert.equal(opened.size, copied.clipboard.size);
  const chunks = [];
  let sequence = 0, result;
  do {
    result = await f.call("file-read", { transfer: opened.transfer, sequence });
    assert.equal(result.sequence, ++sequence);
    chunks.push(Buffer.from(result.data, "base64"));
  } while (!result.eof);
  const bytes = Buffer.concat(chunks);
  assert.equal(bytes.length, opened.size);
  return bytes;
}

macImagePaste("Mac webpage PNG copy exports a bound reference with the original pixels", async t => {
  const f = await fixture(t), one = await f.open();
  await one.page.evaluate(() => {
    document.querySelector("#copy").onclick = async () => {
      const canvas = document.createElement("canvas");
      canvas.width = 8; canvas.height = 4;
      const context = canvas.getContext("2d");
      context.fillStyle = "#d82931"; context.fillRect(0, 0, 4, 4);
      context.fillStyle = "#264dc6"; context.fillRect(4, 0, 4, 4);
      const blob = await new Promise(resolve => canvas.toBlob(resolve, "image/png"));
      await navigator.clipboard.write([new ClipboardItem({ "image/png": blob })]);
    };
  });
  const context = "b".repeat(32);
  await f.call("copy-context", { id: one.id, context });
  await f.click(one.id, one.page, "#copy");
  let copied;
  for (let attempt = 0; attempt < 100 && !copied; attempt++) {
    const state = await f.call("events", {}, "GET");
    copied = state.events.find(event => event.type === "clipboard" && event.value.context === context)?.value;
    if (!copied) await new Promise(resolve => setTimeout(resolve, 20));
  }
  assert.equal(copied?.clipboard?.mime, "image/png", JSON.stringify(copied));
  assert.equal(copied.png, undefined);
  assert.equal(copied.text, undefined);
  assert.equal((await f.call("tabs", {}, "GET")).tabs.some(tab => tab.url.includes("cage-clipboard.localhost")), false);
  await assert.rejects(f.call("file-open", { direction: "download", id: copied.clipboard.id }), { code: "copy_changed" });
  const bytes = await readClipboardReference(f, copied);
  const pixels = await one.page.evaluate(async encoded => {
    const bytes = Uint8Array.from(atob(encoded), value => value.charCodeAt(0));
    const bitmap = await createImageBitmap(new Blob([bytes], { type: "image/png" }));
    const canvas = document.createElement("canvas"); canvas.width = bitmap.width; canvas.height = bitmap.height;
    const context = canvas.getContext("2d"); context.drawImage(bitmap, 0, 0);
    const result = { width: bitmap.width, height: bitmap.height, left: [...context.getImageData(1, 1, 1, 1).data], right: [...context.getImageData(6, 1, 1, 1).data] };
    bitmap.close();
    return result;
  }, bytes.toString("base64"));
  assert.deepEqual(pixels, { width: 8, height: 4, left: [216, 41, 49, 255], right: [38, 77, 198, 255] });
  await f.call("copy-context", { id: one.id, context: "c".repeat(32) });
  await assert.rejects(readClipboardReference(f, copied), { code: "copy_changed" });
});

macImagePaste("Mac native copy and cut preserve 2 MiB UTF-8 text and reject superseded reads", async t => {
  const f = await fixture(t), one = await f.open();
  const text = "中".repeat(Math.floor((2 << 20) / 3)) + "ab";
  await one.page.locator("#first").fill(text);
  await one.page.locator("#first").selectText();
  const copied = await f.call("copy", { id: one.id, context: "1".repeat(32) });
  assert.equal(copied.clipboard.mime, "text/plain");
  assert.equal(copied.clipboard.size, 2 << 20);
  assert.equal((await readClipboardReference(f, copied)).toString("utf8"), text);
  assert.equal(await one.page.locator("#first").inputValue(), text);
  const opened = await f.call("file-open", { direction: "download", id: copied.clipboard.id, tab: copied.id, context: copied.context, document: copied.document });
  const first = await f.call("file-read", { transfer: opened.transfer, sequence: 0 });
  assert.equal(first.eof, false);
  await f.call("copy-context", { id: one.id, context: "2".repeat(32) });
  await assert.rejects(f.call("file-read", { transfer: opened.transfer, sequence: 1 }), { code: "copy_changed" });
  const cut = await f.call("cut", { id: one.id, context: "3".repeat(32) });
  assert.equal((await readClipboardReference(f, cut)).toString("utf8"), text);
  assert.equal(await one.page.locator("#first").inputValue(), "");
  const state = await f.call("events", {}, "GET");
  assert.equal(state.events.some(event => event.type === "clipboard" && [copied.context, cut.context].includes(event.value.context)), false, "explicit copy results must have one consumer");
  await assert.rejects(f.call("copy", { id: one.id, context: "4".repeat(32) }), { code: "copy_unavailable" });
});

const nativeExtensions = playwrightPath && executablePath && process.env.CAGE_BROWSER_TEST_EXTENSIONS === "1" ? test : test.skip;
nativeExtensions("native extension preview, disabled import, activation and restart preserve the selected state", async t => {
  const f = await fixture(t), one = await f.open();
  assert.equal((await f.call("tabs", {}, "GET")).capabilities.extensions, true);
  const archive = fixtureZip([{ name: "manifest.json", data: JSON.stringify({ manifest_version: 3, name: "Cage isolated extension fixture", version: "1.0", content_scripts: [{ matches: [f.url + "/*"], js: ["content.js"], run_at: "document_start" }] }) },
    { name: "content.js", data: "document.addEventListener('DOMContentLoaded',()=>document.documentElement.dataset.fixtureExtension='ran',{once:true})" }]);
  const upload = await f.call("file-open", { direction: "extension", length: archive.length });
  await f.call("file-chunk", { transfer: upload.transfer, sequence: 0, data: archive.toString("base64") });
  const preview = await f.call("file-finish", { transfer: upload.transfer, sequence: 1 });
  assert(preview.hostPermissions.includes(f.url + "/*"));
  let installed = await f.call("extensions", { mode: "install", session: preview.session, enabled: false });
  const extension = installed.items.find(item => item.name === "Cage isolated extension fixture"); assert(extension); assert.equal(extension.enabled, false);
  await f.call("reload", { id: one.id }); await one.page.waitForLoadState("domcontentloaded");
  assert.equal(await one.page.evaluate(() => document.documentElement.dataset.fixtureExtension), undefined);
  await f.call("extensions", { mode: "enable", id: extension.id });
  await f.call("reload", { id: one.id }); await one.page.waitForLoadState("domcontentloaded");
  assert.equal(await one.page.evaluate(() => document.documentElement.dataset.fixtureExtension), "ran");
  await f.call("extensions", { mode: "disable", id: extension.id });
  await f.restart();
  installed = await f.call("extensions", { mode: "list" }); assert.equal(installed.items.find(item => item.id === extension.id).enabled, false);
});

const nativeUI = playwrightPath && executablePath && process.env.CAGE_BROWSER_TEST_HEADED === "1" && process.env.CAGE_BROWSER_TEST_NATIVE_UI === "1" ? test : test.skip;
nativeUI("headed Chrome exposes owned window proof and CDP shortcuts alone do not focus native find UI", async t => {
  const f = await fixture(t), one = await f.open();
  await f.click(one.id, one.page, "#first");
  const before = await f.call("frame", {}, "GET"); assert.equal(before.native.documentFocused, true);
  await f.call("key", { id: one.id, key: "f", meta: process.platform === "darwin", ctrl: process.platform !== "darwin" });
  const after = await f.call("frame", {}, "GET");
  // Windows BrowserRpc must send this through its owned Win32 window bridge.
  // A Page keyboard call does not provide a Chrome-chrome interaction proof.
  assert.equal(after.native.documentFocused, true);
  assert.equal(after.native.window.windowId, before.native.window.windowId);
  assert.equal(after.native.document, before.native.document);
  assert.equal(after.native.id, one.id);
});

function interceptCapture(context, mutate) {
  const create = context.newCDPSession.bind(context);
  context.newCDPSession = async (...args) => {
    const session = await create(...args), on = session.on.bind(session), off = session.off.bind(session);
    const listeners = new Map();
    session.on = (event, callback) => {
      if (event !== "Page.screencastFrame") return on(event, callback);
      const wrapped = frame => callback(mutate(frame));
      listeners.set(callback, wrapped);
      return on(event, wrapped);
    };
    session.off = (event, callback) => off(event, listeners.get(callback) || callback);
    return session;
  };
}

integration("slow viewers bound compositor encoding during continuous animation", async t => {
  const f = await fixture(t, {}, { continuousFrames: true }), { page } = await f.open();
  await f.call("resize", { width: 800, height: 600 });
  await page.evaluate(() => {
    let n = 0;
    window.animation = setInterval(() => document.body.style.backgroundColor = "hsl(" + (n++ * 19 % 360) + " 80% 50%)", 16);
  });
  await f.call("frame", {}, "GET");
  const start = performance.now(), before = (await f.call("health", {}, "GET")).capture;
  for (let i = 0; i < 8; i++) {
    await new Promise(resolve => setTimeout(resolve, 300));
    await f.call("frame", {}, "GET");
  }
  const after = (await f.call("health", {}, "GET")).capture;
  const frames = after.damage_frames - before.damage_frames;
  assert(frames <= 20, "encoding must follow the eight viewer requests, not the 60 Hz animation: " + frames);
  assert.equal(after.fallbacks, 0);
  t.diagnostic(JSON.stringify({ viewerRequests: 8, elapsedMs: Math.round(performance.now()-start), encodedFrames: frames, encodedBytes: after.encoded_bytes - before.encoded_bytes }));
});

integration("mismatched compositor frames fall back and restore input on resize and reconnect", async t => {
  const f = await fixture(t, {}, { continuousFrames: true }), { id, page } = await f.open();
  let corrupt = false;
  interceptCapture(f.context(), frame => corrupt ? { ...frame, data: Buffer.from("bad jpeg").toString("base64") } : frame);
  await f.call("frame", {}, "GET");
  corrupt = true;
  await page.evaluate(() => document.body.style.background = "red");
  await new Promise(resolve => setTimeout(resolve, 120));
  let frame = await f.call("frame", {}, "GET");
  let stats = (await f.call("health", {}, "GET")).capture;
  assert.equal(stats.fallbacks, 1);
  assert(stats.invalid_frames > 0);
  assert(stats.screenshots > 0);
  await f.click(id, page, "#first");
  await page.keyboard.insertText("restored");
  assert.equal(await page.locator("#first").inputValue(), "restored");
  await f.call("resize", { width: 1800, height: 1000 });
  frame = await f.call("frame", {}, "GET");
  assert.equal(frame.width, 1800); assert.equal(frame.height, 1000);
  await new Promise(resolve => setTimeout(resolve, 1700));
  frame = await f.call("frame", {}, "GET");
  assert.equal(frame.width, 1800);
  await f.click(id, page, "#second");
  await page.keyboard.insertText("reconnected");
  assert.equal(await page.locator("#second").inputValue(), "reconnected");
});

integration("stopping continuous capture restores ordinary input before the next viewer", async t => {
  const f = await fixture(t, {}, { continuousFrames: true }), { id, page } = await f.open();
  await f.call("frame", {}, "GET");
  await new Promise(resolve => setTimeout(resolve, 1800));
  assert.equal((await f.call("health", {}, "GET")).resources.captures, 0);
  await f.click(id, page, "#second");
  await page.keyboard.insertText("after expiry");
  assert.equal(await page.locator("#second").inputValue(), "after expiry");
  await f.call("frame", {}, "GET");
  await f.click(id, page, "#first");
  await page.keyboard.insertText("after reconnect");
  assert.equal(await page.locator("#first").inputValue(), "after reconnect");
});

integration("default HiDPI capture retains the unscaled screenshot path", async t => {
  for (const continuousFrames of [undefined, false]) {
    const f = await fixture(t, {}, { continuousFrames }), { page } = await f.open();
    await f.call("frame", {}, "GET");
    const stats = (await f.call("health", {}, "GET")).capture;
    assert.equal(stats.streamed_frames, 0);
    assert(stats.screenshots > 0);
    assert.equal(await page.evaluate(() => devicePixelRatio), 2);
  }
});

integration("default one-to-one frames stream without screenshot polling", async t => {
  const f = await fixture(t, {}, { displayScale: 1 }), { id, page } = await f.open();
  const frame = await f.call("frame", {}, "GET");
  assert.equal(frame.width, 1140); assert.equal(frame.height, 700);
  const stats = (await f.call("health", {}, "GET")).capture;
  assert.equal(stats.screenshots, 0); assert(stats.streamed_frames > 0);
  await f.click(id, page, "#first");
  await page.keyboard.insertText("native pixels");
  assert.equal(await page.locator("#first").inputValue(), "native pixels");
});


integration("startup invalid frames have a bounded recovery window", async t => {
  for (const persistent of [false, true]) {
    const f = await fixture(t, {}, { continuousFrames: true }), { page } = await f.open();
    let count = 0;
    interceptCapture(f.context(), frame => (++count <= 2 || persistent) ?
      { ...frame, data: Buffer.from("bad jpeg").toString("base64") } : frame);
    await page.evaluate(() => {
      let n = 0;
      window.animation = setInterval(() => document.body.style.backgroundColor = "hsl(" + (n++ * 19 % 360) + " 80% 50%)", 16);
    });
    const frame = await f.call("frame", {}, "GET");
    assert(frame.width > 0);
    const stats = (await f.call("health", {}, "GET")).capture;
    assert.equal(stats.fallbacks, persistent ? 1 : 0);
    assert.equal(stats.invalid_frames, persistent ? 3 : 2);
    assert(persistent ? stats.screenshots > 0 : stats.streamed_frames > 0);
  }
});

integration("startup stale sizes recover beyond two frames and remain bounded", async t => {
  for (const persistent of [false, true]) {
    const f = await fixture(t, {}, { continuousFrames: true }), { page } = await f.open();
    const old = (await page.screenshot({ type: "jpeg", quality: 85 })).toString("base64");
    await f.call("resize", { width: 800, height: 600 });
    let count = 0;
    interceptCapture(f.context(), frame => (++count <= 4 || persistent) ? { ...frame, data: old } : frame);
    await page.evaluate(() => {
      let n = 0;
      window.animation = setInterval(() => document.body.style.backgroundColor = "hsl(" + (n++ * 19 % 360) + " 80% 50%)", 16);
    });
    const started = performance.now();
    const frame = await f.call("frame", {}, "GET");
    assert.equal(frame.width, 800); assert.equal(frame.height, 600);
    const stats = (await f.call("health", {}, "GET")).capture;
    assert.equal(stats.fallbacks, persistent ? 1 : 0);
    assert(stats.invalid_frames >= 4 && stats.invalid_frames <= 17);
    assert(performance.now() - started < 5000, "startup size recovery must finish before the frame deadline");
    assert(persistent ? stats.screenshots > 0 : stats.streamed_frames > 0);
  }
});

integration("a late native resize during startup retains the last valid image", { timeout: 10000 }, async t => {
  const f = await fixture(t, {}, { continuousFrames: true }), { page } = await f.open();
  const old = (await page.screenshot({ type: "jpeg", quality: 85 })).toString("base64");
  await f.call("resize", { width: 800, height: 600 });
  let valid = false, stale = 0, signal;
  const observed = new Promise(resolve => { signal = resolve; });
  interceptCapture(f.context(), frame => {
    if (!valid) return frame;
    if (stale++ < 2) { if (stale === 2) signal(); return { ...frame, data: old }; }
    return frame;
  });
  await page.evaluate(() => {
    let n = 0;
    window.animation = setInterval(() => document.body.style.backgroundColor = "hsl(" + (n++ * 19 % 360) + " 80% 50%)", 16);
  });
  await f.call("frame", {}, "GET");
  valid = true;
  await observed;
  const frame = await f.call("frame", {}, "GET");
  assert.equal(frame.width, 800); assert.equal(frame.height, 600);
  const stats = (await f.call("health", {}, "GET")).capture;
  assert(stats.invalid_frames >= 2);
  assert.equal(stats.fallbacks, 0);
  assert.equal(stats.screenshots, 0);
});

test("JPEG dimensions identify clipped Mac compositor surfaces without trusting requested size", async () => {
  const { browserJPEGDimensions } = await import("../windows-browser.mjs");
  const header = (width, height, marker = 0xc0) => Buffer.from([
    0xff, 0xd8, 0xff, 0xe0, 0, 4, 0, 0,
    0xff, marker, 0, 17, 8, height >> 8, height & 255, width >> 8, width & 255,
    3, 1, 0x22, 0, 2, 0x11, 1, 3, 0x11, 1, 0xff, 0xd9,
  ]);
  assert.deepEqual(browserJPEGDimensions(header(2064, 1390).toString("base64")), { width: 2064, height: 1390 });
  assert.deepEqual(browserJPEGDimensions(header(2064, 1476, 0xc2).toString("base64")), { width: 2064, height: 1476 });
  for (const data of [Buffer.from("invalid"), header(64, 48).subarray(0, 18), Buffer.from([0xff,0xd8,0xff,0xc0,0,0])])
    assert.equal(browserJPEGDimensions(data.toString("base64")), null);
});

integration("Mac resized bitmap dimensions match metadata across repeated viewer reconnects", async t => {
  if (process.env.CAGE_BROWSER_TEST_PLATFORM !== "darwin") return t.skip("Mac compositor resize");
  const { browserJPEGDimensions } = await import("../windows-browser.mjs");
  const f = await fixture(t, { networkMode: "packet" });
  await f.open("/resize-surface");
  for (let i = 0; i < 24; i++) {
    const size = { width: i % 2 ? 2064 : 2066, height: i % 2 ? 1476 : 1478 };
    await f.call("resize", size);
    const frame = await f.call("frame", {}, "GET");
    assert.deepEqual(browserJPEGDimensions(frame.jpeg), { width: frame.width, height: frame.height });
    assert.deepEqual({ width: frame.width, height: frame.height }, size);
  }
});

integration("Mac slow fallback keeps its capture alive until the frame is acknowledged", async t => {
  if (process.env.CAGE_BROWSER_TEST_PLATFORM !== "darwin") return t.skip("Mac compositor fallback");
  const { browserJPEGDimensions } = await import("../windows-browser.mjs");
  const f = await fixture(t, { networkMode: "packet" });
  const one = await f.open("/slow-compositor");
  const originalSession = f.context().newCDPSession.bind(f.context());
  const stale = Buffer.from([0xff,0xd8,0xff,0xc0,0,17,8,0,40,0,64,3,1,0x22,0,2,0x11,1,3,0x11,1,0xff,0xd9]).toString("base64");
  f.context().newCDPSession = async page => {
    const session = await originalSession(page), on = session.on.bind(session), off = session.off.bind(session), listeners = new Map();
    session.on = (name, listener) => {
      if (name !== "Page.screencastFrame") return on(name, listener);
      const wrapped = frame => listener({ ...frame, data: stale });
      listeners.set(listener, wrapped); return on(name, wrapped);
    };
    session.off = (name, listener) => off(name, listeners.get(listener) || listener);
    return session;
  };
  const screenshot = one.page.screenshot.bind(one.page);
  one.page.screenshot = async options => { await new Promise(resolve => setTimeout(resolve, 1800)); return screenshot(options); };
  const frame = await f.call("frame", {}, "GET");
  assert.deepEqual(browserJPEGDimensions(frame.jpeg), { width: frame.width, height: frame.height });
});

integration("Mac headed browser keeps a native window and routes actual pointer movement", async t => {
  if (process.env.CAGE_BROWSER_TEST_PLATFORM !== "darwin" || process.env.CAGE_BROWSER_TEST_HEADED !== "1") return t.skip("headed Mac guest acceptance");
  const f = await fixture(t, { networkMode: "packet" });
  const one = await f.open("/native-gui-input");
  assert.equal(f.launchOptions().headless, false);
  const session = await one.page.context().newCDPSession(one.page);
  const window = await session.send("Browser.getWindowForTarget");
  assert(window.windowId > 0);
  assert.equal(window.bounds.windowState, "normal");
  await one.page.evaluate(() => { window.pointerMoves = []; document.addEventListener("mousemove", event => window.pointerMoves.push({ x: event.clientX, y: event.clientY, trusted: event.isTrusted })); });
  const frame = await f.call("frame", {}, "GET");
  for (const [x,y] of [[100,100],[200,120],[250,150]]) await f.call("input", { kind:"pointer",target:frame.target,x:x*2,y:y*2,mask:0 });
  const moves = await one.page.evaluate(() => window.pointerMoves);
  assert(moves.length >= 3);
  assert.deepEqual(moves.at(-1), { x:250,y:150,trusted:true });
  await session.detach();
});

integration("Mac frames preserve full viewport content after compositor updates", async t => {
  if (process.env.CAGE_BROWSER_TEST_PLATFORM !== "darwin") return t.skip("Mac compositor content");
  const f = await fixture(t, { networkMode: "packet", displayScale: 1 });
  const one = await f.open("/viewport-content");
  await one.page.setContent('<style>body{margin:0;background:#ff0000}#right{position:fixed;right:0;top:0;width:25vw;height:100vh;background:#0000ff}#center{position:fixed;left:45vw;top:0;width:10vw;height:100vh;background:#00ff00}</style><div id=right></div><div id=center></div>');
  for (const width of [1000, 1552, 1940, 1250]) {
    await f.call("resize", { width, height: 900 });
    await one.page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    for (let i = 0; i < 4; i++) {
      await one.page.evaluate(i => document.body.dataset.frame = String(i), i);
      const frame = await f.call("frame", {}, "GET");
      const pixels = await one.page.evaluate(async jpeg => {
        const blob = await (await fetch('data:image/jpeg;base64,' + jpeg)).blob();
        const bitmap = await createImageBitmap(blob), canvas = document.createElement('canvas');
        canvas.width = bitmap.width; canvas.height = bitmap.height;
        const ctx = canvas.getContext('2d'); ctx.drawImage(bitmap, 0, 0);
        return [0.1,0.5,0.9].map(x => [...ctx.getImageData(Math.floor(canvas.width*x), Math.floor(canvas.height/2), 1, 1).data].slice(0,3));
      }, frame.jpeg);
      for (const [index, channel] of [[0,0],[1,1],[2,2]]) {
        assert(pixels[index][channel] > 220 && pixels[index].filter((_, n) => n !== channel).every(v => v < 30), JSON.stringify({width,i,pixels}));
      }
      await new Promise(resolve => setTimeout(resolve, 100));
    }
  }
});
