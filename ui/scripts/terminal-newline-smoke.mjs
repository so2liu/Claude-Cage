// Exercises the production xterm handler and final WebSocket input messages.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

for (const platform of ["MacIntel", "Win32", "Linux x86_64"]) {
  const inputs = [];
  let releaseClipboard;
  const { browser, page, errors } = await createWorkbench({
    mockWebSockets: false,
    nativeRequest: request => request.action === "clipboardReadContent"
      ? new Promise(resolve => { releaseClipboard = resolve; }) : null,
    beforeNavigate: page => page.addInitScript(platform => {
      Object.defineProperty(navigator, "platform", { value: platform });
    }, platform),
  });
  let socket;
  try {
    await page.routeWebSocket("**/api/**", remote => {
      if (!remote.url().includes("/terminals/")) return remote.close();
      socket = remote;
      remote.onMessage(message => {
        const { type, data } = JSON.parse(message);
        if (type === "input") inputs.push(data);
      });
      remote.send(Buffer.from("$ "));
    });
    await page.reload();
    await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
    await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
    await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
    await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent.includes("$"));
    await page.locator(".xterm-helper-textarea").focus();
    async function press(key, expected) {
      const start = inputs.length;
      await page.keyboard.press(key);
      await assertInputs(start, expected);
    }
    async function assertInputs(start, expected) {
      const deadline = Date.now() + 3000;
      while (inputs.length < start + expected.length && Date.now() < deadline)
        await new Promise(resolve => setTimeout(resolve, 10));
      assert.deepEqual(inputs.slice(start), expected);
    }
    await press("Shift+Enter", ["\x1b[13;2u"]);
    await press("Enter", ["\r"]);
    await press("Control+c", ["\x03"]);
    socket.send(Buffer.from("\x1b[?2004hMODE_READY"));
    await page.locator(".xterm-rows").getByText(/MODE_READY/).waitFor();
    await press("Shift+Enter", ["\x1b[200~\n\x1b[201~"]);
    await press("Shift+Enter", ["\x1b[200~\n\x1b[201~"]);
    await press("Enter", ["\r"]);
    const beforePaste = inputs.length;
    await page.keyboard.press(platform === "MacIntel" ? "Meta+v" : "Control+Shift+V");
    const deadline = Date.now() + 3000;
    while (!releaseClipboard && Date.now() < deadline)
      await new Promise(resolve => setTimeout(resolve, 10));
    assert.ok(releaseClipboard, "the paste gesture must start its clipboard read");
    await page.keyboard.press("Shift+Enter");
    await page.keyboard.press("Enter");
    assert.equal(inputs.length, beforePaste, "newline and submission must wait for pending paste");
    releaseClipboard({ text: "CAGE_PASTE" });
    await assertInputs(beforePaste, ["\x1b[200~CAGE_PASTE\x1b[201~", "\x1b[200~\n\x1b[201~", "\r"]);
    socket.send(Buffer.from("\x1b[?2004lMODE_OFF"));
    await page.locator(".xterm-rows").getByText(/MODE_OFF/).waitFor();
    await press("Shift+Enter", ["\x1b[13;2u"]);
    assert.deepEqual(errors, []);
    console.log(`${platform}: Shift+Enter, repeated newlines, paste ordering, Enter and Ctrl+C passed`);
  } finally { await browser.close(); }
}
