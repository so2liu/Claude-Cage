import test from "node:test";
import assert from "node:assert/strict";
import { caretPollDelay, caretPollFast, caretPollSlow, caretFreshFor } from "./caret-poll.mjs";

test("caret polling is fast after input and backs off while the viewer is quiet", () => {
  assert.equal(caretPollDelay(0), caretPollFast);
  assert.equal(caretPollDelay(2999), caretPollFast);
  assert.equal(caretPollDelay(3000), 500);
  assert.equal(caretPollDelay(8000), 1000);
  assert.equal(caretPollDelay(20000), caretPollSlow);
  assert.equal(caretPollDelay(3600000), caretPollSlow);
  let previous = 0;
  for (let quiet = 0; quiet < 60000; quiet += 250) {
    assert.ok(caretPollDelay(quiet) >= previous);
    previous = caretPollDelay(quiet);
  }
});

test("a reading from the slowest interval can still begin a composition", () => {
  assert.ok(caretFreshFor > caretPollSlow);
});
