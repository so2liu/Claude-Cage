import assert from "node:assert/strict";
import net from "node:net";
import { once } from "node:events";
import { createPrivacyController } from "./browser-privacy-fixture.mjs";

// Run in an isolated Linux X display with CAGE_TEST_BROWSER_PATH set.
const fixture = await createPrivacyController({ headless: false });
const { api, cdp, evaluate, waitFor, origin } = fixture;
let legacy, listener, accepted;
try {
  const original = (await api("/tabs")).active;
  await waitFor(() => evaluate(original, "document.readyState === 'complete'"), "initial page");
  await evaluate(original, "document.querySelector('input').value='未保存输入🙂';window.framesSeen=0;window.heartbeats=0;requestAnimationFrame(function tick(){framesSeen++;requestAnimationFrame(tick)});setInterval(()=>heartbeats++,200)");
  await api("/visibility", { visible: false });
  await waitFor(() => evaluate(original, "document.hidden"), "background visibility");
  const frames = await evaluate(original, "framesSeen");
  const heartbeat = await evaluate(original, "heartbeats");
  await waitFor(() => evaluate(original, `heartbeats > ${heartbeat}`), "background JavaScript remains live");
  assert.equal(await evaluate(original, "framesSeen"), frames);
  const created = await api("/new", { url: origin + "/background-new" });
  await waitFor(() => evaluate(created.id, "document.hidden"), "new background tab");
  await api("/close", { id: created.id });
  for (let cycle = 0; cycle < 12; cycle++) {
    await api("/visibility", { visible: true });
    await waitFor(() => evaluate(original, "!document.hidden"), "restore viewing");
    assert.equal(await evaluate(original, "document.querySelector('input').value"), "未保存输入🙂");
    assert.equal((await cdp(original, "Browser.getWindowForTarget", { targetId: original })).bounds.windowState, "fullscreen");
    await api("/visibility", { visible: false });
    await waitFor(() => evaluate(original, "document.hidden"), "hide viewing");
  }
  listener = net.createServer(socket => { accepted = socket; });
  await new Promise(resolve => listener.listen(15900, "127.0.0.1", resolve));
  legacy = net.connect(15900, "127.0.0.1");
  await once(legacy, "connect");
  await waitFor(() => evaluate(original, "!document.hidden"), "legacy VNC viewer restores the hidden window");
  legacy.destroy(); accepted.destroy();
  await new Promise(resolve => listener.close(resolve)); listener = undefined;
  await waitFor(() => evaluate(original, "document.hidden"), "hide after legacy viewer closes");
  console.log(JSON.stringify({ passed: true, cycles: 12, checks: ["background rendering pauses", "background JavaScript continues", "new background tab stays hidden", "fullscreen and unsaved input restored", "legacy VNC viewer respected"] }));
} finally {
  legacy?.destroy(); accepted?.destroy(); listener?.close();
  await api("/visibility", { visible: true }).catch(() => {});
  await fixture.close();
}
