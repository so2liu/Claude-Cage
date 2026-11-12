import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
for (const failure of ["http", "native"]) {
  const user = "cage";
  const permissions = { screen:false, input:false, projects:false, managed_credential:true };
  const calls = []; let finishFailed = false;
  const {page, browser, errors} = await createWorkbench({
    async beforeNavigate(page, state) {
      Object.assign(state.account, {initial_mac_setup:true, guest_system:"macos", macos_guest_user:user});
      Object.assign(state.status, {ready:true, vm_running:true});
      const frame = await page.evaluate(() => {
        const canvas = document.createElement("canvas");
        canvas.width = 160; canvas.height = 100;
        return canvas.toDataURL("image/png").split(",")[1];
      });
      await page.route("**/api/macos/first-setup", async r => {
        const q = r.request().postDataJSON(); calls.push(q.action);
        if (q.action === "permissions") return r.fulfill({json:permissions});
        if (q.action === "open") return r.fulfill({json:{session:"setup-fixture"}});
        if (q.action === "frame") return r.fulfill({json:{image:frame}});
        if (q.action === "scroll") {
          assert.equal(q.session,"setup-fixture");
          assert.ok(Number.isInteger(q.x) && q.x >= 0 && q.x < 1024);
          assert.ok(Number.isInteger(q.y) && q.y >= 0 && q.y < 640);
          assert.ok(q.delta_y > 0 && q.delta_y <= 512);
        }
        if (q.action === "fill-password") {
          assert.equal(q.password,undefined,"browser sent a credential");
          assert.equal(q.session,"setup-fixture");
        }
        if (q.action === "finish") {
          assert.ok(permissions.screen && permissions.input && permissions.projects);
          if (!finishFailed) {
            finishFailed=true;
            return failure === "http"
              ? r.fulfill({status:409,body:"临时设置账户未关闭，请重试完成设置"})
              : r.fulfill({json:{error:"未能保存首次设置，请重试"}});
          }
          state.account.initial_mac_setup = false; return r.fulfill({json:{closed:true}});
        }
        return r.fulfill({json:{}});
      });
    },
  });
  async function waitCall(action) {
    for(let i=0; !calls.includes(action) && i<200; i++) await new Promise(resolve=>setTimeout(resolve,20));
    assert.ok(calls.includes(action),`missing ${action}`);
  }
  try {
    await page.getByRole("dialog",{name:"首次设置虚拟 Mac"}).waitFor();
    await waitCall("screen");
    await page.getByRole("dialog",{name:"首次设置虚拟 Mac"}).getByText(/打开 Cage Guest Window 的窗口录制开关/).waitFor();
    const image = page.getByRole("dialog",{name:"首次设置虚拟 Mac"}).getByRole("img");
    await image.waitFor();
    if (process.env.CAGE_INITIALIZATION_EVIDENCE) await page.screenshot({ path: `${process.env.CAGE_INITIALIZATION_EVIDENCE}/setup-window-${failure}.png`, fullPage: true });
    const box = await image.boundingBox();
    await page.mouse.move(box.x+box.width/2,box.y+box.height/2);
    await page.mouse.wheel(0,400);
    await waitCall("scroll");
    const tools=page.getByRole("toolbar",{name:"画面工具"});
    await tools.getByRole("button",{name:"填写授权密码"}).click();
    await waitCall("fill-password");
    permissions.screen=true;
    await waitCall("input");
    await page.getByRole("dialog",{name:"首次设置虚拟 Mac"}).getByText(/打开 Cage Guest Window 的辅助功能开关/).waitFor();
    permissions.input=true;
    await waitCall("projects");
    await page.getByRole("dialog",{name:"首次设置虚拟 Mac"}).getByText(/打开 macFUSE 和 macFUSE \(local\) 两个开关，用于/).waitFor();
    assert.ok(!calls.includes("finish"),"finished before filesystem permission");
    permissions.projects=true;
    await page.getByRole("button",{name:"返回设置指引"}).click();
    const retry=page.getByRole("button",{name:"重试完成设置"});
    await retry.waitFor();
    await page.getByText("管理员密码与找不到设置页时的帮助",{exact:true}).click();
    await page.getByRole("button",{name:"重新打开文件系统扩展",exact:true}).click();
    await page.getByRole("alert").waitFor({state:"hidden"});
    assert.ok(await retry.isVisible(), "checking permissions hid the completion retry");
    await retry.click();
    await page.getByRole("heading",{name:"完成虚拟 Mac 的系统授权"}).waitFor({state:"hidden"});
    await page.getByRole("complementary", { name: "项目与终端" }).waitFor();
    await page.evaluate(() => window.cageNativeAction("settings"));
    await page.getByRole("region", { name: "设置", exact: true }).waitFor();
    await page.evaluate(() => window.cageNativeAction("quit"));
    await page.getByRole("dialog", { name: "退出 Cage", exact: true }).getByRole("button", { name: "取消", exact: true }).click();
    assert.equal(calls.filter(c=>c==="finish").length,2);
    assert.equal(calls.filter(c=>c==="verify-projects").length,0);
    assert.deepEqual(errors, []);
    console.log(user,failure,"automatic setup view, permission progression, credential fill, completion and failure recovery passed");
  } finally { await browser.close(); }
}
