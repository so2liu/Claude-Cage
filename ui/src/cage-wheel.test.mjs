import assert from "node:assert/strict";
import test from "node:test";
import CageWheel, { cageWheelLimit, cageWheelMessage, cageWheelPacket } from "../vendor/novnc/core/cage-wheel.js";

function fixture() {
  const sent = [], frames = [];
  const wheel = new CageWheel((...message) => sent.push(message), callback => frames.push(callback), handle => { frames[handle - 1] = null; });
  // One display frame: run what was scheduled for it.
  const frame = () => { for (const callback of frames.splice(0)) callback?.(); };
  return { wheel, sent, frames, frame };
}

test("the first distance is sent at once and later ones once per frame", () => {
  const { wheel, sent, frame } = fixture();
  wheel.add(10, 20, 0, 120);
  assert.deepEqual(sent, [[10, 20, 0, 120]]);
  // A trackpad reports many small distances between two frames.
  for (const distance of [3, 4.5, 2.25, 7]) wheel.add(11, 21, 0, distance);
  assert.equal(sent.length, 1);
  frame();
  assert.deepEqual(sent[1], [11, 21, 0, 17]);
  // The fraction of a pixel is kept, and an idle frame ends the series.
  frame();
  assert.equal(sent.length, 2);
  wheel.add(12, 22, 0, 0.75);
  assert.deepEqual(sent[2], [12, 22, 0, 1]);
});

test("distance beyond one message is carried into the next frames", () => {
  const { wheel, sent, frame } = fixture();
  wheel.add(1, 2, -5000, 3000);
  frame();
  frame();
  frame();
  assert.deepEqual(sent, [[1, 2, -cageWheelLimit, cageWheelLimit], [1, 2, -cageWheelLimit, 952], [1, 2, -904, 0]]);
});

test("less than a pixel waits, and stopping discards what was not sent", () => {
  const { wheel, sent, frame, frames } = fixture();
  wheel.add(1, 2, 0.2, 0.2);
  assert.equal(sent.length, 0);
  assert.equal(frames.length, 0);
  wheel.add(1, 2, 0.4, 0);
  assert.deepEqual(sent, [[1, 2, 1, 0]]);
  wheel.add(1, 2, 0, 300);
  wheel.stop();
  frame();
  assert.equal(sent.length, 1);
  wheel.add(3, 4, 0, -40);
  assert.deepEqual(sent[1], [3, 4, 0, -40]);
});

test("a message carries its position and signed distances", () => {
  const packet = cageWheelPacket(513, 1026, -720, 2048), view = new DataView(packet.buffer);
  assert.equal(packet.length, 14);
  assert.deepEqual([packet[0], packet[1]], [cageWheelMessage, 0]);
  assert.deepEqual([view.getUint16(2), view.getUint16(4), view.getInt32(6), view.getInt32(10)], [513, 1026, -720, 2048]);
});
