import { spawn } from "node:child_process";
import { existsSync, mkdirSync, renameSync, writeFileSync } from "node:fs";
import path from "node:path";

// Chromium applies an XSETTINGS density in steps of 1/64.
export function displayDensity(value) {
  return typeof value === "number" && value >= 0.75 && value <= 4 ? Math.round(value * 64) / 64 : null;
}

export function densitySettings(scale) {
  const dpi = Math.round(scale * 96 * 1024);
  return `Xft/DPI ${dpi}\nGdk/WindowScalingFactor 1\nGdk/UnscaledDPI ${dpi}\n`;
}

// A display's pixel density is an XSETTINGS value, and Chromium follows a
// change without restarting. Each viewer can then have the density of its own
// screen. Without xsettingsd the display keeps the density it started with.
export function createDisplaySettings({
  file, initial, available = existsSync("/usr/bin/xsettingsd"),
  start = () => spawn("/usr/bin/xsettingsd", ["--config=" + file], { stdio: "ignore" }), restart = 1000,
}) {
  let scale = initial, child, timer, closed = false;
  const write = () => {
    writeFileSync(file + ".new", densitySettings(scale));
    renameSync(file + ".new", file);
  };
  const run = () => {
    const current = child = start();
    // Applications fall back to 96 DPI while no manager owns the settings.
    const ended = () => {
      if (child !== current) return;
      child = undefined;
      if (!closed) timer = setTimeout(run, restart);
    };
    current.on("error", ended);
    current.on("exit", ended);
  };
  if (available) {
    mkdirSync(path.dirname(file), { recursive: true });
    write();
    run();
  }
  return {
    available,
    get scale() { return scale; },
    set(value) {
      if (!available || value === scale) return scale;
      scale = value;
      write();
      // A manager that is being restarted reads the new file when it starts.
      child?.kill("SIGHUP");
      return scale;
    },
    close() {
      closed = true;
      clearTimeout(timer);
      child?.kill();
    },
  };
}
