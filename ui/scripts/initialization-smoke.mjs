import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { createWorkbench } from "./workbench-fixture.mjs";

const evidence = process.env.CAGE_INITIALIZATION_EVIDENCE;
if (evidence) await fs.mkdir(evidence, { recursive: true });

{
  const ip = "203.0.113.10";
  let bindings = 0, stops = 0, starts = 0;
  let rejectBinding = true, disconnectOnStop = false, disconnected = false;
  const savedMessage = "已保存固定出口。请重启 macOS 客机，登录后重新检测。";
  const f = await createWorkbench({
    initialization: { required: true, tracked: true, started: true },
    beforeNavigate: async (page, state) => {
      state.account.guest_system = "macos";
      Object.assign(state.status, { guest_system: "macos", ready: true, vm_running: true, safety: { gate: { mode: "closed" } } });
      await page.route("**/api/status", route => disconnected
        ? route.fulfill({ status: 503, contentType: "text/plain; charset=utf-8", body: "测试：停止后连接中断" })
        : route.fulfill({ json: { ...state.status, workspace: state.settings.workspace } }));
      await page.route("**/api/macos/exit-policy", route => {
        assert.equal(route.request().method(), "POST");
        assert.deepEqual(route.request().postDataJSON(), { ip });
        bindings++;
        if (rejectBinding) return route.fulfill({ status: 409, contentType: "text/plain; charset=utf-8", body: "出口采样已过期，请重新运行联网检查" });
        state.status.job = { id: `bind-${bindings}`, kind: "settings", running: true, message: "正在保存固定出口" };
        return route.fulfill({ json: state.status.job });
      });
      await page.route("**/api/stop", route => {
        stops++;
        state.status.job = { id: `stop-${stops}`, kind: "stop", running: true, message: "正在停止客机" };
        if (disconnectOnStop) disconnected = true;
        return route.fulfill({ json: state.status.job });
      });
      await page.route("**/api/start", route => {
        assert.equal(state.status.vm_running, false, "start must wait for the accepted stop to finish");
        assert.equal(state.status.job.kind, "stop");
        assert.equal(state.status.job.running, false);
        assert.ok(!state.status.job.error);
        starts++;
        Object.assign(state.status, { ready: true, vm_running: true, job: { id: `start-${starts}`, kind: "start", running: true, message: "正在启动并检查客机" } });
        // The backend accepted the start, but its response did not reach the UI.
        return route.fulfill({ status: 502, contentType: "text/plain; charset=utf-8", body: "启动请求的响应传输中断" });
      });
    },
  });
  const { page, state } = f;
  page.setDefaultTimeout(5000);
  const bind = page.getByRole("button", { name: `绑定检测到的固定出口 ${ip}`, exact: true });
  const restart = () => page.getByRole("button", { name: /^(重启客机|启动客机)并重新检测$/ });
  const confirm = async (name) => {
    const dialog = page.getByRole("dialog");
    await dialog.getByRole("button", { name, exact: true }).click();
  };
  try {
    await page.getByRole("heading", { name: "环境已启动，等待联网检查", exact: true }).waitFor();
    assert.equal(await bind.count(), 0, "no candidate must offer no binding");
    state.status.safety.macos_exit_candidate = { ip };
    await bind.waitFor(); // Red against the original initialization screen: the entry is missing.
    if (evidence) await page.screenshot({ path: `${evidence}/macos-exit-candidate.png`, fullPage: true });
    await bind.click();
    await page.getByRole("dialog").getByText(/保存后请重启客机并重新检测/).waitFor();
    await confirm("取消");
    assert.equal(bindings, 0, "cancel must not bind an exit");
    await bind.click(); await confirm("绑定固定出口");
    await page.getByRole("alert").filter({ hasText: "出口采样已过期" }).first().waitFor();
    assert.equal(bindings, 1);
    assert.equal(stops + starts, 0);

    rejectBinding = false;
    await bind.click(); await confirm("绑定固定出口");
    await page.getByText("正在保存固定出口", { exact: true }).waitFor();
    assert.ok(await bind.isDisabled());
    assert.equal(await restart().count(), 0, "an accepted settings job is not a saved exit");
    state.status.job = { ...state.status.job, running: false, error: "固定出口未保存：定位服务暂不可用" };
    await page.getByRole("alert").filter({ hasText: "固定出口未保存" }).first().waitFor();
    assert.equal(stops + starts, 0, "save failure must not restart");

    await bind.evaluate(button => { button.click(); button.click(); });
    await confirm("绑定固定出口");
    await page.getByText("正在保存固定出口", { exact: true }).waitFor();
    await bind.evaluate(button => { button.click(); button.click(); });
    assert.equal(bindings, 3, "duplicate clicks must not submit another binding");
    state.status.job = { ...state.status.job, running: false, message: "已完成" };
    delete state.status.safety.macos_exit_candidate;
    state.status.safety.error = savedMessage;
    await restart().waitFor();
    assert.equal(await page.getByRole("button", { name: "进入工作台", exact: true }).count(), 0);
    await page.setViewportSize({ width: 600, height: 650 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    if (evidence) await page.screenshot({ path: `${evidence}/macos-exit-saved-compact.png`, fullPage: true });
    await page.setViewportSize({ width: 1280, height: 850 });
    await restart().click(); await confirm("取消");
    assert.equal(stops, 0, "cancel must preserve the running guest");
    await restart().click(); await confirm("重启并检查");
    await page.getByText("正在停止客机", { exact: true }).waitFor();
    assert.equal(starts, 0, "a running stop job must not start the guest");
    state.status.job = { ...state.status.job, running: false, error: "停止客机失败" };
    await page.getByRole("alert").filter({ hasText: "停止客机失败" }).first().waitFor();
    assert.equal(starts, 0, "stop failure must not start the guest");

    disconnectOnStop = true;
    await restart().click(); await confirm("重启并检查");
    await page.getByRole("alert").filter({ hasText: "停止后连接中断" }).first().waitFor();
    assert.equal(starts, 0, "lost status must not authorize a start");
    disconnected = false;
    disconnectOnStop = false;
    state.status.job = { id: "stop-2", kind: "stop", running: false, error: "停止请求中断，客机仍在运行" };
    await page.waitForFunction(() => [...document.querySelectorAll("button")].some(button => button.textContent === "重启客机并重新检测" && !button.disabled));
    await restart().click(); await confirm("重启并检查");
    await page.getByText("正在停止客机", { exact: true }).waitFor();
    assert.equal(starts, 0);
    Object.assign(state.status, { ready: false, vm_running: false, job: { id: "stop-3", kind: "stop", running: false } });
    await page.getByText("正在启动并检查客机", { exact: true }).waitFor();
    assert.equal(starts, 1);
    assert.equal(stops, 3);
    await page.getByRole("alert").filter({ hasText: "响应传输中断" }).first().waitFor();
    assert.equal(await page.getByRole("button", { name: "进入工作台", exact: true }).count(), 0);
    state.status.job = { ...state.status.job, running: false };
    state.status.safety = { gate: { mode: "open", until: new Date(Date.now() + 3600000).toISOString() } };
    await page.getByRole("button", { name: "进入工作台", exact: true }).waitFor();
    assert.equal(await restart().count(), 0, "a lost start response must not hide a later verified open gate");
    await page.reload();
    await page.getByRole("button", { name: "进入工作台", exact: true }).click();
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    assert.deepEqual(f.errors, []);
    console.log("PASS macOS exit: explicit binding, cancellation, save failures, stop-before-start, disconnect recovery, lost start response and network completion gate");
  } finally { await f.browser.close(); }
}

for (const [platform, system] of [["darwin", "macos"], ["windows", "windows-11"], ["linux", "ubuntu-22.04"]]) {
  let starts = 0, rejectComplete = true;
  let directory = "/preview/cage-state";
  const initialDirectory = platform === "windows" ? "/preview/Cage/CageData" : directory;
  const fixture = await createWorkbench({
    initialization: { required: true, tracked: true, started: false },
    clientContext: { mode: "local", connection_id: "local", client_platform: platform, server_platform: platform },
    beforeNavigate: async (page, state) => {
      // Audio is outside this initialization flow; avoid unrelated in-flight audio requests during reload.
      await page.addInitScript(() => localStorage.setItem("cage.audio", JSON.stringify({ muted: true })));
      state.status.local_vm_storage = true;
      await page.route("**/api/environment/storage", route => {
        if (route.request().method() === "POST") {
          assert.equal(route.request().postDataJSON().previous, directory);
          directory = route.request().postDataJSON().directory;
          state.status.job = { id: "storage-default", kind: "storage", running: false };
          return route.fulfill({ json: state.status.job });
        }
        return route.fulfill({ json: { directory, has_environment: false, suggested_directory: directory === "/preview/cage-state" ? initialDirectory : undefined } });
      });
      await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, available: true, system, linux_systems: ["ubuntu-22.04"] } }));
      await page.route("**/api/environment/guest-systems/clone", route => route.fulfill({ json: { key: "matched-image", enough_space: true, minimum_free_gib: 51, free_gib: 120, download_bytes: 19769944180, plan: { host: { system, version: system === "windows-11" ? "25H2" : "26.5", build: "123", edition: "Professional", language: "zh-CN" }, version: "26.5", build: "456", bytes: 19769944180 } } }));
      await page.route("**/api/start", route => {
        starts++;
        const submitted = route.request().postDataJSON();
        assert.equal(submitted.guest_system, system);
        if (starts === 1) assert.equal(submitted.clone_key, "matched-image");
        state.status.job = { id: "install-" + starts, running: true, kind: "guest-install", setup_progress: { system, phase: "image", operation: "download", label: "正在下载系统镜像", received: 25 * 1048576, total: 100 * 1048576, bytes_per_second: 1048576 }, log: "镜像下载已开始" };
        return route.fulfill({ json: state.status.job });
      });
      await page.route("**/api/environment/initialization", route => {
        const action = route.request().method() === "POST" ? route.request().postDataJSON()?.action : "";
        if (action === "complete" && rejectComplete) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "环境网络检查尚未通过，请重新检查" });
        if (action === "install") state.initialization.started = true;
        if (action === "complete") state.initialization.required = false;
        return route.fulfill({ json: state.initialization });
      });
    },
  });
  const { page, state, errors, browser } = fixture;
  try {
    assert.equal(await page.getByRole("complementary", { name: "项目与终端" }).count(), 0);
    const picker = page.getByRole("combobox", { name: "客户机系统" });
    const storage = page.getByRole("region", { name: "环境存储目录" });
    await storage.getByLabel("存储目录", { exact: true }).waitFor();
    assert.equal(await storage.getByLabel("存储目录", { exact: true }).inputValue(), initialDirectory);
    await storage.getByText("这是 Cage 的环境数据目录。更新、重装或卸载应用会保留这里的数据；请勿手动删除。").waitFor();
    assert.equal(await storage.getByText("当前环境目录", { exact: true }).count(), 0);
    assert.equal(await storage.getByText("新存储位置", { exact: true }).count(), 0);
    await page.locator(".initialization-content").evaluate(async element => {
      await Promise.all(element.getAnimations({ subtree: true })
        .filter(animation => animation.animationName === "initialization-enter")
        .map(animation => animation.finished));
    });
    const before = await storage.boundingBox();
    await picker.click();
    const option = page.getByRole("option", { name: "自动匹配本机系统版本", exact: true });
    await option.waitFor();
    for (const name of [/^Debian/, /^Ubuntu/, /^macOS/, /^Windows/]) assert.equal(await page.getByRole("option", { name }).count(), 1);
    const expanded = await storage.boundingBox();
    assert.ok(Math.abs(expanded.y - before.y) < 1, "floating system choices must not move storage");
    const menu = await page.getByRole("listbox").boundingBox();
    assert.ok(menu.x >= 0 && menu.y >= 0 && menu.x + menu.width <= 1280 && menu.y + menu.height <= 850, "floating menu must stay inside the viewport");
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-systems-floating.png`, fullPage: true });
    await picker.press("Home"); await picker.press("Enter");
    await page.getByText("Cage 会检测本机系统版本，在下一步显示匹配的官方镜像、下载量与空间要求。").waitFor();
    await page.getByText(/系统镜像通过主机网络下载.*系统代理或 TUN/).waitFor();
    if (system === "windows-11") await page.getByRole("checkbox").check();
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-system.png`, fullPage: true });
    await page.getByRole("button", { name: "下一步：配置代理" }).click();
    await page.getByRole("heading", { name: "连接你的网络代理" }).waitFor();
    await page.getByText("根据本机系统版本匹配官方镜像，安装到独立客机中。").waitFor();
    await page.getByRole("button", { name: "上一步", exact: true }).click();
    await page.getByRole("combobox", { name: "客户机系统" }).getByText("自动匹配本机系统版本").waitFor();
    await page.getByRole("button", { name: "下一步：配置代理" }).click();
    await page.getByRole("button", { name: "开始安装", exact: true }).click();
    const progress = page.getByRole("region", { name: "环境安装进度" });
    await progress.getByText("25 MiB / 100 MiB", { exact: true }).waitFor();
    assert.equal(await progress.getByRole("progressbar").getAttribute("aria-valuenow"), "25");
    assert.equal(await page.getByRole("complementary", { name: "项目与终端" }).count(), 0);
    await page.reload();
    await progress.getByText("25 MiB / 100 MiB", { exact: true }).waitFor();
    assert.equal(starts, 1, "refresh must not restart the accepted installation");
    state.status.job = { ...state.status.job, running: false, error: "下载中断，已保存部分文件" };
    await progress.getByRole("alert").waitFor();
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-failed.png`, fullPage: true });
    await progress.getByRole("button", { name: "继续安装", exact: true }).click();
    await progress.getByRole("progressbar").waitFor();
    assert.equal(starts, 2);
    await page.setViewportSize({ width: 600, height: 650 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    if (evidence) await page.screenshot({ path: `${evidence}/${platform}-progress-compact.png`, fullPage: true });
    state.status.job = { id: "install-2", kind: "start", running: false, message: "环境已启动，联网检查未通过", error: "联网检查未通过：浏览器检测通道返回 HTTP 503，外网保持关闭" };
    Object.assign(state.status, { ready: true, vm_running: true, safety: { gate: { mode: "closed" } } });
    await page.getByRole("heading", { name: "环境已启动，等待联网检查", exact: true }).waitFor();
    assert.equal(await page.getByRole("button", { name: "继续安装", exact: true }).count(), 0);
    let networkChecks = 0;
    await page.route("**/api/safety/check", route => {
      networkChecks++;
      state.status.job = { id: "retry-network", kind: "safety", running: true, message: "正在检查联网条件" };
      return route.fulfill({ json: state.status.job });
    });
    await page.getByRole("button", { name: "运行网络与隔离检查", exact: true }).click();
    await page.getByText("正在检查联网条件", { exact: true }).waitFor();
    assert.equal(networkChecks, 1);
    assert.equal(starts, 2, "network recovery must not restart installation");
    state.status.job = { id: "install-2", kind: "start", running: false };
    Object.assign(state.status, { ready: true, vm_running: true, safety: { gate: { mode: "open", until: new Date(Date.now() + 3600000).toISOString() } } });
    await page.getByRole("button", { name: "进入工作台", exact: true }).click();
    await page.getByRole("alert").filter({ hasText: "网络检查" }).waitFor();
    assert.equal(await page.getByRole("complementary", { name: "项目与终端" }).count(), 0, "server completion rejection must keep initialization visible");
    rejectComplete = false;
    await page.getByRole("button", { name: "进入工作台", exact: true }).click();
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    await page.evaluate(() => window.cageNativeAction("settings"));
    await page.getByRole("region", { name: "设置", exact: true }).waitFor();
    await page.reload();
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    await page.evaluate(() => window.cageNativeAction("quit"));
    const quitDialog = page.getByRole("dialog", { name: "退出 Cage", exact: true });
    await quitDialog.getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(await page.getByRole("heading", { name: "为 Cage 准备独立环境" }).count(), 0);
    assert.deepEqual(errors, []);
    console.log(`PASS ${platform}: independent initialization, inline keyboard selection, back navigation, progress, refresh, retry, completion gate and reopen`);
  } finally { await browser.close(); }
}

{
  let location = { directory: "/preview/default-state", has_environment: false }, writes = 0, fail = true;
  const f = await createWorkbench({ initialization: { required: true, tracked: true }, beforeNavigate: async (page, state) => {
    state.status.local_vm_storage = true;
    await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, available: true, system: "macos", linux_systems: ["ubuntu-22.04"] } }));
    await page.route("**/api/environment/storage", route => {
      if (route.request().method() === "GET") return route.fulfill({ json: location });
      const body = route.request().postDataJSON();
      writes++;
      assert.equal(body.previous, "/preview/default-state");
      assert.equal(body.migrate, false);
      if (fail) return route.fulfill({ status: 400, contentType: "text/plain; charset=utf-8", body: "请选择专用空文件夹" });
      location = { ...location, directory: body.directory };
      state.status.job = { id: "save-storage", kind: "storage", running: false };
      return route.fulfill({ json: state.status.job });
    });
  } });
  try {
    const input = f.page.getByLabel("存储目录", { exact: true });
    assert.equal(await input.inputValue(), "/preview/default-state");
    await input.fill("/preview/chosen-state");
    assert.equal(writes, 0, "editing must not save before Next");
    await f.page.getByRole("button", { name: "下一步：配置代理" }).click();
    await f.page.getByRole("alert").filter({ hasText: "请选择专用空文件夹" }).waitFor();
    assert.equal(await input.inputValue(), "/preview/chosen-state");
    assert.equal(await f.page.getByRole("heading", { name: "连接你的网络代理" }).count(), 0);
    fail = false;
    await f.page.getByRole("button", { name: "下一步：配置代理" }).click();
    await f.page.getByRole("heading", { name: "连接你的网络代理" }).waitFor();
    assert.equal(location.directory, "/preview/chosen-state");
    assert.equal(writes, 2);
    await f.page.getByRole("button", { name: "上一步", exact: true }).click();
    assert.equal(await input.inputValue(), "/preview/chosen-state");
    assert.deepEqual(f.errors, []);
    console.log("PASS storage: one default-filled directory, save on Next, rejected path preserved, retry and back navigation");
  } finally { await f.browser.close(); }
}

{
  let imported = false, applied = false;
  const f = await createWorkbench({ initialization: { required: true, tracked: true }, beforeNavigate: async (page, state) => {
    state.status.local_vm_storage = true;
    await page.route("**/api/environment/storage", route => route.fulfill({ json: { directory: "/preview/cage-state", has_environment: false } }));
    await page.route("**/api/environment/guest-systems", route => route.fulfill({ json: { fresh: true, available: true, system: "macos" } }));
    await page.route("**/api/proxy", route => route.fulfill({ json: { active: applied, selected: applied ? "node" : "", sources: imported ? [{ id: "source", name: "自有节点", nodes: [{ id: "node", name: "本机测试代理", type: "socks5" }] }] : [], port: 17896, core_available: true } }));
    for (const action of ["import", "route"]) await page.route(`**/api/proxy/${action}`, route => {
      if (action === "import") imported = true;
      else applied = true;
      state.status.job = { id: action, kind: "proxy", running: false, message: "已完成" };
      return route.fulfill({ json: state.status.job });
    });
  } });
  try {
    const { page } = f;
    await page.getByRole("button", { name: "下一步：配置代理" }).click();
    const start = page.getByRole("button", { name: "开始安装", exact: true });
    assert.ok(await start.isDisabled());
    await page.getByRole("button", { name: "添加来源", exact: true }).click();
    await page.getByRole("button", { name: "手动节点", exact: true }).click();
    await page.getByLabel("来源名称", { exact: true }).fill("自有节点");
    await page.getByLabel("节点名称", { exact: true }).fill("本机测试代理");
    await page.getByLabel("服务器", { exact: true }).fill("127.0.0.1");
    await page.getByRole("button", { name: "导入并验证", exact: true }).click();
    await page.getByRole("tab", { name: "节点", exact: true }).click();
    await page.getByRole("button", { name: "选择节点 本机测试代理", exact: true }).click();
    await page.getByRole("button", { name: "应用代理", exact: true }).click();
    await page.getByText("已完成", { exact: true }).waitFor();
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === '开始安装' && !button.disabled));
    assert.deepEqual(f.errors, []);
    console.log("PASS first proxy: manual import, node selection and explicit apply unlock installation inside initialization");
  } finally { await f.browser.close(); }
}
