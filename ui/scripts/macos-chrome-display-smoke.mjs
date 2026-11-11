import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";

const frames = new Map(), clicks = [];
let fixed = false;
const { page, browser, errors } = await createWorkbench({
  viewport: {width:1280, height:882}, deviceScaleFactor:2,
  async beforeNavigate(page, state) {
    state.account.guest_system = "macos";
    state.status.vm_running = state.status.ready = true;
    await page.route("**/api/macos/chrome/window", async route => {
      const input = route.request().postDataJSON();
      if (input.action === "pointer" && input.phase === "down") clicks.push(input);
      if (input.action !== "frame") return route.fulfill({json:{installed:true,running:true,screen:true,input:true}});
      const width = fixed ? 600 : input.width, height = fixed ? 560 : input.height;
      assert.equal(input.scale, 2);
      const geometry = width + "x" + height;
      if (!frames.has(geometry)) frames.set(geometry, await page.evaluate(([w,h]) => {
        const canvas = document.createElement("canvas");
        canvas.width=w*2; canvas.height=h*2;
        const ctx=canvas.getContext("2d");
        ctx.fillStyle="#f7f5f1"; ctx.fillRect(0,0,canvas.width,canvas.height);
        ctx.fillStyle="#202020"; ctx.font="32px sans-serif"; ctx.fillText("Chrome",60,100);
        return canvas.toDataURL().split(",")[1];
      }, [width,height]));
      return route.fulfill({json:{window:70,pid:23,image:frames.get(geometry),contentWidth:width,contentHeight:height,geometry}});
    });
  },
});
try {
  await page.getByRole("button",{name:"打开浏览器",exact:true}).click();
  const pane = page.locator(".mac-claude-frame");
  const img = pane.getByRole("img");
  async function fillsPane() {
    await page.waitForFunction(() => {
      const box=document.querySelector(".mac-claude-frame"), img=box?.querySelector("img");
      if (!img?.complete || !img.naturalWidth) return false;
      const a=img.getBoundingClientRect(), b=box.getBoundingClientRect();
      return Math.abs(a.width-b.width)<2 && Math.abs(a.height-b.height)<2 &&
        Math.abs(img.naturalWidth-2*b.width)<3 && Math.abs(img.naturalHeight-2*b.height)<3;
    });
  }
  await fillsPane();
  await page.setViewportSize({width:1110,height:750});
  await fillsPane();
  fixed=true;
  await page.waitForFunction(() => document.querySelector(".mac-claude-frame img")?.naturalWidth===1200 &&
    getComputedStyle(document.querySelector(".mac-claude-frame")).backgroundColor==="rgb(247, 245, 241)");
  const bounds = await img.boundingBox();
  assert.ok(Math.abs(bounds.width-600)<1);
  await img.click({position:{x:bounds.width/2,y:bounds.height/2}});
  await page.waitForFunction(() => document.activeElement?.classList.contains("mac-claude-typing"));
  for (let i=0; !clicks.length && i<40; i++) await new Promise(r=>setTimeout(r,25));
  assert.equal(clicks[0]?.geometry,"600x560");
  assert.ok(Math.abs(clicks[0].x-0.5)<0.005 && Math.abs(clicks[0].y-0.5)<0.005);
  assert.equal(await img.evaluate(el=>getComputedStyle(el).outlineStyle),"none");
  assert.equal(await page.evaluate(()=>document.fullscreenElement),null);
  assert.deepEqual(errors,[]);
  console.log("2x pixels, panel resize, fixed-window background, focus and cropped-frame inputs passed.");
} finally { await browser.close(); }
