import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";
const evidence = process.env.CAGE_INITIALIZATION_EVIDENCE;
if (evidence) await fs.mkdir(evidence, { recursive: true });
for (const scenario of ["permission-read", "start-failure", "open-failure"]) {
  const permissions = { screen: true, input: false, projects: false, managed_credential: false };
  let failRead = true, failAction = true, starts = 0, finishes = 0, permissionReads = 0;
  const f = await createWorkbench({ waitForReady: false, beforeNavigate: async (page, state) => {
    Object.assign(state.account, { initial_mac_setup: true, guest_system: "macos", macos_credentials_ready: true });
    Object.assign(state.status, { ready: scenario !== "start-failure", vm_running: scenario !== "start-failure" });
    await page.route("**/api/start", route => {
      starts++;
      if (failAction) return route.fulfill({ status: 409, contentType: "text/plain", body: "启动失败，已有磁盘已保留" });
      Object.assign(state.status, { ready: true, vm_running: true });
      return route.fulfill({ json: { id: "resumed", running: false } });
    });
    await page.route("**/api/macos/first-setup", route => {
      const { action } = route.request().postDataJSON();
      if (action === "permissions") {
        permissionReads++;
        if (scenario === "permission-read" && failRead && permissionReads > 1) return route.fulfill({ status: 409, contentType: "text/plain", body: "客机连接暂时中断" });
        return route.fulfill({ json: permissions });
      }
      if (action === "finish") { finishes++; state.account.initial_mac_setup = false; }
      if (action === "input" && scenario === "open-failure" && failAction) return route.fulfill({ status: 409, contentType: "text/plain", body: "授权页面暂时无法打开，请重试" });
      return route.fulfill({ json: {} });
    });
  }});
  const { page, browser, errors, state } = f;
  try {
    await page.getByRole("heading", { name: "完成虚拟 Mac 的系统授权" }).waitFor();
    if (scenario === "start-failure") {
      await page.getByRole("alert").filter({ hasText: "启动失败" }).waitFor();
      assert.equal(starts, 1);
      failAction = false;
      await page.getByRole("button", { name: "继续启动并连接", exact: true }).click();
      await page.getByRole("button", { name: "打开辅助功能", exact: true }).waitFor();
      assert.equal(starts, 2);
    } else if (scenario === "permission-read") {
      await page.getByRole("list", { name: "系统授权进度" }).getByText("已验证", { exact: true }).waitFor();
      await page.getByRole("alert").filter({ hasText: "客机连接暂时中断" }).waitFor();
      assert.equal(await page.getByRole("list", { name: "系统授权进度" }).getByText("已验证", { exact: true }).count(), 1, "read failure erased measured permissions");
      permissions.input = true; permissions.projects = true;
      assert.equal(finishes, 0, "read failure advanced setup");
      if (evidence) await page.screenshot({ path: `${evidence}/permission-read-failure.png`, fullPage: true });
      failRead = false;
      await page.getByRole("button", { name: "重新检查状态", exact: true }).click();
      await page.getByRole("heading", { name: "完成虚拟 Mac 的系统授权" }).waitFor({ state: "hidden" });
      assert.equal(finishes, 1);
    } else {
      await page.getByRole("button", { name: "打开辅助功能", exact: true }).click();
      await page.getByRole("alert").filter({ hasText: "授权页面暂时无法打开" }).waitFor();
      failAction = false;
      await page.getByRole("button", { name: "打开辅助功能", exact: true }).click();
      await page.getByRole("alert").waitFor({ state: "hidden" });
      permissions.input = true;
      await page.getByRole("list", { name: "系统授权进度" }).getByText("已验证", { exact: true }).nth(1).waitFor();
      await page.getByRole("region", { name: "当前设置步骤" }).getByText(/打开 macFUSE 和 macFUSE \(local\) 两个开关/).waitFor();
      await page.reload();
      await page.getByRole("list", { name: "系统授权进度" }).getByText("已验证", { exact: true }).nth(1).waitFor();
      assert.equal(starts, 0, "refresh restarted installed VM");
      if (evidence) await page.screenshot({ path: `${evidence}/permissions-resumed.png`, fullPage: true });
    }
    await page.setViewportSize({ width: 390, height: 700 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    assert.deepEqual(errors, []);
    assert.ok(state.status.vm_running);
    console.log(`PASS macOS setup ${scenario}: actionable retry, measured state retention and resume`);
  } finally { await browser.close(); }
}
