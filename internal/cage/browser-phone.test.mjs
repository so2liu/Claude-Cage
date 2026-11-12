import assert from "node:assert/strict";
import { once } from "node:events";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import net from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import { setTimeout as delay } from "node:timers/promises";
import test from "node:test";
import { PhoneBrowser } from "./browser-phone.mjs";
import { webAddress } from "./browser-phone-network.mjs";
import { pathToFileURL } from "node:url";
import { playwrightPhonePipe } from "../../scripts/guest-os/windows-phone-browser.mjs";

test("phone navigation accepts public web URLs", () => {
  for (const url of ["file:///tmp/file", "chrome://settings", "http://2130706433", "http://10.0.0.1",
    "http://localhost./", "http://db.internal", "http://[::1]", "https://user:pass@example.com"]) {
    assert.throws(() => webAddress(url));
  }
  assert.equal(webAddress("https://example.org/path"), "https://example.org/path");
});

test("a failed Chromium launch reports an unavailable browser and releases the session", async t => {
  const directory = await mkdtemp(join(tmpdir(), "cage-phone-launch-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const phone = new PhoneBrowser({ binary: join(directory, "missing-browser"), directory });
  const id = randomUUID();
  await assert.rejects(phone.run("open", { id, profile: "a".repeat(64), width: 390, height: 640 }), { status: 503 });
  await assert.rejects(phone.run("frame", { id }), { status: 410 });
  await writeFile(join(directory, "b".repeat(64)), "not a profile directory");
  await assert.rejects(phone.run("open", { id, profile: "b".repeat(64), width: 390, height: 640 }), { status: 503 });
});

// CAGE_TEST_CHROMIUM points at an installed, sandboxed Chromium. Only the
// fixture hostname is mapped to loopback; Chromium uses the production SOCKS
// listener, including its denial of requests for private literal addresses.
test("real phone Chromium routes, resizes, types, and keeps paired profiles separate",
  { skip: !process.env.CAGE_TEST_CHROMIUM }, async t => {
    const directory = await mkdtemp(join(tmpdir(), "cage-phone-browser-"));
    t.after(() => rm(directory, { recursive: true, force: true }));
    const seen = [];
    let acceptLanguage;
    const site = http.createServer((req, res) => {
      seen.push(req.url);
      if (req.url === "/remember" || req.url === "/restored") acceptLanguage = req.headers["accept-language"];
      if (req.url === "/private") {
        res.writeHead(302, { Location: `http://127.0.0.1:${site.address().port}/forbidden` });
        return res.end();
      }
      if (req.url === "/private-ws") {
        res.setHeader("Content-Type", "text/html; charset=utf-8");
        return res.end(`<meta name="viewport" content="width=device-width,initial-scale=1"><script>
          const socket=new WebSocket('ws://127.0.0.1:${site.address().port}/forbidden-ws');
          document.title=JSON.stringify([innerWidth,innerHeight,'ws-attempted']);
        </script>`);
      }
      if (req.url === "/remember") res.setHeader("Set-Cookie", "state=known; Path=/; Max-Age=3600");
      res.setHeader("Content-Type", "text/html; charset=utf-8");
      if (req.url === "/wide") return res.end(`<style>body{margin:0}input{position:absolute;left:450px;top:40px;width:200px;height:48px}</style>
        <input><script>onload = () => document.title = JSON.stringify([innerWidth,visualViewport.scale]);</script>`);
      res.end(`<meta name="viewport" content="width=device-width,initial-scale=1">
        <style>body{margin:0;height:320vh}input{display:block;width:200px;height:50px}</style>
        <input aria-label="draft"><input type=password aria-label="password">
        <script>
        const show = () => document.title = JSON.stringify([innerWidth, innerHeight,
          document.querySelector("input").value, document.cookie, scrollY, navigator.maxTouchPoints, navigator.language, Intl.DateTimeFormat().resolvedOptions().timeZone, devicePixelRatio,
          navigator.webdriver, Object.getOwnPropertyDescriptor(Navigator.prototype, 'webdriver').get.toString()]);
        oninput = onresize = onscroll = show; show();
        </script>`);
    });
    site.listen(0, "127.0.0.1");
    site.on("upgrade", (request, socket) => { seen.push(request.url); socket.destroy(); });
    await once(site, "listening");
    t.after(() => site.close());
    const chromium = process.env.CAGE_TEST_PHONE_PLAYWRIGHT ? (await import(pathToFileURL(process.env.CAGE_TEST_PHONE_PLAYWRIGHT))).chromium : null;
    const browser = new PhoneBrowser({
      directory, binary: process.env.CAGE_TEST_CHROMIUM, language: "ja-JP",
      connect: async (name, port) => {
        if (name !== "phone.example" || port !== site.address().port) throw new Error("fixture denies destination");
        const socket = net.connect({ host: "127.0.0.1", port });
        await once(socket, "connect");
        return socket;
      },
      ...(chromium ? { pipeFactory: (binary, args) => playwrightPhonePipe(chromium, binary, args) } : {}),
    });
    t.after(() => browser.dispose());
    let id = randomUUID();
    const url = `http://phone.example:${site.address().port}`;
    const open = (profile, path = "/") => browser.run("open", { id, profile, url: url + path, width: 390, height: 750 });
    const action = (type, extra) => browser.run("action", { id, type, ...extra });
    async function read(check = () => true) {
      let latest;
      for (let i = 0; i < 25; i++) {
        try { latest = await browser.run("frame", { id }); }
        catch (error) { if (error.status !== 409) throw error; await delay(50); continue; }
        if (latest.title.startsWith("[") && check(JSON.parse(latest.title), latest)) return latest;
        await delay(50);
      }
      throw new Error("expected frame missing: " + latest?.title);
    }
    async function settled(frame) {
      // Chromium can paint the page after the DOM already has its new title.
      for (let i = 0; i < 20; i++) {
        const next = await browser.run("frame", { id, after: frame.frame_tag });
        if (next.unchanged) {
          assert.deepEqual(next, { id, frame_tag: frame.frame_tag, unchanged: true });
          return frame;
        }
        frame = next;
        await delay(50);
      }
      throw new Error("static page never produced an unchanged frame");
    }
    assert.deepEqual(await open("a".repeat(64), "/remember"), { id });
    let page = await read(([w, h, , cookie, , touch]) => w === 390 && h === 750 && cookie === "state=known" && touch >= 1);
    assert.equal(JSON.parse(page.title)[6], "ja-JP");
    assert.equal(JSON.parse(page.title)[8], 2);
    assert.deepEqual(JSON.parse(page.title).slice(9), [false, "function get webdriver() { [native code] }"]);
    assert.match(acceptLanguage, /^ja-JP/i);
    if (process.env.TZ) assert.equal(JSON.parse(page.title)[7], Intl.DateTimeFormat().resolvedOptions().timeZone);
    assert.ok(Buffer.from(page.image, "base64").subarray(0, 2).equals(Buffer.from([255, 216])));
    assert.match(page.frame_tag, /^[a-f0-9]{32}$/);
    page = await settled(page);
    const initialTag = page.frame_tag;
    page = await action("tap", { x: 60, y: 25, document: page.document, return_frame: true });
    assert.ok(page.focus && page.image);
    assert.notEqual(page.frame_tag, initialTag);
    const old = { document: page.document, focus: page.focus };
    assert.deepEqual(await action("text", { ...old, text: "你好 Cage" }), { id });
    page = await read(([, , value]) => value === "你好 Cage");
    const capture = browser.frame;
    browser.frame = async () => { throw new Error("frame changed after text input"); };
    try {
      assert.deepEqual(await action("text", { ...old, text: "!", return_frame: true }), { id });
    } finally { browser.frame = capture; }
    await read(([, , value]) => value === "你好 Cage!");
    await action("resize", { width: 390, height: 400 });
    page = await read(([, h]) => h === 400);
    assert.equal(page.focus, old.focus);
    assert.notEqual(page.document, old.document);
    await action("text", { focus: old.focus, document: page.document, text: "✓" });
    page = await read(([, , value]) => value === "你好 Cage!✓");
    await action("tap", { x: 60, y: 75, document: page.document });
    await read((_, frame) => frame.password && frame.focus !== old.focus);
    await assert.rejects(action("text", { document: page.document, focus: old.focus, text: "stale" }), /输入框已改变/);
    await action("scroll", { x: 100, y: 280, dx: 0, dy: 200, document: page.document });
    await read(([, , , , y]) => y > 0);
    await action("resize", { width: 750, height: 390 });
    await read(([w, h]) => w === 750 && h === 390);
    await assert.rejects(action("tap", { x: 30, y: 30, document: old.document }), /页面已切换/);
    await action("navigate", { url: url + "/next" });
    page = await read();
    await action("tap", { x: 60, y: 25, document: page.document });
    page = await read((_, frame) => Boolean(frame.focus));
    assert.notEqual(page.focus, old.focus);
    await assert.rejects(action("text", { document: page.document, focus: old.focus, text: "stale" }), /输入框已改变/);
    await action("resize", { width: 390, height: 750 });
    await action("navigate", { url: url + "/wide" });
    page = await read(([w, scale]) => w >= 980 && scale < 1);
    const scale = JSON.parse(page.title)[1];
    await action("tap", { x: 550 * scale, y: 65 * scale, document: page.document });
    await read((_, frame) => Boolean(frame.focus));
    await action("navigate", { url: url + "/private" }).catch(() => {});
    await delay(250);
    assert.equal(seen.includes("/forbidden"), false);
    await action("navigate", { url: url + "/private-ws" });
    await read(([, , status]) => status === "ws-attempted");
    await delay(200);
    assert.equal(seen.includes("/forbidden-ws"), false, "a WebSocket bypassed the public-only proxy");
    await browser.run("close", { id });
    browser.language = "fr-FR";
    id = randomUUID(); await open("a".repeat(64), "/restored");
    page = await read(([, , , cookie]) => cookie === "state=known");
    assert.equal(JSON.parse(page.title)[6], "fr-FR");
    assert.match(acceptLanguage, /^fr-FR/i);
    await browser.run("close", { id });
    id = randomUUID(); await open("b".repeat(64));
    await read(([, , , cookie]) => cookie === "");
    // A secure fixture varies the initial HTML by Chromium's request headers,
    // just as mobile sites do. Fulfill the request before it leaves the test.
    const s = browser.session;
    const originalOnEvent = s.pipe.onEvent;
    const { product } = await s.pipe.send("Browser.getVersion");
    const major = product.split("/")[1].split(".")[0];
    s.pipe.onEvent = event => {
      originalOnEvent(event);
      if (event.method !== "Fetch.requestPaused") return;
      const { request, requestId } = event.params;
      const headers = Object.fromEntries(Object.entries(request.headers).map(([k, v]) => [k.toLowerCase(), v]));
      const mobile = headers["sec-ch-ua-mobile"] === "?1" && headers["user-agent"].includes(" Mobile ") &&
        headers["sec-ch-ua"]?.includes(`"Chromium";v="${major}"`);
      const html = `<meta name="viewport" content="width=device-width,initial-scale=1">
        <style>body{margin:0} form{width:${mobile ? "95%" : "768px"}}</style>
        <form><input></form><script>document.title=JSON.stringify([innerWidth, document.forms[0].offsetWidth,
          navigator.userAgentData.mobile,navigator.language]);</script>`;
      void s.send("Fetch.fulfillRequest", { requestId, responseCode: 200, responseHeaders: [{ name: "Content-Type", value: "text/html" }],
        body: Buffer.from(html).toString("base64") });
    };
    await s.send("Fetch.enable", { patterns: [{ urlPattern: "https://phone.example/*" }] });
    await action("navigate", { url: "https://phone.example/search" });
    page = await read(([w, form, mobile]) => w === 390 && form <= w && mobile === true);
    assert.equal(JSON.parse(page.title)[3], "fr-FR");
    page = await settled(page);
    // A change to the title alone must invalidate the conditional frame.
    await s.send("Runtime.evaluate", { expression: 'document.title = "new title"' });
    const renamed = await browser.run("frame", { id, after: page.frame_tag });
    assert.equal(renamed.title, "new title");
    assert.equal(renamed.image, page.image);
    assert.notEqual(renamed.frame_tag, page.frame_tag);
    await browser.run("close", { id });
  });
