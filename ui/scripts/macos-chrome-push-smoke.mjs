import assert from "node:assert/strict";
import { createWorkbench } from "./workbench-fixture.mjs";
let sent = 0,
  geometry = "main",
  windowID = 70,
  frameSocket,
  config,
  png;
const inputs = [];
const { page, browser, errors } = await createWorkbench({
  viewport: { width: 1280, height: 882 },
  deviceScaleFactor: 2,
  async beforeNavigate(page, state) {
    state.account.guest_system = "macos";
    state.status.ready = state.status.vm_running = true;
    await page.route("**/api/macos/chrome/window", (route) =>
      route.fulfill({
        json: {
          installed: true,
          running: true,
          screen: true,
          input: true,
          persistent: true,
          push_frames: true,
        },
      }),
    );
    await page.routeWebSocket("**/api/macos/chrome/connection?**", (socket) =>
      socket.onMessage((data) => {
        const m = JSON.parse(data);
        inputs.push(m.request);
        socket.send(
          JSON.stringify({
            id: m.id,
            result:
              m.request.action === "status"
                ? {
                    installed: true,
                    running: true,
                    screen: true,
                    input: true,
                    persistent: true,
                    push_frames: true,
                  }
                : { ok: true },
          }),
        );
      }),
    );
    await page.routeWebSocket("**/api/macos/chrome/frames?**", (socket) => {
      frameSocket = socket;
      socket.onMessage(async (data) => {
        const m = JSON.parse(data);
        if (m.ack) return;
        config = m;
        png = await page.evaluate(() => {
          const c = document.createElement("canvas");
          c.width = 1200;
          c.height = 1120;
          return c.toDataURL().split(",")[1];
        });
        send();
      });
    });
  },
});
function send() {
  const image = Buffer.from(png, "base64");
  const h = Buffer.from(
    JSON.stringify({
      mime: "image/png",
      imageLength: image.length,
      sequence: String(++sent),
      geometry,
      window: windowID,
      pid: 23,
      contentWidth: 600,
      contentHeight: 560,
      pixelWidth: 1200,
      pixelHeight: 1120,
      revision: config.revision,
    }),
  );
  const n = Buffer.alloc(4);
  n.writeUInt32BE(h.length);
  frameSocket.send(Buffer.concat([n, h, image]));
}
try {
  await page.getByRole("button", { name: "打开浏览器", exact: true }).click();
  const img = page.locator(".mac-claude-frame img");
  await img.waitFor();
  await page.waitForTimeout(200);
  await img.click();
  await page.waitForTimeout(200);
  assert.ok(
    inputs.some((v) => v.action === "pointer" && v.phase === "down"),
    "initial click",
  );
  assert.ok(
    inputs.findIndex((v) => v.action === "launch") <
      inputs.findIndex((v) => v.action === "pointer" && v.phase === "down"),
  );
  inputs.length = 0;
  geometry = "popup";
  windowID = 71;
  send();
  await page.waitForTimeout(200);
  await img.click();
  await page.waitForTimeout(200);
  assert.ok(
    inputs.some(
      (v) => v.action === "pointer" && v.phase === "down" && v.window === 71,
    ),
    "popup click",
  );
  const down = inputs.findIndex(
    (v) => v.action === "pointer" && v.phase === "down",
  );
  assert.ok(
    down > 0 && inputs[down - 1].action === "launch",
    "popup click must restore guest focus before HID delivery",
  );
  console.log(
    JSON.stringify({
      result: "passed",
      inputs: inputs.map((v) => ({
        action: v.action,
        phase: v.phase,
        window: v.window,
      })),
      errors,
    }),
  );
  assert.deepEqual(errors, []);
} finally {
  await browser.close();
}
