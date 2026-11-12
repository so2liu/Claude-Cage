import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";
const evidence = process.env.CAGE_INITIALIZATION_EVIDENCE;
if (evidence) await fs.mkdir(evidence, { recursive: true });

for (const mode of ["project-approval", "legacy", "guest-login", "filesystem-permission", "network", "restart"]) {
  let connected = false, checks = 0, starts = 0, stopped = false, stopFinished = false;
  const workspace = "/preview/shared-workspace";
  const fixture = await createWorkbench({ initialization: { required: true, tracked: true, started: true, prepared: true }, beforeNavigate: async (page, state) => {
    Object.assign(state.account, { guest_system: "macos" });
    state.settings.workspace = workspace;
    Object.assign(state.status, { guest_system: "macos", ready: true, vm_running: true, workspace, environment_directory: "/preview/other-directory", safety: { gate: { mode: "closed" }, error: "工作区访问尚未允许", report: { checks: [{ id: "filesystem_boundary", title: "共享文件与宿主私有目录", state: "unknown", evidence: "客机文件夹授权未完成", remedy: "允许访问工作区后继续" }] } }, network_error: "内置代理未就绪；外网保持关闭", job: { id: "failed-check", kind: "safety", running: false, message: "检查联网条件失败", error: "请在 Cage 显示的客机授权窗口中允许此项目，然后验证授权。", ...(mode === "legacy" ? {} : { recovery: mode === "restart" ? "environment-restart" : mode, workspace }) }, project_errors: [] });
    if (mode === "project-approval") state.status.project_errors = [{ path: workspace, recovery: "project-approval", message: "请允许工作区访问" }];
    await page.route("**/api/projects/open", route => {
      assert.equal(route.request().postDataJSON().workspace, workspace);
      assert.equal(route.request().postDataJSON().add, false);
      return route.fulfill({ json: connected ? { workspace } : { workspace, approval: "scoped-grant" } });
    });
    await page.route("**/api/macos/project-approval", route => route.fulfill({ json: { error: "waiting" } }));
    await page.route("**/api/safety/check", route => {
      checks++;
      state.status.job = { id: "checking", kind: "safety", running: false, message: "已完成" };
      state.status.safety = { gate: { mode: "open", until: new Date(Date.now() + 600000).toISOString() } };
      state.status.project_errors = [];
      return route.fulfill({ json: state.status.job });
    });
    await page.route("**/api/stop", route => {
      stopped = true; state.status.job = { id: "stop-once", kind: "stop", running: true, message: "正在停止" };
      setTimeout(() => { stopFinished = true; state.status.job.running = false; state.status.vm_running = false; }, 300);
      return route.fulfill({ json: state.status.job });
    });
    await page.route("**/api/start", route => {
      assert.ok(stopped && stopFinished, "start ran before stop completed"); starts++; state.status.vm_running = true;
      state.status.job = { id: "restarted", kind: "start", running: false, message: "已完成" };
      return route.fulfill({ json: state.status.job });
    });
  }});
  const { page, state, browser, errors } = fixture;
  try {
    await page.getByRole("heading", { name: "环境已启动，等待联网检查", exact: true }).waitFor();
    await page.getByRole("button", { name: "查看操作详情", exact: true }).click();
    const details = page.getByRole("dialog", { name: "初始化操作详情" });
    await details.getByText("请在 Cage 显示的客机授权窗口中允许此项目，然后验证授权。", { exact: true }).waitFor();
    await details.getByText("客机文件夹授权未完成", { exact: true }).waitFor();
    await details.getByRole("button", { name: "关闭面板" }).click();
    if (mode === "guest-login") await page.getByRole("button", { name: "登录客机账户", exact: true }).waitFor();
    if (mode === "filesystem-permission") await page.getByRole("button", { name: "打开客机授权设置", exact: true }).waitFor();
    if (["project-approval", "legacy"].includes(mode)) {
      const action = mode === "project-approval" ? "打开工作区授权" : "连接工作区并继续检查";
      if (evidence) await page.screenshot({ path: `${evidence}/${mode}.png`, fullPage: true });
      await page.reload();
      await page.getByRole("button", { name: action, exact: true }).click();
      const approval = page.getByRole("dialog", { name: "允许客机访问所选项目" });
      await approval.getByRole("button", { name: "取消", exact: true }).click();
      assert.equal(checks, 0, "cancel must not start network verification");
      await page.getByRole("button", { name: action, exact: true }).click();
      await approval.getByRole("button", { name: "验证授权", exact: true }).waitFor();
      connected = true;
      await approval.getByRole("button", { name: "验证授权", exact: true }).click();
      await page.getByRole("button", { name: "进入工作台", exact: true }).waitFor();
      assert.equal(checks, 1);
    } else if (mode === "restart") {
      await page.getByRole("button", { name: "保存后重启并检查", exact: true }).click();
      await page.getByRole("dialog", { name: "重启虚拟 Mac 并检查？" }).getByRole("button", { name: "重启并检查", exact: true }).click();
      await page.waitForFunction(() => ![...document.querySelectorAll('[role="dialog"]')].length);
      await page.getByRole("button", { name: "查看操作详情" }).waitFor();
      await page.waitForFunction(() => ![...document.querySelectorAll('button')].some(b => b.textContent === '停止安装'));
      assert.equal(starts, 1);
    } else {
      await page.getByRole("button", { name: "运行网络与隔离检查", exact: true }).click();
      await page.getByRole("button", { name: "进入工作台", exact: true }).waitFor();
    }
    await page.setViewportSize({ width: 390, height: 700 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    assert.deepEqual(errors, []);
    console.log(`PASS macOS initialization: ${mode}, precise details and recovery without reinstalling`);
  } finally { await browser.close(); }
}
