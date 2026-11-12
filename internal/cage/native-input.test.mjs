import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { EventEmitter } from "node:events";
import { mkdtempSync, mkdirSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough } from "node:stream";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { createDesktopRelay } from "./native-input.mjs";

function fakeChild() {
  const child = new EventEmitter();
  child.stdin = new PassThrough();
  child.stdout = new PassThrough();
  child.killed = false;
  child.kill = () => { child.killed = true; };
  child.stdin.on("finish", () => child.emit("close", 0));
  return child;
}

test("one relay answers consecutive requests in order", async () => {
  const children = [];
  const relay = createDesktopRelay({ start: () => { const child = fakeChild(); children.push(child); return child; } });
  const answers = Promise.all([relay.request('{"n":1}\n'), relay.request('{"n":2}\n')]);
  assert.equal(children.length, 1);
  assert.equal(children[0].stdin.read().toString(), '{"n":1}\n{"n":2}\n');
  // Responses can arrive split or joined.
  children[0].stdout.write('{"a":');
  children[0].stdout.write('1}\n{"a":2}\n');
  assert.deepEqual(await answers, ['{"a":1}', '{"a":2}']);
  const third = relay.request('{"n":3}\n');
  children[0].stdout.write('{"a":3}\n');
  assert.equal(await third, '{"a":3}');
  assert.equal(children.length, 1);
  relay.close();
});

test("an idle relay ends and the next request starts another", async () => {
  const children = [];
  const relay = createDesktopRelay({ start: () => { const child = fakeChild(); children.push(child); return child; }, idle: 20 });
  const first = relay.request("a\n");
  children[0].stdout.write("1\n");
  await first;
  await new Promise(resolve => children[0].once("close", resolve));
  assert.equal(relay.available(), true);
  const second = relay.request("b\n");
  assert.equal(children.length, 2);
  children[1].stdout.write("2\n");
  assert.equal(await second, "2");
  relay.close();
});

test("a relay that never answered is not started again", async () => {
  const children = [];
  const relay = createDesktopRelay({ start: () => { const child = fakeChild(); children.push(child); return child; } });
  const refused = relay.request("a\n");
  children[0].emit("close", 1);
  await assert.rejects(refused, /unavailable/);
  assert.equal(relay.available(), false);
});

test("a stalled bridge rejects every waiting request and ends the relay", async () => {
  const children = [];
  const relay = createDesktopRelay({ start: () => { const child = fakeChild(); children.push(child); return child; }, timeout: 20 });
  const answered = relay.request("a\n");
  children[0].stdout.write("1\n");
  await answered;
  await assert.rejects(Promise.all([relay.request("b\n"), relay.request("c\n")]), /unavailable/);
  assert.equal(children[0].killed, true);
  // It answered before, so a later request may try again.
  assert.equal(relay.available(), true);
});

test("an unrequested or oversized answer ends the relay", async () => {
  const children = [];
  const relay = createDesktopRelay({ start: () => { const child = fakeChild(); children.push(child); return child; } });
  const first = relay.request("a\n");
  children[0].stdout.write("1\nunrequested\n");
  await first;
  assert.equal(children[0].killed, true);
  const second = relay.request("b\n");
  children[1].stdout.write("x".repeat(40000));
  await assert.rejects(second, /unavailable/);
});

test("the helper relays each line to the input bridge until its pipe closes", { skip: process.platform === "win32" }, async () => {
  const home = mkdtempSync(join(tmpdir(), "cage-input-"));
  const seen = [];
  const bridge = createServer((socket) => {
    socket.once("data", (data) => { seen.push(data.toString()); socket.end(JSON.stringify({ echo: JSON.parse(data).n }) + "\n"); });
  });
  let relay;
  try {
    mkdirSync(join(home, ".cache/cage-input"), { recursive: true });
    await new Promise(resolve => bridge.listen(join(home, ".cache/cage-input/input.sock"), resolve));
    const start = () => spawn("python3", [fileURLToPath(new URL("./native-ime.py", import.meta.url)), "--relay"], { env: { ...process.env, HOME: home }, stdio: ["pipe", "pipe", "inherit"] });
    const children = [];
    relay = createDesktopRelay({ start: () => { const child = start(); children.push(child); return child; }, idle: 50 });
    assert.deepEqual(await Promise.all([1, 2, 3].map(n => relay.request(JSON.stringify({ n }) + "\n"))), [1, 2, 3].map(n => JSON.stringify({ echo: n })));
    assert.equal(seen.length, 3);
    assert.equal(children.length, 1);
    // An unreachable bridge is an ordinary unavailable answer.
    await new Promise(resolve => bridge.close(resolve));
    rmSync(join(home, ".cache/cage-input/input.sock"), { force: true });
    assert.equal(await relay.request('{"n":4}\n'), '{"available":false}');
    assert.equal(await new Promise(resolve => children[0].once("close", resolve)), 0);
  } finally {
    relay?.close();
    bridge.close();
    rmSync(home, { recursive: true, force: true });
  }
});
