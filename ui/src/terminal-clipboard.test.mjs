import test from "node:test";
import assert from "node:assert/strict";
import { terminalClipboardText } from "./terminal-clipboard.mjs";

test("OSC 52 accepts UTF-8 clipboard writes within the shared clipboard limit", () => {
  const text = "Claude 回复\n含有换行和 emoji 🖼️";
  assert.equal(terminalClipboardText(`c;${Buffer.from(text).toString("base64")}`), text);
  assert.equal(terminalClipboardText(";"), "");
  const maximum = "x".repeat(2 * 1024 * 1024);
  assert.equal(terminalClipboardText(`c;${Buffer.from(maximum).toString("base64")}`), maximum);
  assert.equal(terminalClipboardText(`c;${Buffer.from(maximum + "x").toString("base64")}`), undefined);
});

test("OSC 52 ignores clipboard reads, other selections and malformed content", () => {
  for (const value of ["c;?", "p;eA==", "c", "c;%%%", "c;eA", "c;e===", "c;/w=="])
    assert.equal(terminalClipboardText(value), undefined, value);
});
