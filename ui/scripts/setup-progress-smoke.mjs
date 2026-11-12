import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const { browser, page, state, errors } = await createWorkbench();
try {
  let starts = 0;
  await page.route("**/api/start", (route) => {
    starts++;
    state.status.job = { id: "setup-" + starts, kind: "start", running: true, message: "正在准备基础环境",
      setup_progress: { phase: "image", label: "下载 Debian 13 基础镜像", received: 25 * 1048576, total: 100 * 1048576 } };
    return route.fulfill({ json: state.status.job });
  });
  await page.getByRole("button", { name: "启动 Cage 环境", exact: true }).click();
  const panel = page.getByRole("region", { name: "环境安装进度" });
  const bar = panel.getByRole("progressbar");
  await panel.getByText("25 MiB / 100 MiB").waitFor();
  assert.equal(await bar.getAttribute("aria-valuenow"), "25");
  assert.equal(await panel.locator('[aria-current="step"]').textContent(), "系统镜像");
  await panel.getByRole("button", { name: "操作详情" }).click();
  await page.getByRole("dialog").waitFor();
  await page.keyboard.press("Escape");
  if (process.env.CAGE_SETUP_SCREENSHOT) await page.screenshot({ path: process.env.CAGE_SETUP_SCREENSHOT, animations: "disabled" });
  state.status.job.setup_progress = { phase: "tools", label: "下载 code-server", received: 8 * 1048576 };
  await panel.getByText("已接收 8 MiB").waitFor();
  assert.equal(await bar.getAttribute("aria-valuenow"), null);
  state.status.job.setup_progress = { phase: "services", label: "正在配置浏览器与终端" };
  await panel.getByRole("heading", { name: "正在配置浏览器与终端" }).waitFor();
  assert.equal(await bar.getAttribute("aria-valuenow"), null);
  assert.equal(await panel.locator(".setup-transfer-bytes").count(), 0);
  await panel.getByRole("button", { name: "停止安装" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "取消" }).click();
  assert.equal(state.requests.filter(r => r.path === "/stop").length, 0);
  await page.setViewportSize({ width: 600, height: 600 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  state.status.job = { ...state.status.job, running: false, error: "下载重试次数已用尽，未完成文件已保存" };
  await panel.getByRole("alert").waitFor();
  assert.equal(await bar.count(), 0);
  await panel.getByText("可手动继续安装，已保存的下载会尝试续传。").waitFor();
  assert.equal(starts, 1);
  await panel.getByRole("button", { name: "继续安装" }).click();
  await panel.getByText("25 MiB / 100 MiB").waitFor();
  assert.equal(starts, 2);
  state.status.job = { ...state.status.job, running: false, error: "下载再次中断，未完成文件已保存" };
  await panel.getByRole("alert").waitFor();
  await panel.getByRole("button", { name: "继续安装" }).click();
  await panel.getByText("25 MiB / 100 MiB").waitFor();
  assert.equal(starts, 3);
  await panel.getByRole("button", { name: "停止安装" }).click();
  const stopped = page.waitForRequest("**/api/stop");
  await page.getByRole("dialog").getByRole("button", { name: "停止安装" }).click();
  await stopped;
  assert.deepEqual(errors, []);
  console.log("PASS: measured bytes and percent, unknown size, phase only, details, compact layout, cancel and retry.");
} finally { await browser.close(); }

{
  const {browser,page,state,errors} = await createWorkbench();
  try {
    state.status.job = {id:"mac-install",kind:"guest-install",running:true,message:"正在安装所选客户机系统…",setup_progress:{system:"macos",operation:"download",phase:"image",label:"正在下载 macOS 系统镜像",received:8*1024**3,total:20*1024**3,bytes_per_second:26*1024**2}};
    const panel = page.getByRole("region",{name:"环境安装进度"});
    const bar = panel.getByRole("progressbar");
    await panel.getByText("8 GiB / 20 GiB",{exact:true}).waitFor();
    await panel.getByText("· 26 MiB/s",{exact:true}).waitFor();
    assert.equal(await bar.getAttribute("aria-valuenow"),"40");
    if (process.env.CAGE_SETUP_NATIVE_SCREENSHOT) await page.screenshot({path:process.env.CAGE_SETUP_NATIVE_SCREENSHOT,animations:"disabled"});
    await page.reload();
    await panel.getByText("8 GiB / 20 GiB",{exact:true}).waitFor();
    assert.equal(await bar.getAttribute("aria-valuenow"),"40");
    state.status.job.setup_progress = {...state.status.job.setup_progress,operation:"verify",label:"正在校验 macOS 系统镜像",received:0};
    await panel.getByRole("heading",{name:"正在校验 macOS 系统镜像"}).waitFor();
    assert.equal(await bar.getAttribute("aria-valuenow"),"0");
    assert.equal(await panel.getByText(/MiB\/s/).count(),0);
    state.status.job.setup_progress = {system:"macos",phase:"packages",label:"正在安装 macOS 系统"};
    await panel.getByRole("heading",{name:"正在安装 macOS 系统"}).waitFor();
    assert.equal(await panel.locator('[aria-current="step"]').textContent(),"系统安装");
    assert.equal(await bar.getAttribute("aria-valuenow"),null);
    await page.setViewportSize({width:375,height:812});
    await page.getByRole("button",{name:"收起侧栏",exact:true}).click();
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
    if (process.env.CAGE_SETUP_MOBILE_SCREENSHOT) await page.screenshot({path:process.env.CAGE_SETUP_MOBILE_SCREENSHOT,animations:"disabled"});
    state.status.job = {...state.status.job,running:false,error:"安装中断，原文件和磁盘已保留"};
    await panel.getByRole("alert").waitFor();
    let submitted;
    await page.route("**/api/start",route=>{
      submitted = route.request().postDataJSON();
      state.status.job = {...state.status.job,running:true,error:""};
      return route.fulfill({json:state.status.job});
    });
    const response = page.waitForResponse("**/api/start");
    await panel.getByRole("button",{name:"继续安装"}).click();
    await response;
    assert.equal(submitted.guest_system,"macos");
    assert.deepEqual(errors,[]);
    console.log("PASS: native image bytes, percent, measured rate, refresh, separate verification, install stage, 375px layout and native retry.");
  } finally {await browser.close();}
}

{
  const { browser, page, state, errors } = await createWorkbench({
    initialization: { required: true, tracked: true, started: true },
    clientContext: { mode: "local", connection_id: "local", client_platform: "windows", server_platform: "windows" },
    beforeNavigate: async (page, state) => {
      state.status.job = { id: "windows-install", kind: "guest-install", running: false,
        setup_progress: { system: "windows-11", phase: "packages", operation: "oobe", label: "请在虚拟机连接窗口完成 Windows 首次设置" } };
    },
  });
  try {
    const panel = page.getByRole("region", { name: "环境安装进度" });
    await panel.getByText(/虚拟机连接（VMConnect）窗口/).waitFor();
    assert.equal(await panel.getByRole("progressbar").count(), 0);
    assert.equal(await panel.getByRole("button", { name: "停止安装" }).count(), 0);
    await page.reload();
    const resume = panel.getByRole("button", { name: "已进入桌面，继续安装" });
    await resume.waitFor();
    let submitted;
    await page.route("**/api/start", route => {
      submitted = route.request().postDataJSON();
      state.status.job = { ...state.status.job, running: true,
        setup_progress: { system: "windows-11", phase: "packages", operation: "credentials", label: "请在安装窗口输入客机管理员账户" } };
      return route.fulfill({ json: state.status.job });
    });
    await resume.click();
    await panel.getByText(/请保持该窗口开启/).waitFor();
    assert.equal(submitted.guest_system, "windows-11");
    assert.equal(await resume.count(), 0);
    for (const [operation, phase, label] of [["media", "image", "正在下载并校验客机开发组件"], ["tools", "tools", "正在安装客机应用和开发工具"], ["services", "services", "正在完成客机配置"]]) {
      state.status.job.setup_progress = { system: "windows-11", phase, operation, label };
      await panel.getByRole("heading", { name: label }).waitFor();
      await panel.getByText(/请保持 PowerShell 安装窗口开启/).waitFor();
    }
    await page.setViewportSize({ width: 600, height: 650 });
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    if (process.env.CAGE_SETUP_WINDOWS_SCREENSHOT) await page.screenshot({ path: process.env.CAGE_SETUP_WINDOWS_SCREENSHOT, animations: "disabled" });
    state.status.job.setup_progress = { system: "macos", phase: "tools", label: "正在安装客机应用和开发工具" };
    await panel.getByText(/请保持 PowerShell 安装窗口开启/).waitFor({ state: "hidden" });
    assert.deepEqual(errors, []);
    console.log("PASS: Windows OOBE resume, refresh, guest credentials, component phases and platform-specific guidance.");
  } finally { await browser.close(); }
}
