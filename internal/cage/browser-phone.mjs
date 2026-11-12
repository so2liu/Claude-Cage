// One displayed page and private debugging pipe per active phone browser.
// The desktop process, its targets and its profile are never attached here.
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile, rename } from "node:fs/promises";
import { join } from "node:path";
import { phoneError, startPhoneProxy, webAddress } from "./browser-phone-network.mjs";

const idleTimeout = 2 * 60 * 1000;
const validID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const keys = new Map([
  ["Enter", 13], ["Tab", 9], ["Escape", 27], ["Backspace", 8], ["Delete", 46],
  ["ArrowLeft", 37], ["ArrowUp", 38], ["ArrowRight", 39], ["ArrowDown", 40],
]);

function dimensions({ width, height }) {
  if (!Number.isInteger(width) || width < 240 || width > 1440 ||
      !Number.isInteger(height) || height < 160 || height > 1600) throw new Error("手机视口大小无效");
  return { width, height };
}

class BrowserPipe {
  constructor(binary, args) {
    this.calls = new Map();
    this.sequence = 0;
    this.process = spawn(binary, args, { stdio: ["ignore", "ignore", "ignore", "pipe", "pipe"] });
    this.closed = new Promise(resolve => this.process.once("close", resolve));
    let buffered = [];
    const disconnected = () => {
      this.dead = true;
      for (const call of this.calls.values()) call.reject(phoneError(410, "远程浏览器已关闭"));
      this.calls.clear();
      this.process.kill();
    };
    this.process.on("error", disconnected);
    this.process.once("exit", disconnected);
    for (const stream of this.process.stdio.slice(3)) stream.on("error", disconnected);
    this.process.stdio[4].on("data", data => {
      let end;
      while ((end = data.indexOf(0)) !== -1) {
        buffered.push(data.subarray(0, end));
        let event;
        try { event = JSON.parse(Buffer.concat(buffered)); } catch { disconnected(); return; }
        data = data.subarray(end + 1);
        buffered = [];
        if (!event.id) { this.onEvent?.(event); continue; }
        const call = this.calls.get(event.id);
        if (!call) continue;
        this.calls.delete(event.id);
        if (event.error) call.reject(new Error("Chromium 暂时无法完成此操作"));
        else call.resolve(event.result);
      }
      if (data.length) buffered.push(data);
    });
  }
  async send(method, params = {}, sessionId) {
    if (this.dead) throw phoneError(410, "远程浏览器已关闭");
    const id = ++this.sequence;
    let timer;
    try {
      return await new Promise((resolve, reject) => {
        // The first CDP command includes macOS Chrome's cold process startup.
        let timeout = method === "Page.navigate" || method === "Page.navigateToHistoryEntry" ? 12000 : 4500;
        if (method === "Target.getTargets" && process.platform === "darwin") timeout = 75000;
        timer = setTimeout(() => reject(new Error("远程浏览器操作超时")), timeout);
        this.calls.set(id, { resolve, reject });
        this.process.stdio[3].write(JSON.stringify({ id, method, params, sessionId }) + "\0");
      });
    } finally { clearTimeout(timer); this.calls.delete(id); }
  }
  async close() {
    if (!this.dead) void this.send("Browser.close").catch(() => {});
    const timeout = setTimeout(() => this.process.kill("SIGKILL"), 1800);
    try { await this.closed; } finally { clearTimeout(timeout); }
  }
}

// Executed in an isolated world in the current top-level document. A password
// only enters the page through Input.insertText; no field value is inspected.
const inspectFocus = `(() => {
  let node = document.activeElement;
  while (node?.shadowRoot?.activeElement) node = node.shadowRoot.activeElement;
  const input = node instanceof HTMLInputElement;
  const editable = node && !node.readOnly && !node.disabled &&
    (node.isContentEditable || node instanceof HTMLTextAreaElement ||
      (input && ["text","email","search","url","tel","password","number"].includes(node.type)));
  if (!editable) return { focus: "", password: false };
  const ids = globalThis.cageInputs ??= new WeakMap();
  if (!ids.has(node)) ids.set(node, String(globalThis.cageSequence = (globalThis.cageSequence || 0) + 1));
  return { focus: ids.get(node), password: input && node.type === "password" };
})()`;

async function saveLanguage(profile, language) {
  const directory = join(profile, "Default");
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const file = join(directory, "Preferences");
  let settings = {};
  try { settings = JSON.parse(await readFile(file, "utf8")); }
  catch (error) { if (error.code !== "ENOENT") throw new Error("手机浏览器资料暂时无法读取"); }
  settings.intl ??= {};
  settings.intl.accept_languages = language;
  settings.intl.selected_languages = language;
  const next = file + ".phone-next";
  await writeFile(next, JSON.stringify(settings), { mode: 0o600 });
  await rename(next, file);
}

export class PhoneBrowser {
  constructor({ binary = "/usr/bin/chromium", directory = "/home/cage-browser/phone-profiles",
                language = process.env.CAGE_LANGUAGE || "en-US", connect, pipeFactory } = {}) {
    this.binary = binary;
    this.directory = directory;
    this.language = language;
    this.connect = connect;
    this.pipeFactory = pipeFactory ?? ((binary, args) => new BrowserPipe(binary, args));
    this.queue = Promise.resolve();
    this.pending = 0;
  }
  run(operation, input, signal) {
    // Serialize screenshot/context checks with all layout and input changes.
    if (this.pending >= 12) return Promise.reject(new Error("手机浏览器正在处理上一项操作"));
    this.pending++;
    const task = this.queue.then(() => this.perform(operation, input, signal));
    this.queue = task.catch(() => {}).finally(() => { this.pending--; });
    return task;
  }
  async perform(operation, input, signal) {
    if (!input || !validID.test(input.id)) throw new Error("手机浏览器会话无效");
    if (operation === "open") {
      const size = dimensions(input);
      if (typeof input.profile !== "string" || !/^[a-f0-9]{64}$/.test(input.profile)) throw new Error("手机浏览器资料无效");
      const url = input.url ? webAddress(input.url) : "";
      if (!this.session) {
        try { await this.open(input.id, input.profile, size, url, signal); }
        catch (error) {
          if (error.status === 403 || error.status === 409) throw error;
          throw phoneError(503, "客机浏览器暂时无法启动，请稍后重新打开");
        }
      }
      else if (input.profile !== this.session.profile) throw phoneError(409, "另一部手机正在使用浏览器");
    }
    const s = this.session;
    if (!s || s.id !== input.id) throw phoneError(410, "手机浏览器会话已结束，请重新打开");
    clearTimeout(this.timer);
    this.timer = setTimeout(() => { void this.run("close", { id: s.id }).catch(() => {}); }, idleTimeout).unref();
    if (operation === "open") return { id: s.id };
    if (operation === "frame") return this.frame(s, input.after);
    if (operation === "action") {
      await this.action(s, input);
      if (input.return_frame === true) {
        // Navigation can invalidate the screenshot context after the input was
        // delivered. Acknowledge the input so the phone retries only the frame.
        try { return await this.frame(s); } catch { return { id: s.id }; }
      }
    } else if (operation === "close") await this.dispose(s);
    else throw new Error("未知的手机浏览器操作");
    return { id: input.id };
  }
  async open(id, profile, size, url, signal) {
    const data = join(this.directory, profile);
    await saveLanguage(data, this.language);
    const proxy = await startPhoneProxy(this.connect);
    let pipe;
    try { pipe = await this.pipeFactory(this.binary, [
      "--headless=new", "--disable-extensions", "--disable-quic", "--disable-background-networking",
      "--no-first-run", "--no-default-browser-check", "--disable-session-crashed-bubble",
      "--disable-sync", "--disable-dev-shm-usage", "--remote-debugging-pipe",
      "--disable-blink-features=AutomationControlled",
      "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
      "--proxy-server=socks5://127.0.0.1:" + proxy.port, "--proxy-bypass-list=<-loopback>",
      "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1",
      "--lang=" + this.language, "--accept-lang=" + this.language, "--user-data-dir=" + data, "about:blank",
    ]); } catch (error) { proxy.close(); throw error; }
    const s = { id, profile, ...size, pipe, proxy };
    this.session = s;
    let stopping;
    const stop = () => { stopping ??= this.dispose(s); };
    signal?.addEventListener("abort", stop, { once: true });
    pipe.closed.then(() => {
      proxy.close();
      if (this.session === s) { this.session = null; clearTimeout(this.timer); }
    });
    try {
      signal?.throwIfAborted();
      const { targetInfos } = await pipe.send("Target.getTargets");
      const target = targetInfos.find(t => t.type === "page" && t.url === "about:blank");
      if (!target) throw phoneError(503, "客机浏览器尚未就绪，请稍后重新打开");
      s.targetId = target.targetId;
      const { sessionId } = await pipe.send("Target.attachToTarget", { targetId: target.targetId, flatten: true });
      s.send = (method, params) => pipe.send(method, params, sessionId);
      pipe.onEvent = ({ method, params, sessionId: src }) => {
        if (method === "Target.targetCreated" && params.targetInfo.type === "page" && params.targetInfo.targetId !== target.targetId)
          void pipe.send("Target.closeTarget", { targetId: params.targetInfo.targetId }).catch(() => {});
        if (src === sessionId && method === "Page.javascriptDialogOpening")
          void s.send("Page.handleJavaScriptDialog", { accept: false }).catch(() => {});
      };
      await pipe.send("Target.setDiscoverTargets", { discover: true });
      await pipe.send("Browser.setDownloadBehavior", { behavior: "deny" });
      await s.send("Page.enable");
      // Some sites select their layout on the first HTTP request. Device
      // metrics alone still send Chromium's desktop UA and mobile hint.
      const { userAgent } = await pipe.send("Browser.getVersion");
      const platform = process.platform === "darwin" ? "macOS" : process.platform === "win32" ? "Windows" : "Linux";
      await s.send("Emulation.setUserAgentOverride", {
        userAgent: userAgent.replace(" Safari/", " Mobile Safari/"),
        // Chromium supplies its own brands and full versions when omitted.
        userAgentMetadata: { platform, platformVersion: "", architecture: "", model: "", mobile: true, formFactors: ["Mobile"] },
      });
      await s.send("Page.setInterceptFileChooserDialog", { enabled: true });
      await this.resize(s, size);
      await s.send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 1 });
      if (url) await s.send("Page.navigate", { url });
    } catch (error) { await (stopping ?? this.dispose(s)); throw error; }
    finally { signal?.removeEventListener("abort", stop); }
  }
  async dispose(s = this.session) {
    if (!s) return;
    clearTimeout(this.timer);
    s.proxy.close();
    await s.pipe.close();
    if (this.session === s) this.session = null;
  }
  async resize(s, size) {
    // Keep the page in phone CSS pixels, with enough image pixels for legible
    // text. Cap the raster size so a tablet frame still fits the paired link.
    const density = Math.max(1, Math.min(2, Math.sqrt(2_000_000 / (size.width * size.height))));
    await s.send("Emulation.setDeviceMetricsOverride", { ...size, screenWidth: size.width, screenHeight: size.height,
      deviceScaleFactor: density, mobile: true });
    Object.assign(s, size);
  }
  async document(s) {
    const [{ frameTree: { frame } }, { cssVisualViewport: viewport }] = await Promise.all([
      s.send("Page.getFrameTree"), s.send("Page.getLayoutMetrics"),
    ]);
    const failed = frame.url === "chrome-error://chromewebdata/";
    const url = frame.url === "about:blank" ? "" : webAddress(failed ? frame.unreachableUrl : frame.url + (frame.urlFragment || ""));
    const token = createHash("sha256").update(JSON.stringify([frame.loaderId, url, s.width, s.height,
      viewport.scale, viewport.offsetX, viewport.offsetY])).digest("hex");
    return { url, token, failed, scale: viewport.scale, frameId: frame.id, loaderId: frame.loaderId };
  }
  async focus(s, { frameId, loaderId }) {
    const { executionContextId } = await s.send("Page.createIsolatedWorld", { frameId, worldName: "CagePhone" });
    const result = await s.send("Runtime.evaluate", { expression: inspectFocus, contextId: executionContextId, returnByValue: true });
    const field = result.result.value;
    if (!field?.focus) return { focus: "", password: false };
    // Opening the phone keyboard changes the frame geometry, but the same
    // DOM input and its draft remain valid until the document or focus changes.
    const token = createHash("sha256").update(JSON.stringify([frameId, loaderId, field.focus])).digest("hex");
    return { focus: token, password: field.password };
  }
  async frame(s, previousTag) {
    const before = await this.document(s);
    const [picture, info, focus] = await Promise.all([
      s.send("Page.captureScreenshot", { format: "jpeg", quality: 70, captureBeyondViewport: false }),
      s.pipe.send("Target.getTargetInfo", { targetId: s.targetId }),
      before.failed ? { focus: "", password: false } : this.focus(s, before),
    ]);
    const after = await this.document(s);
    if (before.token !== after.token) throw phoneError(409, "页面正在切换，请重新读取画面");
    if (!picture.data || picture.data.length > 2500000) throw new Error("远程画面过大，请减小视口");
    const metadata = { id: s.id, url: after.url, title: info.targetInfo.title.slice(0, 512), width: s.width,
      height: s.height, document: after.token, ...focus };
    const tag = createHash("sha256").update(JSON.stringify(metadata)).update(picture.data, "ascii").digest("hex").slice(0, 32);
    if (previousTag === tag) return { id: s.id, frame_tag: tag, unchanged: true };
    return { ...metadata, frame_tag: tag, image: picture.data };
  }
  async action(s, input) {
    const { type } = input;
    if (type === "resize") return this.resize(s, dimensions(input));
    if (type === "navigate") return s.send("Page.navigate", { url: webAddress(input.url) });
    const doc = await this.document(s);
    if (doc.token !== input.document) throw phoneError(409, "页面已切换，请重新读取画面");
    if (type === "back" || type === "forward") {
      const history = await s.send("Page.getNavigationHistory");
      const entry = history.entries[history.currentIndex + (type === "back" ? -1 : 1)];
      if (entry) { if (entry.url !== "about:blank") webAddress(entry.url); await s.send("Page.navigateToHistoryEntry", { entryId: entry.id }); }
      return;
    }
    if (type === "reload") return s.send("Page.reload");
    if (doc.failed) throw new Error("网页加载失败，请检查地址或重新载入");
    if (type === "text" || type === "key") {
      if (type === "text" || input.key === "Enter" || input.focus) {
        const actual = await this.focus(s, doc);
        if (!actual.focus || actual.focus !== input.focus) throw phoneError(409, "输入框已改变，请重新点选");
      }
      if (type === "text") {
        if (typeof input.text !== "string" || !input.text || Buffer.byteLength(input.text) > 8192) throw new Error("输入文字无效");
        return s.send("Input.insertText", { text: input.text });
      }
      const code = keys.get(input.key);
      if (!code) throw new Error("按键不受支持");
      const key = { key: input.key, code: input.key, windowsVirtualKeyCode: code };
      await s.send("Input.dispatchKeyEvent", { ...key, type: "keyDown", ...(input.key === "Enter" ? { text: "\r" } : {}) });
      await s.send("Input.dispatchKeyEvent", { ...key, type: "keyUp" });
      return;
    }
    if (!Number.isFinite(input.x) || !Number.isFinite(input.y) ||
        input.x < 0 || input.y < 0 || input.x >= s.width || input.y >= s.height) throw new Error("触点不在当前画面内");
    // CDP takes CSS coordinates; pages without a viewport meta tag can
    // render a 980px layout scaled down into a 390px phone image.
    const x = input.x / doc.scale, y = input.y / doc.scale;
    if (type === "tap") {
      await s.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x, y }] });
      await s.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    } else if (type === "scroll") {
      if (![input.dx, input.dy].every(n => Number.isFinite(n) && Math.abs(n) <= 3200)) throw new Error("滚动距离无效");
      await s.send("Input.dispatchMouseEvent", { type: "mouseWheel", x, y, deltaX: input.dx / doc.scale, deltaY: input.dy / doc.scale });
    } else throw new Error("未知的浏览器动作");
  }
}
