import assert from "node:assert/strict";
import { createRequire } from "node:module";
const require = createRequire(import.meta.url);
const { chromium } = require(
  process.env.CAGE_PLAYWRIGHT_MODULE || "playwright",
);
const browser = await chromium.launch({
  headless: true,
  executablePath: process.env.CAGE_TEST_BROWSER_PATH,
});
const page = await browser.newPage();
const errors = [];
page.on("pageerror", (error) => errors.push(error.message));
const origin = process.env.CAGE_UI_URL || "http://127.0.0.1:5173";
const received = [];
let release,
  fail = false;
await page.route("**/api/**", route => route.fulfill({ json: { ok: true } }));
await page.route("**/api/appearance", (route) => route.fulfill({ json: {} }));
await page.route("**/api/input/paste/start", route => route.fulfill({ json: { available: true, context: "a".repeat(32) } }));
await page.route("**/api/input/image", async (route) => {
  received.push(route.request().postDataJSON());
  await new Promise((resolve) => {
    release = resolve;
  });
  await route.fulfill(
    fail
      ? { status: 502, contentType: "text/plain; charset=utf-8", body: "测试：图片传递失败" }
      : {
          json: {
            path: "/home/cage/.local/share/cage/attachments/image-test.png",
            ok: true,
          },
        },
  );
});
const messages = [];
await page.routeWebSocket("**/api/terminals/**", (socket) => {
  socket.onMessage((data) => messages.push(JSON.parse(data)));
  socket.send(Buffer.from("\x1b[?2004hImage input acceptance\r\n$ "));
});
const html = `<div id="remote" style="height:220px"></div><input id="local" aria-label="本机输入框"><div id="terminal" style="height:400px"></div><div id="status" role="status"></div>`;
await page.route("**/image-input-test", (route) =>
  route.fulfill({ contentType: "text/html", body: html }),
);
const poll = async (predicate) => {
  for (let i = 0; i < 100; i++) {
    if (predicate()) return;
    await new Promise((r) => setTimeout(r, 30));
  }
  throw new Error("fixture state timed out");
};
const imageEvent = (selector, type, image = true) =>
  page.locator(selector).evaluate(
    async (element, { type, image }) => {
      const canvas = document.createElement("canvas");
      canvas.width = 32;
      canvas.height = 24;
      canvas.getContext("2d").fillRect(0, 0, 32, 24);
      const blob = image
        ? await new Promise((resolve) => canvas.toBlob(resolve, "image/png"))
        : new Blob(["not an image"], { type: "text/plain" });
      const data = new DataTransfer();
      data.items.add(
        new File([blob], image ? "test.png" : "test.txt", { type: blob.type }),
      );
      element.dispatchEvent(
        type === "paste"
          ? new ClipboardEvent("paste", {
              bubbles: true,
              cancelable: true,
              clipboardData: data,
            })
          : new DragEvent("drop", {
              bubbles: true,
              cancelable: true,
              dataTransfer: data,
            }),
      );
    },
    { type, image },
  );
try {
  await page.goto(origin + "/image-input-test");
  await page.evaluate(async () => {
    const [{ default: RFB }, { installRemoteInput }, { sendImage, imagePayload }] =
      await Promise.all([
        import("/.generated/novnc/core/rfb.js"),
        import("/src/remote-input.js"),
        import("/src/image-transfer.js"),
      ]);
    const rfb = new EventTarget(),
      container = document.getElementById("remote"),
      order = [];
    for (const name of ["_cageQueueInput", "cagePasteContent", "sendKey"])
      rfb[name] = RFB.prototype[name].bind(rfb);
    rfb.cageSetKeyboardTarget = (target) => {
      rfb.target = target;
    };
    rfb.focus = () => rfb.target?.focus();
    rfb._cageRunInput = (action) => action();
    rfb._cageSendKey = (sym) => order.push(sym);
    rfb._cagePasteText = (text) => {
      order.push(text);
      return Promise.resolve();
    };
    rfb.cagePasteText = (text) =>
      rfb._cageQueueInput(() => rfb._cagePasteText(text));
    let active = true;
    const input = installRemoteInput({
      events: rfb,
      setKeyboardTarget: target => rfb.cageSetKeyboardTarget(target),
      sendKey: (...key) => rfb.sendKey(...key),
      focus: () => rfb.focus(),
      pasteText: text => rfb.cagePasteText(text),
      runKeyAction: action => rfb._cageQueueInput(() => action((...key) => rfb._cageSendKey(...key))),
    }, container, {
      mode: () => "mac",
      isActive: () => active,
      insertText: async () => {},
      onStatus: (text) => {
        document.getElementById("status").textContent = text;
      },
      onFallback() {},
      pasteContent: (content) =>
        rfb.cagePasteContent(
          content,
          async (payload) => {
            await sendImage(payload.files ? await imagePayload(payload.files[0]) : payload, "browser", "tab-1");
            order.push("image");
          },
          () => active && Boolean(rfb.target),
        ),
    });
    window.probe = {
      rfb,
      order,
      input,
      deactivate: () => {
        active = false;
      },
    };
    rfb.focus();
  });
  await imageEvent("[aria-label='Cage 网页输入']", "paste");
  await poll(() => received.length === 1);
  await page.evaluate(() => probe.rfb.sendKey(0xff0d, "Enter", true));
  assert.deepEqual(await page.evaluate(() => probe.order), []);
  release();
  await page.waitForFunction(() => probe.order.length === 2);
  assert.deepEqual(await page.evaluate(() => probe.order), ["image", 0xff0d]);
  assert.equal(received[0].target, "browser");
  assert.equal(
    Buffer.from(received[0].png, "base64").subarray(1, 4).toString(),
    "PNG",
  );
  await imageEvent("#remote", "drop");
  await poll(() => received.length === 2);
  release();
  await page.getByRole("status").filter({ hasText: "已传递" }).waitFor();
  await imageEvent("#remote", "drop", false);
  await page.getByRole("status").filter({ hasText: "请选择 PNG" }).waitFor();
  assert.equal(received.length, 2);
  await page.locator("#local").focus();
  await imageEvent("#local", "paste");
  assert.equal(received.length, 2, "local fields must not upload");
  const cancelled = await page.evaluate(async () => {
    let finish;
    const delayed = new Promise((resolve) => {
      finish = resolve;
    });
    const pending = probe.rfb.cagePasteContent(
      delayed,
      () => {
        throw new Error("stale image was sent");
      },
      () => false,
    );
    finish({ png: "unused" });
    try {
      await pending;
      return false;
    } catch (error) {
      return error.message === "Input target changed";
    }
  });
  assert.equal(
    cancelled,
    true,
    "clipboard completion after a target change must cancel",
  );
  await page.evaluate(() => {
    probe.input.dispose();
  });
  await page.evaluate(async () => {
    const { default: RefreshRuntime } = await import("/@react-refresh");
    RefreshRuntime.injectIntoGlobalHook(window);
    window.$RefreshReg$ = () => {};
    window.$RefreshSig$ = () => (type) => type;
    window.__vite_plugin_react_preamble_installed__ = true;
    await import("/scripts/image-input-fixture.jsx");
  });
  await page.locator(".xterm-helper-textarea").waitFor();
  await page.locator(".xterm-helper-textarea").focus();
  await imageEvent(".xterm-helper-textarea", "paste");
  await poll(() => received.length === 3);
  await page.keyboard.press("Enter");
  assert.equal(messages.filter((m) => m.type === "input").length, 0);
  release();
  await poll(() => messages.filter((m) => m.type === "input").length === 2);
  assert.deepEqual(
    messages.filter((m) => m.type === "input").map((m) => m.data),
    [
      "\x1b[200~'/home/cage/.local/share/cage/attachments/image-test.png' \x1b[201~",
      "\r",
    ],
  );
  assert.equal(received[2].target, "terminal");
  fail = true;
  await imageEvent(".xterm-helper-textarea", "paste");
  await poll(() => received.length === 4);
  await page.keyboard.press("Enter");
  release();
  await page.getByRole("status").filter({ hasText: "图片传递失败" }).waitFor();
  assert.equal(
    messages.filter((m) => m.type === "input").length,
    2,
    "failed image cancels queued submit",
  );
  assert.deepEqual(errors, []);
  console.log(
    "PASS: remote paste/drop PNG, image-before-Enter, unsupported files, local fields, real xterm bracketed path paste and failed-submit cancellation",
  );
} finally {
  await browser.close();
}
