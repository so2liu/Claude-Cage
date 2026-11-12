import assert from "node:assert/strict";
import test from "node:test";
import { setTimeout as pause } from "node:timers/promises";
import { displayGeometry, displaySizeSync, watchPixelRatio } from "./remote-display.mjs";

test("normal size preserves logical pixels; zoom changes workspace, large displays retain aspect ratio", () => {
  assert.deepEqual(displayGeometry({ width: 1000, height: 700 }, { desktop: true }), { width: 2000, height: 1400 });
  assert.deepEqual(displayGeometry({ width: 1000, height: 700 }, { desktop: true, zoom: 1.25 }), { width: 1600, height: 1120 });
  assert.deepEqual(displayGeometry({ width: 5000, height: 3000 }, { desktop: true }), { width: 8192, height: 4915 });
  assert.deepEqual(displayGeometry({ width: 286, height: 446 }), { width: 572, height: 892 });
  assert.deepEqual(displayGeometry({ width: 160, height: 240 }, { density: 1.5 }), { width: 320, height: 360 });
  assert.deepEqual(displayGeometry({ width: 1000, height: 700 }, { desktop: true, zoom: 1.25, density: 1.5 }), { width: 1200, height: 840 });
  assert.deepEqual(displayGeometry({ width: 1000, height: 700 }, { density: 1 }), { width: 1000, height: 700 });
  assert.deepEqual(displayGeometry({ width: 1000, height: 700 }, { density: 1.25 }), { width: 1250, height: 875 });
  assert.deepEqual(displayGeometry({ width: 1000, height: 700 }, { density: 0 }), { width: 2000, height: 1400 });
  assert.deepEqual(displayGeometry({ width: 160, height: 240 }, { density: 1 }), { width: 320, height: 240 });
  assert.equal(displayGeometry({ width: 0, height: 700 }), null);
});

test("a guest that follows its viewer renders one pixel per screen pixel", () => {
  const size = { width: 1000, height: 700 };
  // The pixel count follows the screen; zoom only changes the density.
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 2 }), { width: 2000, height: 1400, scale: 2 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 1 }), { width: 1000, height: 700, scale: 1 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 1.5 }), { width: 1500, height: 1050, scale: 1.5 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 2, zoom: 1.25 }), { width: 2000, height: 1400, scale: 2.5 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 1, zoom: 0.75 }), { width: 1000, height: 700, scale: 0.75 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 2, zoom: 0.9 }), { width: 2000, height: 1400, scale: 1.796875 });
  // Reduced clarity keeps three quarters of the pixels, but never fewer than the 1x screen has.
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 2, density: 1.5 }), { width: 1500, height: 1050, scale: 1.5 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 1, density: 1.5 }), { width: 1000, height: 700, scale: 1 });
  // Denser screens are rendered as today, and odd ratios are bounded.
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 3 }), { width: 2000, height: 1400, scale: 2 });
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: 0.5 }), { width: 1000, height: 700, scale: 1 });
  // The browser keeps its minimum logical size, and a huge window its density.
  assert.deepEqual(displayGeometry({ width: 160, height: 240 }, { pixelRatio: 1.25 }), { width: 200, height: 300, scale: 1.25 });
  assert.deepEqual(displayGeometry({ width: 5000, height: 3000 }, { desktop: true, pixelRatio: 2 }), { width: 8192, height: 4915, scale: 1.640625 });
  // Without a ratio the guest keeps its fixed density and receives none.
  assert.deepEqual(displayGeometry(size, { desktop: true, pixelRatio: undefined }), { width: 2000, height: 1400 });
});

test("pixel ratio changes are reported until watching stops", () => {
  const queries = [];
  const view = {
    devicePixelRatio: 2,
    matchMedia(text) {
      const query = { text, listeners: new Set(), addEventListener(_, listener) { this.listeners.add(listener); }, removeEventListener(_, listener) { this.listeners.delete(listener); } };
      queries.push(query);
      return query;
    },
  };
  const move = ratio => { view.devicePixelRatio = ratio; const query = queries.at(-1); for (const listener of [...query.listeners]) { query.listeners.delete(listener); listener(); } };
  let changes = 0;
  const stop = watchPixelRatio(() => changes++, view);
  assert.equal(queries[0].text, "(resolution: 2dppx)");
  move(1);
  assert.equal(changes, 1);
  assert.equal(queries[1].text, "(resolution: 1dppx)");
  move(1.5);
  assert.equal(changes, 2);
  stop();
  assert.equal(queries[2].listeners.size, 0);
});

test("a size with another density is a different request", async () => {
  const requests = [];
  const sync = displaySizeSync(async (size) => { requests.push(size.scale); }, { delay: 0 });
  try {
    sync.update({ width: 1000, height: 700, scale: 2 });
    await pause(10);
    sync.update({ width: 1000, height: 700, scale: 2 });
    sync.update({ width: 1000, height: 700, scale: 2.5 });
    await pause(10);
    assert.deepEqual(requests, [2, 2.5]);
  } finally { sync.dispose(); }
});

test("a slow guest applies the newest size last without concurrent requests", async () => {
  const requests = [];
  let release;
  const sync = displaySizeSync(async (size) => {
    requests.push(size.width);
    if (requests.length === 1) await new Promise((resolve) => { release = resolve; });
  }, { delay: 0 });
  try {
    sync.update({ width: 1000, height: 700 });
    await pause(10);
    sync.update({ width: 1200, height: 700 });
    sync.update({ width: 1400, height: 700 });
    await pause(10);
    assert.deepEqual(requests, [1000]);
    release();
    await pause(10);
    assert.deepEqual(requests, [1000, 1400]);
  } finally { sync.dispose(); }
});

test("transient failure recovers without another resize; disconnect cancels scheduled work", async () => {
  let requests = 0, recovered = 0;
  const sync = displaySizeSync(async () => {
    if (++requests === 1) throw new Error("guest busy");
  }, { delay: 0, onSuccess: () => recovered++ });
  sync.update({ width: 1000, height: 700 });
  await pause(600);
  assert.equal(requests, 2);
  assert.equal(recovered, 1);
  sync.update({ width: 1200, height: 700 });
  sync.dispose();
  await pause(20);
  assert.equal(requests, 2);
});

test("persistent failure has bounded retries and supports explicit retry", async () => {
  let requests = 0;
  const sync = displaySizeSync(async () => { requests++; throw new Error("guest unavailable"); }, { delay: 0 });
  try {
    sync.update({ width: 1000, height: 700 });
    await pause(3900);
    assert.equal(requests, 4);
    sync.retry();
    await pause(20);
    assert.equal(requests, 5);
  } finally { sync.dispose(); }
});

test("a continuous drag sends the latest size on each tick", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const requests = [];
  const sync = displaySizeSync(async size => { requests.push(size.width); }, { delay: 100 });
  try {
    for (let width = 1000; width < 1012; width++) {
      sync.update({ width, height: 700 });
      t.mock.timers.tick(25);
      await Promise.resolve();
    }
    assert.deepEqual(requests, [1003, 1007, 1011]);
  } finally { sync.dispose(); }
});

test("native desktop honors its actual DPI instead of silently doubling 100% and 125% guests", () => {
  for (const density of [1, 1.25, 1.5, 1.75, 2, 2.5, 3]) {
    assert.deepEqual(displayGeometry({ width: 1200, height: 800 }, { desktop: true, density }),
      { width: 1200 * density, height: 800 * density });
  }
});

test("automatic desktop fits the window at native readable scale and bounds large allocations", () => {
  assert.deepEqual(displayGeometry({ width: 1200, height: 800 }, { desktop: true, zoom: "auto", density: 1 }),
    { width: 1200, height: 800 });
  const large = displayGeometry({ width: 6000, height: 4000 }, { desktop: true, zoom: "auto", density: 3 });
  assert.ok(large.width * large.height <= 12_000_000);
  assert.ok(Math.abs(large.width / large.height - 1.5) < 0.002);
});
