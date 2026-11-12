import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as pause } from "node:timers/promises";
import test from "node:test";
import { createDisplaySettings, densitySettings, displayDensity } from "./display-density.mjs";

function fakeManager() {
  const child = new EventEmitter();
  child.signals = [];
  child.kill = signal => { child.signals.push(signal ?? "SIGTERM"); };
  return child;
}

test("densities are bounded and take Chromium's 1/64 steps", () => {
  assert.equal(displayDensity(2), 2);
  assert.equal(displayDensity(1.8), 1.796875);
  assert.equal(displayDensity(0.75), 0.75);
  for (const value of [0.74, 4.01, NaN, Infinity, "2", undefined, null]) assert.equal(displayDensity(value), null);
  assert.equal(densitySettings(1.5), "Xft/DPI 147456\nGdk/WindowScalingFactor 1\nGdk/UnscaledDPI 147456\n");
});

test("a change is written before the manager is told to reload", t => {
  const directory = mkdtempSync(join(tmpdir(), "cage-density-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const file = join(directory, "settings", "xsettings.conf"), managers = [];
  const settings = createDisplaySettings({ file, initial: 2, available: true, start: () => { managers.push(fakeManager()); return managers.at(-1); } });
  assert.equal(readFileSync(file, "utf8"), densitySettings(2));
  assert.equal(settings.set(1.25), 1.25);
  assert.equal(readFileSync(file, "utf8"), densitySettings(1.25));
  assert.deepEqual(managers[0].signals, ["SIGHUP"]);
  // An unchanged density leaves applications alone.
  assert.equal(settings.set(1.25), 1.25);
  assert.deepEqual(managers[0].signals, ["SIGHUP"]);
  settings.close();
  assert.deepEqual(managers[0].signals, ["SIGHUP", "SIGTERM"]);
});

test("a manager that ends is started again with the current density", async t => {
  const directory = mkdtempSync(join(tmpdir(), "cage-density-"));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const file = join(directory, "xsettings.conf"), managers = [];
  const settings = createDisplaySettings({ file, initial: 2, available: true, restart: 5, start: () => { managers.push(fakeManager()); return managers.at(-1); } });
  managers[0].emit("exit", 1);
  assert.equal(settings.set(1), 1);
  await pause(30);
  assert.equal(managers.length, 2);
  assert.equal(readFileSync(file, "utf8"), densitySettings(1));
  settings.close();
  managers[1].emit("exit", 0);
  await pause(30);
  assert.equal(managers.length, 2);
});

test("a display without a settings manager keeps its density", () => {
  const settings = createDisplaySettings({ file: "/nonexistent/xsettings.conf", initial: 1.5, available: false, start: () => assert.fail("started") });
  assert.equal(settings.available, false);
  assert.equal(settings.set(1), 1.5);
  settings.close();
});

const tools = ["/usr/bin/Xtigervnc", "/usr/bin/xsettingsd", "/usr/bin/dump_xsettings"].every(existsSync);
test("xsettingsd publishes each density on a real display", { skip: !tools && "needs Xtigervnc and xsettingsd" }, async t => {
  // X socket paths are short; the repository may live in a deep directory.
  const directory = mkdtempSync(join(tmpdir(), "cgd-"));
  const display = `:${180 + process.pid % 400}`;
  const server = spawn("/usr/bin/Xtigervnc", [display, "-geometry", "640x480", "-depth", "24", "-rfbport", "-1", "-rfbunixpath", join(directory, "v"), "-nolisten", "tcp", "-SecurityTypes", "None", "-ac"], { stdio: "ignore" });
  const env = { ...process.env, DISPLAY: display };
  delete env.XAUTHORITY;
  let settings;
  t.after(() => { settings?.close(); server.kill(); rmSync(directory, { recursive: true, force: true }); });
  const published = async dpi => {
    for (let attempt = 0; attempt < 100; attempt++) {
      try { if (execFileSync("/usr/bin/dump_xsettings", { env, stdio: ["ignore", "pipe", "ignore"] }).toString().includes(`Xft/DPI ${dpi}\n`)) return true; } catch { /* Not owned yet. */ }
      await pause(50);
    }
    return false;
  };
  const file = join(directory, "xsettings.conf");
  settings = createDisplaySettings({ file, initial: 2, available: true, start: () => spawn("/usr/bin/xsettingsd", ["--config=" + file], { env, stdio: "ignore" }) });
  assert.equal(await published(196608), true);
  settings.set(1.25);
  assert.equal(await published(122880), true);
});
