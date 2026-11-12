// This process runs over the standard user's pinned guest SSH session. Chrome's
// DevTools connection uses inherited pipes, and host WebKit receives only pixels.
import { chromium } from "/usr/local/libexec/cage-workbench-browser-4.145.0-pw1.65.0/playwright/index.mjs";
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdir, rm, writeFile } from "node:fs/promises";
import { homedir } from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { once } from "node:events";

const editorSessions = path.join(
  homedir(),
  "Library",
  "Application Support",
  "Cage",
  "EditorSessions",
);
const profile = path.join(
  homedir(),
  "Library",
  "Application Support",
  "Cage",
  "Browser",
);
let editorConfig;
let browser, page, editor, editorFolder, editorAddress, editorPassword, closing;
function close() {
  return (closing ??= (async () => {
    if (editor) editor.kill();
    await browser?.close().catch(() => {});
    if (editorConfig) await rm(editorConfig, { force: true });
  })());
}
async function getPage() {
  if (!browser) {
    await mkdir(profile, { recursive: true, mode: 0o700 });
    browser = await chromium.launchPersistentContext(profile, {
      executablePath:
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
      headless: true,
      chromiumSandbox: true,
      viewport: { width: 1140, height: 700 },
      args: ["--disable-blink-features=AutomationControlled", "--disable-quic", "--no-default-browser-check"],
      ignoreDefaultArgs: ["--enable-automation"],
      acceptDownloads: false,
      timeout: 20000,
    });
    browser.setDefaultTimeout(7000);
    page = browser.pages()[0] ?? (await browser.newPage());
    browser.on("page", (p) => {
      page = p;
    });
    browser.on("close", () => {
      browser = undefined;
      page = undefined;
    });
  }
  if (!page || page.isClosed())
    page = browser.pages().at(-1) ?? (await browser.newPage());
  return page;
}
async function openEditor(folder) {
  const p = await getPage();
  if (
    editorFolder !== folder ||
    !editor ||
    editor.exitCode !== null ||
    !editorAddress
  ) {
    if (editor) editor.kill();
    editorPassword = randomBytes(32).toString("hex");
    const project = path.basename(folder);
    if (
      !/^[0-9a-f]{24}$/.test(project) ||
      folder !== path.join(homedir(), "Cage", "Projects", project)
    )
      throw new Error("请选择已授权的客机项目");
    await mkdir(editorSessions, { recursive: true, mode: 0o700 });
    if (editorConfig) await rm(editorConfig, { force: true });
    editorConfig = path.join(editorSessions, project + ".yaml");
    await writeFile(
      editorConfig,
      "bind-addr: 127.0.0.1:0\nauth: password\npassword: " +
        editorPassword +
        "\ncert: false\n",
      { mode: 0o600 },
    );
    const child = spawn(
      "/usr/local/libexec/cage-macos-terminal",
      ["--editor", folder],
      {
        stdio: ["pipe", "pipe", "pipe"],
      },
    );
    editor = child;
    child.stdin.on("error", () => {});
    editorAddress = await new Promise((resolve, reject) => {
      let recent = "";
      const timer = setTimeout(
        () => finish(new Error("客机编辑器未能启动，请检查项目授权")),
        12000,
      );
      const read = (data) => {
        recent = (recent + data.toString()).slice(-6000);
        const address = recent.match(
          /HTTP server listening on (http:\/\/127\.0\.0\.1:[1-9][0-9]*)/,
        );
        if (address) finish(null, address[1]);
      };
      const failed = () =>
        finish(new Error("请先在 Cage 授权此项目，再打开编辑器"));
      function finish(error, address) {
        clearTimeout(timer);
        child.stdout.off("data", read);
        child.stderr.off("data", read);
        child.off("error", failed);
        child.off("exit", failed);
        if (error) {
          child.kill();
          reject(error);
        } else {
          child.stdout.resume();
          child.stderr.resume();
          resolve(address);
        }
      }
      child.stdout.on("data", read);
      child.stderr.on("data", read);
      child.once("error", failed);
      child.once("exit", failed);
    });
    editorFolder = folder;
    child.once("exit", () => {
      if (editor === child) {
        editor = undefined;
        editorAddress = undefined;
      }
    });
  }
  const login = await p.request.post(editorAddress + "/login", {
    form: { password: editorPassword },
    maxRedirects: 0,
    timeout: 7000,
  });
  if (![302, 303].includes(login.status()))
    throw new Error("客机编辑器登录失败");
  await login.dispose();
  await p.goto(editorAddress + "/?folder=" + encodeURIComponent(folder), {
    waitUntil: "commit",
  });
}
const keys = new Set([
  "Tab",
  "Enter",
  "Backspace",
  "Delete",
  "Escape",
  "ArrowLeft",
  "ArrowRight",
  "ArrowUp",
  "ArrowDown",
  "Home",
  "End",
  "PageUp",
  "PageDown",
  "Space",
  "F5",
  "F8",
  "F9",
  "F10",
  "F11",
]);
// Observe a fresh document in the actual guest browser. The separate network
// context carries no workbench session cookies and can reach only the fixed diagnostic service.
async function audit(timezone, network) {
  await getPage();
  const previous = page;
  const settingsPage = await browser.newPage();
  let settings;
  try {
    settings = await settingsPage.evaluate(
      (zone) => ({
        timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
        expected_timezone: new Intl.DateTimeFormat("en", {
          timeZone: zone,
        }).resolvedOptions().timeZone,
        language: navigator.language,
        platform: navigator.platform,
      }),
      timezone,
    );
  } finally {
    await settingsPage.close();
    page = previous;
  }
  if (!network) return settings;
  const owner = browser.browser();
  if (!owner) throw new Error("无法创建独立的浏览器诊断会话");
  const isolated = await owner.newContext({ serviceWorkers: "block" });
  try {
    const test = await isolated.newPage();
    await test.goto("http://198.18.0.254/", {
      waitUntil: "domcontentloaded",
      timeout: 12000,
    });
    settings.network = await test.evaluate(async () => {
      if (location.origin !== "http://198.18.0.254")
        throw new Error("无效的诊断地址");
      try {
        const r = await fetch("/egress", {
          cache: "no-store",
          credentials: "omit",
          signal: AbortSignal.timeout(14000),
        });
        return { ...(await r.json()), status: r.status };
      } catch {
        return { status: 0 };
      }
    });
    return settings;
  } finally {
    await isolated.close();
  }
}

async function handle(a) {
  if (a.action === "audit") return audit(a.timezone, a.network);
  const p = await getPage();
  switch (a.action) {
    case "frame": {
      const data = await p.screenshot({
        type: "jpeg",
        quality: 70,
        timeout: 5000,
      });
      return {
        image: data.toString("base64"),
        url: p.url(),
        title: await p.title(),
        width: p.viewportSize().width,
        height: p.viewportSize().height,
      };
    }
    case "editor":
      await openEditor(a.folder);
      break;
    case "navigate": {
      const u = new URL(a.url);
      if (!["http:", "https:"].includes(u.protocol) || u.username || u.password)
        throw new Error("请输入 HTTP 或 HTTPS 地址");
      await p.goto(u.href, { waitUntil: "commit", timeout: 8000 });
      break;
    }
    case "click":
      await p.mouse.click(a.x, a.y, { clickCount: a.double ? 2 : 1 });
      break;
    case "wheel":
      await p.mouse.move(a.x, a.y);
      await p.mouse.wheel(a.dx, a.dy);
      break;
    case "text":
      if (typeof a.text !== "string" || a.text.length > 4096)
        throw new Error("输入过长");
      await p.keyboard.insertText(a.text);
      break;
    case "key": {
      if (!(keys.has(a.key) || /^[a-z0-9]$/i.test(a.key)))
        throw new Error("不支持此键");
      await p.keyboard.press(
        [
          a.meta && "Meta",
          a.ctrl && "Control",
          a.shift && "Shift",
          a.alt && "Alt",
          a.key,
        ]
          .filter(Boolean)
          .join("+"),
      );
      break;
    }
    case "reload":
      await p.reload({ waitUntil: "commit" });
      break;
    case "back":
      await p.goBack({ waitUntil: "commit" });
      break;
    case "forward":
      await p.goForward({ waitUntil: "commit" });
      break;
    default:
      throw new Error("未知的客机浏览器操作");
  }
  return { ok: true };
}
process.once("SIGHUP", () => {
  void close().finally(() => process.exit(0));
});
process.once("SIGTERM", () => {
  void close().finally(() => process.exit(0));
});
try {
  const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
  for await (const line of lines) {
    let result;
    try {
      result = await handle(JSON.parse(line));
    } catch (error) {
      result = { error: String(error.message).split("\n")[0].slice(0, 220) };
    }
    if (!process.stdout.write(JSON.stringify(result) + "\n"))
      await once(process.stdout, "drain");
  }
} finally {
  await close();
}
