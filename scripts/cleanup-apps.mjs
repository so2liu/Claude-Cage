#!/usr/bin/env node
// Remove Cage bundles only after the caller has verified the replacement.
import { execFileSync } from "node:child_process";
import { lstat, readdir, realpath, rm } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const bundleID = "io.github.lee-xeo.claude-cage";
function isCage(bundle) {
  try {
    return (
      execFileSync(
        "/usr/libexec/PlistBuddy",
        [
          "-c",
          "Print :CFBundleIdentifier",
          path.join(bundle, "Contents/Info.plist"),
        ],
        { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] },
      ).trim() === bundleID
    );
  } catch {
    return false;
  }
}
function inUse(bundle) {
  const files = ["Contents/MacOS/Cage", "Contents/Resources/cage", "Contents/Resources/Cage Virtual Machine.app/Contents/MacOS/cage"].map((file) => path.join(bundle, file));
  try {
    return execFileSync("/usr/sbin/lsof", ["-t", ...files], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim() !== "";
  } catch (error) {
    if (String(error.stdout || "").trim()) return true;
    if (error.status === 1) return false;
    throw error;
  }
}
function intact(bundle) {
  try {
    execFileSync('/usr/bin/codesign', ['--verify', '--deep', '--strict', bundle], { stdio: 'ignore' });
    return true;
  } catch { return false; }
}

export async function cleanupApps({ keep, roots, onInUse = () => {}, onModified = () => {} }) {
  if (!keep.length)
    throw new Error("At least one verified Cage.app must be kept.");
  const retained = new Set();
  for (const bundle of keep) {
    if (!isCage(bundle) || !intact(bundle)) throw new Error(`Invalid replacement App: ${bundle}`);
    retained.add(await realpath(bundle));
  }
  const removed = [];
  async function visit(directory) {
    let stat;
    try {
      stat = await lstat(directory);
    } catch (error) {
      if (error.code === "ENOENT") return;
      throw error;
    }
    if (!stat.isDirectory() || stat.isSymbolicLink()) return;
    const canonical = await realpath(directory);
    if (retained.has(canonical)) return;
    if (directory.endsWith(".app")) {
      if (isCage(directory)) {
        if (!intact(directory)) {
          onModified(directory);
          return;
        }
        if (inUse(directory)) {
          onInUse(directory);
          return;
        }
        await rm(directory, { recursive: true });
        removed.push(directory);
      }
      return;
    }
    for (const entry of await readdir(directory))
      await visit(path.join(directory, entry));
  }
  for (const root of roots) await visit(path.resolve(root));
  return removed;
}

if (
  process.argv[1] &&
  import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href
) {
  const keep = [],
    roots = [];
  for (let i = 2; i < process.argv.length; i += 2) {
    const flag = process.argv[i],
      value = process.argv[i + 1];
    if (!value || !["--keep", "--root"].includes(flag))
      throw new Error(
        "Usage: cleanup-apps.mjs --keep <verified-app> [--keep <app>] --root <directory> ...",
      );
    (flag === "--keep" ? keep : roots).push(value);
  }
  if (!roots.length) throw new Error("At least one cleanup root is required.");
  for (const bundle of await cleanupApps({
    keep, roots,
    onInUse: (bundle) => console.error(`Old Cage App is still running; quit it and rerun cleanup: ${bundle}`),
    onModified: (bundle) => console.error(`旧应用包含修改内容或无法验证完整性，文件已保留：${bundle}`),
  }))
    console.log(`Removed old App: ${bundle}`);
}
