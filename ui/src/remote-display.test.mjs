import assert from "node:assert/strict";
import test from "node:test";
import { setTimeout as pause } from "node:timers/promises";
import { displayGeometry, displaySizeSync } from "./remote-display.mjs";

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
