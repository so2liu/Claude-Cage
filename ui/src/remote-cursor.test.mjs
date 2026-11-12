import assert from "node:assert/strict";
import test from "node:test";
import { installLocalCursorFallback } from "./remote-cursor.mjs";

function cursorStub() {
  return {
    _target: { style: { cursor: "none" } },
    changes: [],
    change(...args) {
      this.changes.push(args);
      this._target.style.cursor = "remote";
    },
    clear() {
      this._target.style.cursor = "none";
    },
  };
}

const visibleShape = [0, 0, 0, 255];

test("macOS browser fallback remains visible before a remote cursor arrives", () => {
  const cursor = cursorStub();
  cursor._target = null; // RFB attaches its cursor asynchronously after construction.
  installLocalCursorFallback(cursor, true);
  cursor._target = { style: { cursor: "none" } };
  cursor.clear();
  assert.equal(cursor._target.style.cursor, "default");
  assert.deepEqual(cursor.changes, []);
});

test("macOS browser fallback replaces an empty or transparent remote cursor", () => {
  const cursor = cursorStub();
  installLocalCursorFallback(cursor, true);
  for (const rgba of [[], [0, 0, 0, 0], [255, 255, 255, 0, 0, 0, 0, 0]]) {
    cursor.change(rgba, 0, 0, rgba.length / 4, 1);
    assert.equal(cursor._target.style.cursor, "default");
  }
  assert.deepEqual(cursor.changes, []);
});

test("a real remote cursor shape is preserved without fallback pixels", () => {
  const cursor = cursorStub();
  installLocalCursorFallback(cursor, true);
  cursor.change(visibleShape, 2, 3, 1, 1);
  assert.equal(cursor._target.style.cursor, "remote");
  assert.deepEqual(cursor.changes, [[visibleShape, 2, 3, 1, 1]]);
});

test("non-target displays retain noVNC cursor behavior", () => {
  const cursor = cursorStub();
  const change = cursor.change;
  const clear = cursor.clear;
  installLocalCursorFallback(cursor, false);
  assert.equal(cursor.change, change);
  assert.equal(cursor.clear, clear);
  cursor.clear();
  assert.equal(cursor._target.style.cursor, "none");
  cursor.change([], 0, 0, 0, 0);
  assert.equal(cursor._target.style.cursor, "remote");
});
