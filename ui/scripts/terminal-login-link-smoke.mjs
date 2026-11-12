// Click both link formats emitted by command-line tools in the production xterm.
import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const url = "https://claude.ai/oauth/authorize?state=" + "fixture".repeat(22);
for (const format of ["printed", "OSC 8"]) {
  const opened = [];
  const { browser, page, errors } = await createWorkbench({ mockWebSockets: false });
  try {
    await page.route("**/api/terminals/*/login-link", route => {
      opened.push(route.request().postDataJSON());
      return route.fulfill({ json: { id: "login-tab" } });
    });
    await page.routeWebSocket("**/api/**", socket => {
      if (!socket.url().includes("/terminals/")) return socket.close();
      socket.send(Buffer.from(format === "printed" ? url : `\x1b]8;;${url}\x07Open Claude\x1b]8;;\x07`));
    });
    await page.reload();
    await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
    await page.getByRole("button", { name: "新建终端", exact: true }).first().click();
    await page.getByRole("menuitem", { name: "普通终端", exact: true }).click();
    const row = page.locator(".xterm-rows > div").first();
    await row.filter({ hasText: format === "printed" ? "claude.ai" : "Open Claude" }).waitFor();
    const rect = await row.boundingBox();
    await page.mouse.move(rect.x + 20, rect.y + rect.height / 2);
    await page.waitForFunction(() => !!document.querySelector(".xterm-cursor-pointer"));
    assert.equal(opened.length, 0);
    await page.mouse.down();
    await page.mouse.up();
    await page.getByRole("tab", { name: /账号授权/ }).waitFor();
    assert.deepEqual(opened, [{ url }]);
    assert.equal(page.context().pages().length, 1);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
}
console.log("Printed and OSC 8 authorization links open the guest login tab on click.");
