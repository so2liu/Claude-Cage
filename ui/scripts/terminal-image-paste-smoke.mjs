import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a/e8AAAAASUVORK5CYII=";
for (const platform of ["MacIntel", "Win32", "Linux x86_64"]) {
  const inputs = [], images = [];
  let clipboard = { png }, reads = 0, release, fail = false;
  const { page, browser, state, errors } = await createWorkbench({
    mockWebSockets: false,
    beforeNavigate: page => page.addInitScript(platform => {
      Object.defineProperty(navigator, "platform", { value: platform });
    }, platform),
    nativeRequest: request => {
      if (request.action === "clipboardReadContent") { reads++; return clipboard; }
    },
  });
  const wait = async predicate => {
    const deadline = Date.now() + 5000;
    while (!predicate() && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10));
    assert.ok(predicate(), "terminal paste did not finish");
  };
  try {
    await page.route("**/api/terminals", route => {
      if (route.request().method() !== "POST") return route.fallback();
      const terminal = { id: "claude-image", kind: "claude", title: "Claude 1", workspace: state.settings.workspace };
      state.terminals = [terminal];
      return route.fulfill({ json: terminal });
    });
    await page.route("**/api/input/paste/start", route => route.fulfill({ json: { available: true, context: "claude-image" } }));
    await page.route("**/api/input/image", async route => {
      images.push(route.request().postDataJSON());
      await new Promise(resolve => { release = resolve; });
      await route.fulfill(fail ? { status: 502, contentType: "text/plain", body: "测试：图片传递失败" }
        : { json: { path: "/guest/attachments/image-test.png" } });
    });
    await page.routeWebSocket("**/api/**", socket => {
      if (!socket.url().includes("/terminals/")) return socket.close();
      socket.onMessage(message => { const value = JSON.parse(message); if (value.type === "input") inputs.push(value.data); });
      socket.send(Buffer.from("\x1b[?2004hClaude image paste\r\n> "));
    });
    await page.reload();
    await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
    await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
    await page.getByRole("menuitem", { name: /Claude/ }).first().click();
    await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent.includes("Claude"));
    await page.locator(".xterm-helper-textarea").focus();
    const expected = "\x1b[200~'/guest/attachments/image-test.png' \x1b[201~";
    // Claude's Ctrl+V reads the host image; Enter waits for its guest path.
    await page.keyboard.press("Control+v");
    await wait(() => images.length === 1);
    await page.keyboard.press("Enter");
    assert.deepEqual(inputs, []);
    release();
    await wait(() => inputs.length === 2);
    assert.deepEqual(inputs, [expected, "\r"]);
    assert.equal(images[0].png, png);
    assert.equal(images[0].target, "terminal");
    assert.equal(images[0].id, "claude-image");

    await page.keyboard.press(platform === "MacIntel" ? "Meta+v" : "Control+Shift+V");
    await wait(() => images.length === 2);
    release();
    await wait(() => inputs.length === 3);
    assert.equal(inputs.at(-1), expected);
    assert.equal(reads, 2);

    // The desktop Edit > Paste action shares the same image path.
    assert.equal(await page.evaluate(() => window.cageEditAction("paste")), true);
    await wait(() => images.length === 3);
    release();
    await wait(() => inputs.length === 4);
    assert.equal(inputs.at(-1), expected);

    fail = true;
    await page.keyboard.press("Control+v");
    await wait(() => images.length === 4);
    await page.keyboard.press("Enter");
    release();
    await page.getByText("测试：图片传递失败", { exact: true }).waitFor();
    assert.equal(inputs.length, 4, "a failed image must cancel its queued Enter");
    fail = false;
    clipboard = { text: "paste after failure" };
    await page.keyboard.press("Control+v");
    await wait(() => inputs.length === 5);
    assert.equal(inputs.at(-1), "\x1b[200~paste after failure\x1b[201~");
    assert.deepEqual(errors, []);
    console.log(`${platform}: native Ctrl+V, primary paste, Edit > Paste, image/Enter ordering and failure recovery passed`);
  } catch (error) {
    console.error({ platform, reads, inputs, images: images.length, terminals: state.terminals,
      recentRequests: state.requests.slice(-8), screen: await page.locator(".terminal-view").allTextContents(), errors });
    throw error;
  } finally { await browser.close(); }
}
