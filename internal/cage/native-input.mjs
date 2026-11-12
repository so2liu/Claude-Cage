import { createConnection } from "node:net";
import { spawn } from "node:child_process";

const helper = ["-n", "-H", "-u", "cage", "/usr/local/bin/cage-native-input"];
const responseLimit = 32768;

// The caret is polled several times a second while Desktop has focus. One
// relay answers consecutive requests; it ends after a short idle period.
// A relay that never answered is not started again.
export function createDesktopRelay({ start = () => spawn("/usr/bin/sudo", [...helper, "--relay"], { stdio: ["pipe", "pipe", "ignore"] }), idle = 10000, timeout = 3500 } = {}) {
  let current, answered = false, unusable = false;
  const open = () => {
    const relay = { child: start(), jobs: [], data: "", closed: false, timer: undefined };
    const close = () => {
      if (relay.closed) return;
      relay.closed = true;
      if (current === relay) current = undefined;
      if (!answered) unusable = true;
      clearTimeout(relay.timer);
      for (const job of relay.jobs.splice(0)) { clearTimeout(job.timer); job.reject(new Error("Desktop input bridge unavailable")); }
      relay.child.kill();
    };
    relay.close = close;
    relay.child.stdout.setEncoding("utf8");
    relay.child.stdout.on("data", (chunk) => {
      relay.data += chunk;
      for (let end; (end = relay.data.indexOf("\n")) >= 0;) {
        const line = relay.data.slice(0, end), job = relay.jobs.shift();
        relay.data = relay.data.slice(end + 1);
        if (!job) return close();
        clearTimeout(job.timer);
        answered = true;
        job.resolve(line);
      }
      if (relay.data.length > responseLimit) return close();
      if (!relay.jobs.length) relay.timer = setTimeout(() => relay.child.stdin.end(), idle);
    });
    relay.child.on("error", close);
    relay.child.on("close", close);
    relay.child.stdin.on("error", close);
    return relay;
  };
  return {
    available: () => !unusable,
    request(line) {
      return new Promise((resolve, reject) => {
        const relay = current && !current.closed && !current.child.stdin.writableEnded ? current : current = open();
        clearTimeout(relay.timer);
        // Answers arrive in request order. A stalled bridge ends the relay.
        relay.jobs.push({ resolve, reject, timer: setTimeout(relay.close, timeout) });
        relay.child.stdin.write(line);
      });
    },
    close: () => current?.close(),
  };
}

const desktopRelay = createDesktopRelay();

// Each display owns a private IBus socket. The browser can access Desktop's
// bridge only through this fixed, sudo-authorized helper, with data on stdin.
export function nativeInput(value) {
  return new Promise((resolve, reject) => {
    let data = "";
    const finish = () => {
      try {
        const result = JSON.parse(data);
        if (result.error) throw new Error(result.error);
        resolve(result);
      } catch (error) { reject(error); }
    };
    const unavailable = (error) => {
      if (value.mode === "status") resolve({ available: false });
      else reject(error);
    };
    const collect = (chunk) => {
      data += chunk;
      if (data.length > responseLimit) {
        channel.destroy();
        reject(new Error("Input response too large"));
      }
    };
    let channel;
    if (value.target === "desktop" && desktopRelay.available()) {
      desktopRelay.request(JSON.stringify(value) + "\n").then((line) => { data = line; finish(); }, unavailable);
    } else if (value.target === "desktop") {
      const child = spawn("/usr/bin/sudo", [...helper, "--request"], {
        stdio: ["pipe", "pipe", "ignore"], timeout: 3500,
      });
      channel = child.stdout;
      channel.on("data", collect);
      child.on("error", unavailable);
      child.stdin.on("error", unavailable);
      child.on("close", (code) => {
        if (code === 0) finish();
        else unavailable(new Error("Desktop input bridge unavailable"));
      });
      child.stdin.end(JSON.stringify(value) + "\n");
    } else {
      channel = createConnection("/home/cage-browser/.cache/cage-input/input.sock");
      channel.setTimeout(3000, () => channel.destroy(new Error("Input bridge timed out")));
      channel.on("error", unavailable);
      channel.on("connect", () => channel.end(JSON.stringify(value) + "\n"));
      channel.on("data", collect);
      channel.on("end", finish);
    }
  });
}
