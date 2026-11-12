import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";
import { webcrypto } from "node:crypto";
import { caretPollDelay, caretPollFast, caretFreshFor } from "./caret-poll.mjs";
const source = readFileSync(
  new URL("./remote-input.js", import.meta.url),
  "utf8",
)
  .replace(/^import[\s\S]*?;\n/gm, "")
  .replace("export function installRemoteInput", "function installRemoteInput");
function fixture(native = false, browserComposition = false, options = {}) {
  const doc = new EventTarget(),
    win = new EventTarget(),
    canvas = {},
    local = {},
    keys = [],
    pastes = [],
    texts = [],
    compositions = [],
    copyContexts = [],
    writes = [],
    statuses = [];
  let reads = 0,
    mode = "mac",
    currentTabId = "tab-1",
    guestPlatform = "linux",
    systemClipboard = true;
  doc.activeElement = canvas;
  doc.hasFocus = () => true;
  doc.body = { append() {} };
  const target = Object.assign(new EventTarget(), {
    value: "",
    style: {},
    classList: { add() {}, remove() {} },
    setAttribute() {},
    focus() {
      doc.activeElement = this;
    },
    remove() {},
  });
  doc.createElement = () => target;
  win.innerWidth = 1280;
  win.innerHeight = 800;
  const rfb = new EventTarget();
  rfb.setKeyboardTarget = (element) => {
    rfb.target = element;
  };
  rfb.events = rfb;
  rfb.sendKey = (...args) => keys.push(args);
  rfb.runKeyAction = action => Promise.resolve().then(() => action(rfb.sendKey));
  rfb.focus = () => {
    doc.activeElement = rfb.target || canvas;
  };
  rfb.pasteText = (text) => {
    pastes.push(text);
    return Promise.resolve();
  };
  const context = {
    document: doc,
    window: win,
    setTimeout: options.setTimeout || setTimeout,
    clearTimeout: options.clearTimeout || clearTimeout,
    Date: options.Date || Date,
    caretPollDelay,
    caretPollFast,
    caretFreshFor,
    Promise,
    Map,
    Set,
    console,
    crypto: webcrypto,
    TextEncoder,
    Blob,
    Uint8Array,
    atob,
    ClipboardItem: class { constructor(values) { this.values = values; } },
    navigator: { clipboard: { write: async values => writes.push(values) } },
    api: options.api || (async () => { throw new Error("unexpected API request"); }),
    native: options.native || (async (operation, value) => {
      if (operation === "clipboardRevision") return 1;
      if (operation === "clipboardSyncWrite") { writes.push(value.text || value.png); return { revision: 2 }; }
      return true;
    }),
    readClipboardContent: async () => {
      reads++;
      options.onRead?.();
      return { text: "中文🙂" };
    },
    transferFiles: (data) => Array.from(data?.files || []),
    hasFiles: (data) => Array.from(data?.types || []).includes("Files"),
    imagePayload: async () => ({ png: "fixture-image" }),
    writeClipboard: async (text) => writes.push(text),
    nativeAvailable: () => native,
    getKeycode: (e) => e.code,
    getKeysym: (e) =>
      ({ Shift: e.code === "ShiftRight" ? 0xffe2 : 0xffe1, Control: 0xffe3, Alt: 0xffe9, Meta: 0xffeb, Enter: 0xff0d })[e.key] ??
      (e.key.length === 1 ? e.key.codePointAt(0) : null),
    Keysyms: {
      lookup: (codepoint) =>
        codepoint <= 255 ? codepoint : 0x01000000 | codepoint,
    },
  };
  const install = vm.runInNewContext(source + "\ninstallRemoteInput;", context);
  const container = Object.assign(new EventTarget(), {
      querySelector: () => options.surface || null,
      contains: (element) => element === canvas || element === target,
      append() {},
      getBoundingClientRect: () => ({ left: 0, top: 0 }),
    });
  const controller = install(
    rfb,
    container,
    {
      mode: () => mode,
      guestPlatform: () => guestPlatform,
      systemClipboard: () => systemClipboard,
      preparePaste: options.preparePaste,
      copyText: options.copyText,
      onStatus: (t) => statuses.push(t),
      onFallback() {},
      isActive: () => true,
      setCopyContext: async (context, valid) => { if (valid()) copyContexts.push(context); },
      insertText: async (text) => {
        texts.push(text);
      },
      insertComposition: browserComposition ? async (id, value) => {
        if (id !== currentTabId) throw new Error("Input target changed");
        compositions.push({ id, ...value });
      } : null,
      tabId: () => currentTabId,
      caretRect: options.caretRect || (browserComposition ? async () => ({
        available: true, context: "a".repeat(32), rect: [240, 120, 0, 24],
      }) : null),
      pasteContent: async (content) => {
        const value = await content;
        if (rfb.target) pastes.push(value.png || value.text);
      },
    },
  );
  const key = (type, key, code, extra = {}) => {
    const event = new Event(type, { cancelable: true });
    Object.assign(event, {
      key,
      code,
      metaKey: false,
      ctrlKey: false,
      shiftKey: false,
      altKey: false,
      ...extra,
    });
    doc.dispatchEvent(event);
    return event;
  };
  return {
    container,
    doc,
    win,
    rfb,
    keys,
    pastes,
    texts,
    compositions,
    copyContexts,
    writes,
    statuses,
    armCopy: () => {
      const event = new Event("pointerdown");
      Object.defineProperty(event, "isTrusted", { value: true });
      Object.assign(event, { button: 0, clientX: 20, clientY: 20 });
      container.dispatchEvent(event);
      return copyContexts.at(-1);
    },
    controller,
    key,
    local,
    target,
    setMode: (value) => {
      mode = value;
    },
    setGuestPlatform: value => { guestPlatform = value; },
    setSystemClipboard: value => { systemClipboard = value; },
    setTabId: (value) => {
      currentTabId = value;
    },
    reads: () => reads,
  };
}
for (const guest of ["linux", "windows", "darwin"]) {
  test(`${guest}: Shift+Enter restores a modifier missed during focus changes`, () => {
    const f = fixture();
    f.setGuestPlatform(guest);
    try {
      assert.equal(f.key("keydown", "Enter", "Enter", { shiftKey: true }).defaultPrevented, true);
      f.key("keyup", "Enter", "Enter", { shiftKey: true });
      assert.deepEqual(f.keys, [
        [0xffe1, "ShiftLeft", true], [0xff0d, "Enter", true],
        [0xff0d, "Enter", false], [0xffe1, "ShiftLeft", false],
      ]);
      f.keys.length = 0;
      f.key("keydown", "Enter", "Enter", { shiftKey: true, isComposing: true });
      assert.deepEqual(f.keys, [], "IME confirmation must not insert a newline");
    } finally { f.controller.dispose(); }
  });
  for (const code of ["ShiftLeft", "ShiftRight"]) {
    test(`${guest}: Shift+Enter keeps held ${code} until its release`, () => {
      const f = fixture();
      f.setGuestPlatform(guest);
      try {
        const sym = code === "ShiftRight" ? 0xffe2 : 0xffe1;
        f.key("keydown", "Shift", code, { shiftKey: true });
        f.key("keydown", "Enter", "Enter", { shiftKey: true });
        f.key("keyup", "Enter", "Enter", { shiftKey: true });
        assert.ok(!f.keys.some(([key, , down]) => key === sym && !down));
        f.key("keyup", "Shift", code);
        assert.deepEqual(f.keys.at(-1), [sym, code, false]);
        f.keys.length = 0;
        f.win.dispatchEvent(new Event("blur"));
        assert.deepEqual(f.keys, [], "released Shift must not remain held remotely");
      } finally { f.controller.dispose(); }
    });
  }
}

test("Mac find and redo preserve the Linux Ctrl/Shift chord", () => {
  const f = fixture();
  try {
    assert.equal(
      f.key("keydown", "f", "KeyF", { metaKey: true }).defaultPrevented,
      true,
    );
    assert.deepEqual(
      f.keys.map((k) => [k[0], k[2]]),
      [
        [0xffe3, true],
        [102, true],
        [102, false],
        [0xffe3, false],
      ],
    );
    f.keys.length = 0;
    f.key("keydown", "\0", "KeyZ", { metaKey: true, shiftKey: true });
    assert.deepEqual(
      f.keys.map((k) => [k[0], k[2]]),
      [
        [0xffe3, true],
        [0xffe1, true],
        [90, true],
        [90, false],
        [0xffe1, false],
        [0xffe3, false],
      ],
    );
  } finally {
    f.controller.dispose();
  }
});

test("Mac guests use Command for edit actions and preserve physical Control", () => {
  const f = fixture();
  f.setGuestPlatform("darwin");
  try {
    f.key("keydown", "f", "KeyF", { metaKey: true });
    assert.deepEqual(f.keys.map(([symbol, , down]) => [symbol, down]), [[0xffeb, true], [102, true], [102, false], [0xffeb, false]]);
    f.keys.length = 0;
    f.controller.edit("selectAll");
    assert.deepEqual(f.keys.map(([symbol, , down]) => [symbol, down]), [[0xffeb, true], [97, true], [97, false], [0xffeb, false]]);
    f.keys.length = 0;
    f.key("keydown", "Control", "ControlLeft", { ctrlKey: true });
    f.key("keydown", "a", "KeyA", { ctrlKey: true });
    f.key("keyup", "a", "KeyA", { ctrlKey: true });
    f.key("keyup", "Control", "ControlLeft");
    assert.deepEqual(f.keys.map(([symbol, , down]) => [symbol, down]), [[0xffe3, true], [97, true], [97, false], [0xffe3, false]]);
    f.keys.length = 0;
    f.key("keydown", "c", "KeyC", { ctrlKey: true });
    assert.deepEqual(f.keys.map(([symbol, , down]) => [symbol, down]), [[99, true]]);
  } finally { f.controller.dispose(); }
});

test("a private browser clipboard uses its surface path while OS sharing is active", async () => {
  const order = [];
  const f = fixture(true, false, {
    preparePaste: () => { order.push("prepare"); return { dispose() { order.push("dispose"); } }; },
    onRead: () => order.push("read"),
  });
  f.setGuestPlatform("darwin");
  f.setSystemClipboard(false);
  f.win.cageSharedClipboardActive = true;
  try {
    await f.controller.readAndPaste();
    assert.equal(f.reads(), 1);
    assert.deepEqual(order, ["prepare", "read", "dispose"]);
    f.doc.visibilityState = "visible";
    const context = f.armCopy();
    await f.controller.receiveCopy({ id: "tab-1", text: "private browser copy", context });
    assert.deepEqual(f.writes, ["private browser copy"]);
  } finally { f.controller.dispose(); }
});

test("shared clipboard input skips fallback copy setup and resumes it after disconnect", async () => {
  const nativeCalls = [];
  const f = fixture(true, false, {
    native: async operation => { nativeCalls.push(operation); return 1; },
  });
  try {
    const beforeSharing = f.armCopy();
    assert.ok(beforeSharing);
    f.win.cageSharedClipboardActive = true;
    nativeCalls.length = 0;
    f.armCopy();
    for (const [key, code] of [[" ", "Space"], ["Enter", "Enter"], ["Tab", "Tab"]]) {
      f.key("keydown", key, code, { cageNativeInput: true });
    }
    assert.deepEqual(nativeCalls, []);
    assert.deepEqual(f.copyContexts, [beforeSharing]);
    f.win.cageSharedClipboardActive = false;
    await f.controller.receiveCopy({ id: "tab-1", text: "obsolete", context: beforeSharing });
    assert.deepEqual(nativeCalls, []);
    assert.notEqual(f.armCopy(), beforeSharing);
    assert.deepEqual(nativeCalls, ["clipboardRevision"]);
  } finally { f.controller.dispose(); }
});

test("a right Command shortcut never leaves an extra left Command pressed", () => {
  const f = fixture();
  f.setGuestPlatform("macos");
  try {
    f.key("keydown", "Meta", "MetaRight", { metaKey: true });
    f.key("keydown", "f", "KeyF", { metaKey: true });
    f.key("keyup", "f", "KeyF", { metaKey: true });
    f.key("keyup", "Meta", "MetaRight");
    assert.equal(f.keys.some(([, code]) => code === "MetaLeft"), false);
    assert.deepEqual(f.keys.at(-1).slice(1), ["MetaRight", false]);
  } finally { f.controller.dispose(); }
});

function inputEvent(f, type, properties = {}) {
  const event = new Event(type, { cancelable: true });
  Object.assign(event, properties);
  f.target.dispatchEvent(event);
  return event;
}

test("host IME receives composition keys and sends only committed Unicode in order", () => {
  const f = fixture();
  try {
    f.rfb.focus();
    assert.equal(f.doc.activeElement, f.target);
    assert.equal(
      f.key("keydown", "Process", "KeyN", { keyCode: 229 }).defaultPrevented,
      false,
    );
    inputEvent(f, "compositionstart");
    f.target.value = "nihao";
    inputEvent(f, "input", {
      data: "nihao",
      isComposing: true,
      inputType: "insertCompositionText",
    });
    assert.equal(
      f.key("keydown", " ", "Space", { isComposing: true }).defaultPrevented,
      false,
    );
    assert.deepEqual(f.keys, []);
    inputEvent(f, "compositionend", { data: "你好🙂" });
    // Safari sends a final input after compositionend; Chromium sends it before.
    f.target.value = "你好🙂";
    inputEvent(f, "input", {
      data: "你好🙂",
      inputType: "insertFromComposition",
    });
    assert.deepEqual(f.keys, []);
    assert.equal(f.target.value, "");
    assert.deepEqual(f.texts, ["你好🙂"]);
    assert.equal(f.reads(), 0);
    inputEvent(f, "compositionstart");
    inputEvent(f, "compositionend", { data: "你好" });
    assert.equal(
      f.texts.length,
      2,
      "identical consecutive commits are not deduplicated",
    );
  } finally {
    f.controller.dispose();
  }
  assert.equal(f.rfb.target, null);
});

test("browser composition is forwarded as preedit, then committed separately", async () => {
  const f = fixture(false, true);
  await new Promise((resolve) => setTimeout(resolve, 5));
  try {
    f.rfb.focus();
    inputEvent(f, "compositionstart");
    f.target.value = "nihao";
    inputEvent(f, "compositionupdate", { data: "nihao" });
    inputEvent(f, "input", { data: "nihao", inputType: "insertCompositionText", isComposing: true });
    await Promise.resolve();
    inputEvent(f, "compositionend", { data: "你好" });
    await new Promise((resolve) => setImmediate(resolve));
    assert.deepEqual(f.texts, []);
    assert.equal(f.compositions[0].mode, "begin");
    assert.equal(new Set(f.compositions.map(value => value.session)).size, 1);
    assert.deepEqual(f.compositions.map(value => value.sequence), [0, 1, 2]);
    assert.deepEqual(JSON.parse(JSON.stringify(f.compositions.slice(1).map(({session, sequence, ...value}) => value))), [
      { id: "tab-1", mode: "update", text: "nihao", selectionStart: 5, selectionEnd: 5 },
      { id: "tab-1", mode: "commit", text: "你好", selectionStart: 0, selectionEnd: 0 },
    ]);
  } finally {
    f.controller.dispose();
  }
});

// Timers and the clock advance together, one due timer at a time.
function fakeClock() {
  let now = 1_000_000, sequence = 0;
  const timers = new Map();
  return {
    Date: { now: () => now },
    setTimeout: (run, delay = 0) => { timers.set(++sequence, { at: now + delay, run }); return sequence; },
    clearTimeout: (id) => { timers.delete(id); },
    async advance(duration) {
      const end = now + duration;
      for (;;) {
        const due = [...timers].filter(([, timer]) => timer.at <= end).sort((a, b) => a[1].at - b[1].at || a[0] - b[0])[0];
        if (!due) break;
        timers.delete(due[0]);
        now = due[1].at;
        due[1].run();
        for (let turn = 0; turn < 8; turn++) await Promise.resolve();
      }
      now = end;
    },
  };
}

test("caret readings back off while the viewer is quiet and resume with input", async () => {
  const clock = fakeClock();
  let readings = 0;
  const f = fixture(false, true, { ...clock, caretRect: async () => {
    readings++;
    return { available: true, context: "a".repeat(32), rect: [240, 120, 0, 24] };
  } });
  try {
    f.rfb.focus();
    await clock.advance(2900);
    assert.equal(readings, 12);
    readings = 0;
    // The earlier fixed interval read the caret 240 times in this minute.
    await clock.advance(60000);
    assert.ok(readings >= 30 && readings <= 50, `${readings} readings`);
    readings = 0;
    await clock.advance(1900);
    assert.equal(readings, 1);
    // Input reschedules a slow wait instead of waiting it out.
    f.key("keydown", "a", "KeyA");
    readings = 0;
    await clock.advance(250);
    assert.equal(readings, 1);
    await clock.advance(1000);
    assert.equal(readings, 5);
  } finally {
    f.controller.dispose();
  }
});

test("a composition begins from the last reading of a quiet viewer", async () => {
  const clock = fakeClock();
  const f = fixture(false, true, clock);
  try {
    f.rfb.focus();
    await clock.advance(60000);
    // Just before the next slow reading, the latest one is two seconds old.
    await clock.advance(1900);
    inputEvent(f, "compositionstart");
    assert.equal(f.compositions[0]?.mode, "begin");
    assert.equal(f.compositions[0].context, "a".repeat(32));
  } finally {
    f.controller.dispose();
  }
});

test("queued composition stays bound to its original browser tab", async () => {
  const f = fixture(false, true);
  await new Promise((resolve) => setTimeout(resolve, 5));
  f.rfb.focus();
  inputEvent(f, "compositionstart");
  f.target.value = "nihao";
  inputEvent(f, "input", { data: "nihao", inputType: "insertCompositionText", isComposing: true });
  f.setTabId("tab-2");
  inputEvent(f, "compositionend", { data: "你好" });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(f.compositions.map(value => value.mode), ["begin", "update"]);
  assert.ok(f.statuses.includes("Input target changed"));
  f.controller.dispose();
});

test("cancelled and disconnected compositions do not insert text", () => {
  const f = fixture();
  f.rfb.focus();
  inputEvent(f, "compositionstart");
  inputEvent(f, "compositionend", { data: "" });
  assert.deepEqual(f.keys, []);
  inputEvent(f, "compositionstart");
  f.controller.dispose();
  inputEvent(f, "compositionend", { data: "迟到的文本" });
  assert.deepEqual(f.keys, []);
  assert.deepEqual(f.texts, []);
});

test("text input without a composition supports emoji and clears the host buffer", () => {
  const f = fixture();
  try {
    f.rfb.focus();
    f.target.value = "🙂";
    inputEvent(f, "input", { data: "🙂", inputType: "insertText" });
    assert.deepEqual(f.keys, []);
    assert.deepEqual(f.texts, ["🙂"]);
    assert.equal(f.target.value, "");
  } finally {
    f.controller.dispose();
  }
});

test("fullwidth punctuation without composition uses text, while host punctuation keeps its default action", () => {
  const f = fixture();
  try {
    f.rfb.focus();
    for (const character of ["？", "？", "，", "。", "！", "🙂"]) {
      assert.equal(f.key("keydown", character, "Slash", { shiftKey: true }).defaultPrevented, true);
      f.key("keyup", character, "Slash");
    }
    assert.deepEqual(f.texts, ["？", "？", "，", "。", "！", "🙂"]);
    assert.deepEqual(f.keys, []);
    assert.equal(f.key("keydown", "?", "Slash", { shiftKey: true }).defaultPrevented, false);
    f.target.value = "？";
    inputEvent(f, "input", { data: "？", inputType: "insertText" });
    assert.equal(f.texts.at(-1), "？");
    assert.equal(f.key("keydown", "?", "Slash", { ctrlKey: true }).defaultPrevented, false);
  } finally { f.controller.dispose(); }
});
test("local fields and native keyboard mode do not acquire mapped shortcuts", () => {
  const f = fixture();
  try {
    f.doc.activeElement = f.local;
    assert.equal(
      f.key("keydown", "f", "KeyF", { metaKey: true }).defaultPrevented,
      false,
    );
    assert.equal(f.keys.length, 0);
    f.rfb.focus();
    f.setMode("native");
    f.key("keydown", "f", "KeyF", { metaKey: true });
    assert.equal(f.keys.length, 0);
  } finally {
    f.controller.dispose();
  }
});
test("background clipboard notifications never write to the host", async () => {
  const f = fixture();
  try {
    const clipboard = new Event("clipboard");
    clipboard.detail = { text: "guest text" };
    f.rfb.dispatchEvent(clipboard);
    await Promise.resolve();
    assert.equal(f.reads(), 0);
    assert.deepEqual(f.writes, []);
    f.key("keydown", "c", "KeyC", { metaKey: true });
    f.rfb.dispatchEvent(clipboard);
    await new Promise(resolve => setImmediate(resolve));
    assert.deepEqual(f.writes, ["guest text"]);
  } finally {
    f.controller.dispose();
  }
});
test("native menu paste accepts every rapid gesture, without debouncing", async () => {
  const f = fixture(true);
  try {
    for (let i = 0; i < 10; i++)
      f.key("keydown", "v", "KeyV", { metaKey: true });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(f.reads(), 10);
    assert.equal(f.pastes.length, 10);
    assert.ok(f.pastes.every((t) => t === "中文🙂"));
  } finally {
    f.controller.dispose();
  }
});
test("mapped modifier is released even after focus is lost", () => {
  const f = fixture();
  try {
    f.key("keydown", "Shift", "ShiftLeft", { metaKey: true, shiftKey: true });
    f.doc.activeElement = f.local;
    f.win.dispatchEvent(new Event("blur"));
    assert.deepEqual(f.keys, [
      [0xffe1, "ShiftLeft", true],
      [0xffe1, "ShiftLeft", false],
    ]);
  } finally {
    f.controller.dispose();
  }
});

test("a clipboard read finishing after disconnect is not pasted into a stale connection", async () => {
  const f = fixture(true);
  f.controller.readAndPaste();
  f.controller.dispose();
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(f.pastes.length, 0);
});

test("verified page copies only write to the host from the focused live tab", async () => {
  const f = fixture();
  const context = f.armCopy();
  const copy = (id = "tab-1", text = "实际复制的文字🙂") => f.controller.receiveCopy({ id, text, context });
  try {
    await copy();
    assert.deepEqual(f.writes, ["实际复制的文字🙂"]);
    await copy("other-tab");
    f.doc.hasFocus = () => false;
    await copy();
    f.doc.hasFocus = () => true;
    f.doc.visibilityState = "hidden";
    await copy();
    f.doc.visibilityState = "visible";
    f.doc.activeElement = f.local;
    await copy();
    f.doc.activeElement = f.target;
    await copy("tab-1", "x".repeat(2 * 1024 * 1024 + 1));
    assert.equal(f.writes.length, 1);
    f.controller.dispose();
    await copy();
    assert.equal(f.writes.length, 1);
  } finally { f.controller.dispose(); }
});

test("copy results from before a focus or tab round-trip cannot write after returning", async () => {
  const f = fixture();
  try {
    const context = f.armCopy();
    const oldCopy = { id: "tab-1", text: "old pending copy", context };
    f.win.dispatchEvent(new Event("blur"));
    const next = f.armCopy();
    assert.notEqual(context, next);
    await f.controller.receiveCopy(oldCopy);
    f.setTabId("tab-2");
    f.controller.cancelCopy();
    f.setTabId("tab-1");
    const latest = f.armCopy();
    await f.controller.receiveCopy({ ...oldCopy, context: next });
    assert.equal(f.writes.length, 0);
    await f.controller.receiveCopy({ ...oldCopy, text: "new copy", context: latest });
    assert.deepEqual(f.writes, ["new copy"]);
  } finally { f.controller.dispose(); }
});

test("native copied PNG uses the host revision captured by its user gesture", async () => {
  const calls = [];
  let revision = 7;
  const f = fixture(true, false, {
    api: async (path, body) => { calls.push([path, body.id]); revision = 8; return { png: "aW1hZ2U=" }; },
    native: async (operation, value) => {
      if (operation === "clipboardRevision") return revision;
      calls.push([operation, value.revision]);
      return { superseded: value.revision !== revision };
    },
  });
  try {
    const context = f.armCopy();
    await f.controller.receiveCopy({ id: "tab-1", context, document: "page-a", clipboard: { id: "opaque-id", mime: "image/png", size: 5 } });
    assert.deepEqual(calls, [["/browser/clipboard", "opaque-id"], ["clipboardSyncWrite", 7]]);
    assert.ok(f.statuses.at(-1).includes("剪贴板已更新"));
  } finally { f.controller.dispose(); }
});

test("a PNG fetched after a focus change cannot write the host clipboard", async () => {
  let finish;
  const f = fixture(true, false, { api: () => new Promise(resolve => { finish = resolve; }) });
  try {
    const context = f.armCopy();
    const pending = f.controller.receiveCopy({ id: "tab-1", context, document: "page-a", clipboard: { id: "opaque-id", mime: "image/png", size: 5 } });
    await new Promise(resolve => setImmediate(resolve));
    f.win.dispatchEvent(new Event("blur"));
    finish({ png: "aW1hZ2U=" });
    await pending;
    assert.deepEqual(f.writes, []);
  } finally { f.controller.dispose(); }
});

test("a private browser cut uses its callback without a duplicate key chord", async () => {
  const actions = [];
  const f = fixture(false, false, {
    copyText: async (_sendCopy, valid, action) => { assert.equal(valid(), true); actions.push(action.key); return { text: "cut result" }; },
  });
  f.setGuestPlatform("darwin");
  f.setSystemClipboard(false);
  try {
    await f.controller.copy("x");
    assert.deepEqual(actions, ["x"]);
    assert.deepEqual(f.keys, []);
    assert.deepEqual(f.writes, ["cut result"]);
  } finally { f.controller.dispose(); }
});

test("explicit desktop copy preserves the queued native copy keys", async () => {
  const f = fixture(false, false, { copyText: async sendCopy => { await sendCopy(); return { text: "fresh desktop text" }; } });
  f.setGuestPlatform("darwin");
  try {
    await f.controller.copy();
    assert.deepEqual(f.keys.map(([symbol, , down]) => [symbol, down]), [[0xffeb, true], [99, true], [99, false], [0xffeb, false]]);
    assert.deepEqual(f.writes, ["fresh desktop text"]);
  } finally { f.controller.dispose(); }
});

test("a late browser capability can switch copying from native events to its private adapter", async () => {
  let available = false;
  const f = fixture(false, false, { copyText: () => available ? Promise.resolve({ text: "private copy" }) : undefined });
  try {
    await f.controller.copy();
    assert.equal(f.keys.length, 4);
    const event = new Event("clipboard");
    event.detail = { text: "native event" };
    f.rfb.dispatchEvent(event);
    await new Promise(resolve => setImmediate(resolve));
    assert.deepEqual(f.writes, ["native event"]);
    available = true;
    f.keys.length = 0;
    await f.controller.copy();
    assert.deepEqual(f.keys, []);
    assert.deepEqual(f.writes, ["native event", "private copy"]);
  } finally { f.controller.dispose(); }
});

test("copied text supports two MiB in UTF-8 and rejects larger content", async () => {
  const f = fixture();
  try {
    const context = f.armCopy();
    const text = "界".repeat(Math.floor(2 * 1024 * 1024 / 3));
    await f.controller.receiveCopy({ id: "tab-1", context, text });
    assert.deepEqual(f.writes, [text]);
    await f.controller.receiveCopy({ id: "tab-1", context, text: text + "界" });
    assert.equal(f.writes.length, 1);
  } finally { f.controller.dispose(); }
});

test("clicking between split panes rejects earlier copies after returning", async () => {
  const f = fixture();
  try {
    const leftContext = f.armCopy();
    f.setTabId("tab-2");
    const rightContext = f.armCopy();
    f.setTabId("tab-1");
    const currentContext = f.armCopy();

    // Both panes share one input surface, so these clicks do not cause blur.
    await f.controller.receiveCopy({ id: "tab-1", text: "old left copy", context: leftContext });
    await f.controller.receiveCopy({ id: "tab-2", text: "old right copy", context: rightContext });
    assert.deepEqual(f.writes, []);
    await f.controller.receiveCopy({ id: "tab-1", text: "current left copy", context: currentContext });
    assert.deepEqual(f.writes, ["current left copy"]);
  } finally { f.controller.dispose(); }
});

test("an unrelated RFB clipboard update never authorizes a host write", async () => {
  const f = fixture();
  try {
    const event = new Event("clipboard");
    event.detail = { text: "unrelated guest application" };
    f.rfb.dispatchEvent(event);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(f.writes.length, 0);
  } finally { f.controller.dispose(); }
});

test("malformed native Command letters retain clipboard and redo semantics", async () => {
  const f = fixture(true);
  try {
    f.key("keydown", "\u0016", "KeyV", { metaKey: true });
    await new Promise((resolve) => setImmediate(resolve));
    assert.equal(f.reads(), 1);
    assert.equal(f.pastes.length, 1);
    assert.equal(f.keys.length, 0);
    for (const letter of ["\ufffd", "\uf700", "Ω"]) {
      f.keys.length = 0;
      f.key("keydown", letter, "KeyZ", { metaKey: true, shiftKey: true });
      assert.deepEqual(
        f.keys.map((k) => [k[0], k[2]]),
        [
          [0xffe3, true],
          [0xffe1, true],
          [90, true],
          [90, false],
          [0xffe1, false],
          [0xffe3, false],
        ],
      );
    }
  } finally {
    f.controller.dispose();
  }
});

test('remote hover shape updates even without an editable focus and rejects arbitrary CSS', async () => {
  const changes = [];
  let cursor = 'text';
  const f = fixture(false, false, {
    surface: { style: { setProperty: (...args) => changes.push(args) } },
    caretRect: async () => ({ available: false, cursor }),
  });
  try {
    await new Promise(resolve => setTimeout(resolve, 20));
    assert.deepEqual(changes.at(-1), ['cursor', 'text', 'important']);
    cursor = 'url(https://invalid.example/cursor)';
    const before = changes.length;
    await new Promise(resolve => setTimeout(resolve, 280));
    assert.equal(changes.length, before);
    cursor = 'default';
    await new Promise(resolve => setTimeout(resolve, 280));
    assert.deepEqual(changes.at(-1), ['cursor', 'default', 'important']);
  } finally { f.controller.dispose(); }
});

for (const platform of ["windows", "windows-11"]) {
test(platform + ' hover after toolbar focus refreshes cursor without activating native IME', async () => {
  const changes = [], calls = [];
  let cursor = 'default';
  const f = fixture(true, false, {
    surface: { style: { setProperty: (...args) => changes.push(args) } },
    caretRect: async () => ({ available: true, cursor, rect: [20, 20, 1, 20] }),
    native: async (...args) => { calls.push(args); return true; },
  });
  f.setGuestPlatform(platform);
  try {
    f.doc.activeElement = {};
    f.win.cageNativeIMEAvailable = true;
    f.container.dispatchEvent(new Event('pointermove'));
    await new Promise(resolve => setTimeout(resolve, 20));
    assert.deepEqual(changes.at(-1), ['cursor', 'default', 'important']);
    cursor = 'text';
    f.container.dispatchEvent(new Event('pointermove'));
    await new Promise(resolve => setTimeout(resolve, 280));
    assert.deepEqual(changes.at(-1), ['cursor', 'text', 'important']);
    assert.equal(calls.filter(([name, value]) => name === 'remoteInput' && value.operation === 'activate').length, 0);
  } finally { f.controller.dispose(); }
});
}

for (const platform of ["linux", "darwin"]) {
  test(platform + " toolbar hover makes no cursor query and keeps focused IME polling", async () => {
    const clock = fakeClock();
    let readings = 0;
    const f = fixture(false, true, { ...clock, caretRect: async () => {
      readings++;
      return { available: true, context: "a".repeat(32) };
    } });
    f.setGuestPlatform(platform);
    try {
      f.doc.activeElement = f.local;
      await clock.advance(1000);
      const before = readings;
      f.container.dispatchEvent(new Event("pointermove"));
      await clock.advance(1000);
      assert.equal(readings, before);
      f.rfb.focus();
      await clock.advance(2500);
      assert.ok(readings > before);
    } finally { f.controller.dispose(); }
  });
}
