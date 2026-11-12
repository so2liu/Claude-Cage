// Production workbench and RFB input with an isolated recording native bridge.
// The user's OS clipboard, profile and Claude conversations are never accessed.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
import { startDisplay } from "./rfb-wire-fixture.mjs";

const display = await startDisplay(process.env.CAGE_UI_URL || "http://127.0.0.1:5198", { sharedClipboard: true });
let revision = 1, clipboard = { text: "initial host text" };
const writes = [];
const { browser, page, state, errors } = await createWorkbench({
  mockWebSockets: false, url: display.url,
  nativeRequest: request => {
    if (request.action === "clipboardWrite") { clipboard = { text: request.text }; revision++; return true; }
    if (request.action === "clipboardRevision") return revision;
    if (request.action === "clipboardSyncRead") return { ...clipboard, revision };
    if (request.action === "clipboardSyncWrite") {
      if (request.revision !== revision) return { revision, superseded: true };
      clipboard = Object.fromEntries(Object.entries(request).filter(([key]) => ["text","png","files"].includes(key)));
      writes.push(clipboard); return { revision: ++revision };
    }
    return true;
  },
});
const waitFor = async predicate => { for (let attempt = 0; attempt < 100; attempt++) { if (predicate()) return; await page.waitForTimeout(30); } assert.ok(predicate(),"clipboard state did not arrive"); };
try {
  await page.route("**/api/claude-desktop", route => route.fulfill({ json: { installed: true, running: true, failed: false, managed: true, version: "fixture" } }));
  state.status.ready = state.status.vm_running = true;
  await page.reload();
  await page.getByRole("button",{ name:"Claude Desktop", exact:true }).click();
  await page.getByText("正在连接画面",{ exact:true }).waitFor({ state:"hidden" });
  await page.waitForFunction(() => window.cageSharedClipboardActive === true);
  assert.equal(display.stats.clipboardUpdates.at(-1).text,"initial host text");

  for (const content of [{ text:"Claude 消息🙂\n保留换行" },{ png:"fixture-png" },{ files:"10000000-2222-4333-8444-123456789abc" }]) {
    display.sharedClipboard(content);
    await waitFor(() => JSON.stringify(writes.at(-1)) === JSON.stringify(content));
    const count = display.stats.clipboardUpdates.length;
    await page.waitForTimeout(350);
    assert.equal(display.stats.clipboardUpdates.length,count,"native writes must not echo");
  }

  clipboard = { text:"new host copy" }; revision++;
  await page.locator(".remote-canvas canvas").click({ position:{ x:40,y:40 } });
  display.stats.keys.length = 0;
  await page.keyboard.press("Meta+v");
  await waitFor(() => display.stats.keys.some(key => key.down && key.symbol === 118));
  assert.equal(display.stats.clipboardUpdates.at(-1).text,"new host copy");
  assert.equal(state.requests.some(request => request.path === "/input/image"),false,"synchronized paste uses the actual guest clipboard");

  await page.getByRole("button",{ name:"工作台", exact:true }).click();
  await page.getByRole("region",{ name:"Claude Desktop", exact:true }).waitFor({ state:"hidden" });
  display.sharedClipboard({ text:"guest background copy" });
  await waitFor(() => clipboard.text === "guest background copy");
  assert.equal(display.stats.clipboardConnections,1,"hiding a canvas retains the shared clipboard");

  await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
  await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent.includes("Terminal"));
  await page.locator(".xterm-helper-textarea").focus();
  const terminalCopy = "Claude 终端复制 → 共享剪贴板";
  display.terminalOutput(`\x1b]52;c;${Buffer.from(terminalCopy).toString("base64")}\x07`);
  await waitFor(() => clipboard.text === terminalCopy && display.stats.clipboardUpdates.at(-1).text === terminalCopy);

  const connections = display.stats.clipboardConnections;
  display.disconnectClipboard();
  clipboard = { text:"copy during disconnection" }; revision++;
  await waitFor(() => display.stats.clipboardConnections > connections && display.stats.clipboardUpdates.at(-1).text === "copy during disconnection");
  await page.waitForFunction(() => window.cageSharedClipboardActive === true);
  assert.deepEqual(errors,[]);
  console.log("PASS: Desktop text/image/files copy, terminal OSC 52 to shared guest clipboard, no echo, native paste, background sharing and reconnect.");
} finally { await browser.close(); await display.close(); }
