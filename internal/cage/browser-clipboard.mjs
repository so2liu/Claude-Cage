import { randomBytes } from "node:crypto";

// This runs before site scripts, in each new document's main world. The binding
// and token stay in the closure. Existing documents expose only captured
// function references to CDP for native-code verification before installation.
function prepareCopies(bindingName, token, contextSetter, limit, includeChanges) {
  const emit = globalThis[bindingName];
  delete globalThis[bindingName];
  if (typeof emit !== "function") return;
  const apply = Reflect.apply;
  const define = Object.defineProperty;
  const descriptor = Object.getOwnPropertyDescriptor;
  const stringify = JSON.stringify;
  const string = String;
  const then = Promise.prototype.then;
  const hasFocus = Document.prototype.hasFocus;
  const visibility = descriptor(Document.prototype, "visibilityState").get;
  const activated = descriptor(UserActivation.prototype, "isActive").get;
  const userActivation = descriptor(Navigator.prototype, "userActivation").get;
  const addListener = EventTarget.prototype.addEventListener;
  const prevented = descriptor(Event.prototype, "defaultPrevented").get;
  const clipboardData = descriptor(
    ClipboardEvent.prototype,
    "clipboardData",
  ).get;
  const blobText = Blob.prototype.text;
  const lower = String.prototype.toLowerCase;
  const exec = Document.prototype.execCommand;
  const setData = DataTransfer.prototype.setData;
  const clearData = DataTransfer.prototype.clearData;
  const writeText = globalThis.Clipboard?.prototype.writeText;
  const write = globalThis.Clipboard?.prototype.write;
  const iterator = Symbol.iterator;
  const getType = globalThis.ClipboardItem?.prototype.getType;
  const clipboardPrototype = globalThis.Clipboard?.prototype;
  const transferPrototype = DataTransfer.prototype;
  const documentPrototype = Document.prototype;
  const activeElement = descriptor(Document.prototype, "activeElement").get;
  const shadowActiveElement = descriptor(
    ShadowRoot.prototype,
    "activeElement",
  ).get;
  const shadowRoot = descriptor(Element.prototype, "shadowRoot").get;
  const tagName = descriptor(Element.prototype, "tagName").get;
  const inputType = descriptor(HTMLInputElement.prototype, "type").get;
  const inputStart = descriptor(
    HTMLInputElement.prototype,
    "selectionStart",
  ).get;
  const inputEnd = descriptor(HTMLInputElement.prototype, "selectionEnd").get;
  const textareaStart = descriptor(
    HTMLTextAreaElement.prototype,
    "selectionStart",
  ).get;
  const textareaEnd = descriptor(
    HTMLTextAreaElement.prototype,
    "selectionEnd",
  ).get;
  const getSelection = Document.prototype.getSelection;
  const selectionText = Selection.prototype.toString;
  const selectionCollapsed = descriptor(Selection.prototype, "isCollapsed").get;
  const schedule = globalThis.setTimeout;
  const nativeFunctions = {
    "": emit,
    apply,
    defineProperty: define,
    getOwnPropertyDescriptor: descriptor,
    stringify,
    String: string,
    then,
    hasFocus,
    "get visibilityState": visibility,
    "get isActive": activated,
    "get userActivation": userActivation,
    addEventListener: addListener,
    "get defaultPrevented": prevented,
    "get clipboardData": clipboardData,
    text: blobText,
    toLowerCase: lower,
    execCommand: exec,
    setData,
    clearData,
    Symbol,
    writeText,
    write,
    getType,
    "get activeElement": activeElement,
    "get activeElement#shadow": shadowActiveElement,
    "get shadowRoot": shadowRoot,
    "get tagName": tagName,
    "get type": inputType,
    "get selectionStart#input": inputStart,
    "get selectionEnd#input": inputEnd,
    "get selectionStart#textarea": textareaStart,
    "get selectionEnd#textarea": textareaEnd,
    getSelection,
    toString: selectionText,
    "get isCollapsed": selectionCollapsed,
    setTimeout: schedule,
  };
  const plain = (value) => {
    const type = apply(lower, value, []);
    return type === "text" || type === "text/plain";
  };
  return {
    nativeFunctions,
    install() {
      const activation = apply(userActivation, navigator, []);
      let epoch = 0;
      let operation;
      let context = "";
      // Only future copies use a newly supplied host interaction context.
      define(globalThis, contextSetter, {
        value: function (value) {
          if (typeof value === "string" && value.length <= 64) context = value;
        },
      });
      const current = () =>
        apply(hasFocus, document, []) &&
        apply(visibility, document, []) === "visible";
      const begin = () =>
        context && current() && apply(activated, activation, [])
          ? { epoch, context }
          : null;
      const focusedElement = () => {
        let element = apply(activeElement, document, []);
        let root;
        while (element && (root = apply(shadowRoot, element, [])))
          element = apply(shadowActiveElement, root, []);
        return element;
      };
      const selectedControlText = () => {
        const element = focusedElement();
        if (!element) return;
        const tag = apply(tagName, element, []);
        if (tag !== "TEXTAREA" && tag !== "INPUT") return;
        if (tag === "INPUT") {
          const type = apply(inputType, element, []);
          if (
            type !== "text" &&
            type !== "search" &&
            type !== "url" &&
            type !== "tel"
          )
            return;
        }
        const start = apply(
          tag === "INPUT" ? inputStart : textareaStart,
          element,
          [],
        );
        const end = apply(
          tag === "INPUT" ? inputEnd : textareaEnd,
          element,
          [],
        );
        if (start === null || end <= start) return;
        const selection = apply(getSelection, document, []);
        return selection && apply(selectionText, selection, []);
      };
      const hasCopySelection = () => {
        const element = focusedElement();
        if (!element) return false;
        const tag = apply(tagName, element, []);
        if (tag === "INPUT" || tag === "TEXTAREA") {
          if (tag === "INPUT") {
            const type = apply(inputType, element, []);
            if (type !== "text" && type !== "search" && type !== "url" && type !== "tel") return false;
          }
          const start = apply(tag === "INPUT" ? inputStart : textareaStart, element, []);
          const end = apply(tag === "INPUT" ? inputEnd : textareaEnd, element, []);
          return start !== null && end > start;
        }
        const selection = apply(getSelection, document, []);
        return selection && !apply(selectionCollapsed, selection, []);
      };
      const send = (text, started) => {
        if (
          started === null ||
          epoch !== started.epoch ||
          !current() ||
          !includeChanges && (typeof text !== "string" || text.length > limit)
        )
          return;
        const content = typeof text === "string" && text.length <= limit
          ? ',"text":' + stringify(text)
          : ',"changed":true';
        emit(
          '{"token":"' +
            token +
            '","context":' +
            stringify(started.context) +
            content +
            "}",
        );
      };
      const watch = (promise, text, started) => {
        // Observe the captured native promise without trusting site-modified then().
        apply(then, promise, [
          () => {
            send(text, started);
          },
          () => {},
        ]);
        return promise;
      };
      const cancel = () => {
        epoch++;
      };
      apply(addListener, window, ["blur", cancel, true]);
      apply(addListener, window, ["pagehide", cancel, true]);
      apply(addListener, document, [
        "visibilitychange",
        () => {
          epoch++;
        },
        true,
      ]);

      if (writeText) {
        define(clipboardPrototype, "writeText", {
          configurable: true,
          writable: true,
          value: function (value) {
            if (!arguments.length || typeof value === "symbol")
              return apply(writeText, this, arguments);
            const started = begin();
            // Pass the same converted string to Chromium and the observer.
            const text = string(value);
            return watch(apply(writeText, this, [text]), text, started);
          },
        });
        if (write && getType)
          define(clipboardPrototype, "write", {
            configurable: true,
            writable: true,
            value: function (value) {
              const started = begin();
              let items;
              try {
                items = [...value];
              } catch {
                return apply(write, this, arguments);
              }
              define(items, iterator, {
                value: function () {
                  let index = 0;
                  return {
                    next: () =>
                      index < items.length
                        ? { value: items[index++], done: false }
                        : { done: true },
                  };
                },
              });
              const promise = apply(write, this, [items]);
              if (started === null) return promise;
              if (includeChanges) return watch(promise, undefined, started);
              if (items.length !== 1) return promise;
              let text,
                written = false;
              const completed = () => {
                if (written && typeof text === "string") send(text, started);
              };
              try {
                apply(then, apply(getType, items[0], ["text/plain"]), [
                  (blob) => {
                    apply(then, apply(blobText, blob, []), [
                      (value) => {
                        text = value;
                        completed();
                      },
                      () => {},
                    ]);
                  },
                  () => {},
                ]);
              } catch {
                return promise;
              }
              apply(then, promise, [
                () => {
                  written = true;
                  completed();
                },
                () => {},
              ]);
              return promise;
            },
          });
      }

      define(transferPrototype, "setData", {
        configurable: true,
        writable: true,
        value: function (format, value) {
          if (
            arguments.length < 2 ||
            typeof format === "symbol" ||
            typeof value === "symbol"
          )
            return apply(setData, this, arguments);
          const type = string(format);
          const text = string(value);
          const result = apply(setData, this, [type, text]);
          if (operation?.data === this && plain(type)) operation.text = text;
          return result;
        },
      });
      define(transferPrototype, "clearData", {
        configurable: true,
        writable: true,
        value: function (format) {
          if (typeof format === "symbol")
            return apply(clearData, this, arguments);
          const type = format === undefined ? undefined : string(format);
          const result = apply(
            clearData,
            this,
            type === undefined ? [] : [type],
          );
          if (operation?.data === this && (type === undefined || plain(type)))
            operation.text = undefined;
          return result;
        },
      });
      const copied =
        (event) => {
          if (!event.isTrusted) return;
          if (operation && !operation.native) {
            operation.event = event;
            operation.data = apply(clipboardData, event, []);
          } else if (includeChanges) {
            const copy = { native: true, event, data: apply(clipboardData, event, []), started: begin(), selected: hasCopySelection() };
            operation = copy;
            // The default copy runs after the DOM event. A trusted no-selection
            // or password-field event must never export an older clipboard.
            apply(schedule, globalThis, [() => {
              if (operation === copy) operation = undefined;
              if (apply(prevented, event, [])) {
                if (typeof copy.text === "string") send(copy.text, copy.started);
              } else if (copy.selected) send(undefined, copy.started);
            }, 0]);
          }
        };
      apply(addListener, document, ["copy", copied, true]);
      if (includeChanges) apply(addListener, document, ["cut", copied, true]);
      define(documentPrototype, "execCommand", {
        configurable: true,
        writable: true,
        value: function (command, ...args) {
          if (!arguments.length || typeof command === "symbol")
            return apply(exec, this, arguments);
          const name = string(command);
          if (apply(lower, name, []) !== "copy" || this !== document)
            return apply(exec, this, [name, ...args]);
          const parent = operation;
          const copy = { started: begin(), selected: includeChanges && hasCopySelection() };
          operation = copy;
          let result;
          try {
            result = apply(exec, this, [name, ...args]);
          } finally {
            operation = parent;
          }
          if (result && copy.event) {
            if (apply(prevented, copy.event, [])) {
              if (typeof copy.text === "string") send(copy.text, copy.started);
            }
            else {
              // Chromium's default DOM copy may add image alt text or transform
              // whitespace. Text controls have an exact selection representation.
              const text = selectedControlText();
              if (text) send(text, copy.started);
              else if (copy.selected) send(undefined, copy.started);
            }
          }
          return result;
        },
      });
    },
  };
}

export class BrowserClipboard {
  constructor(cdp, onCopy, { includeChanges = false } = {}) {
    this.cdp = cdp;
    this.onCopy = onCopy;
    this.includeChanges = includeChanges;
    this.sessions = new Map();
    this.copyContexts = new Map();
  }

  async attach(sessionId, tabId) {
    if (this.sessions.has(sessionId)) return this.sessions.get(sessionId).ready;
    const state = {
      tabId,
      contexts: new Map(),
      name: "cageCopy" + randomBytes(16).toString("hex"),
      token: randomBytes(32).toString("hex"),
      contextSetter: "cageCopyContext" + randomBytes(16).toString("hex"),
    };
    this.sessions.set(sessionId, state);
    state.ready = this.#install(sessionId, state);
    return state.ready;
  }

  async #install(sessionId, state) {
    const { tabId } = state;
    try {
      await this.cdp("Page.enable", {}, sessionId);
      await this.cdp("Runtime.enable", {}, sessionId);
      await this.cdp("Runtime.addBinding", { name: state.name }, sessionId);
      const source = `(${prepareCopies})(${JSON.stringify(state.name)},${JSON.stringify(state.token)},${JSON.stringify(state.contextSetter)},65536,${this.includeChanges})`;
      await this.cdp(
        "Page.addScriptToEvaluateOnNewDocument",
        { source: `(${source})?.install()` },
        sessionId,
      );
      const skipped = [];
      for (const contextId of state.contexts.keys()) {
        const group = "cage-clipboard-" + randomBytes(8).toString("hex");
        try {
          const prepared = await this.cdp(
            "Runtime.evaluate",
            { expression: source, contextId, objectGroup: group },
            sessionId,
          );
          if (prepared.exceptionDetails) {
            skipped.push({
              contextId,
              reason: "Clipboard APIs unavailable or modified",
            });
            continue;
          }
          if (!prepared.result?.objectId) continue;
          const properties = await this.cdp(
            "Runtime.getProperties",
            { objectId: prepared.result.objectId, ownProperties: true },
            sessionId,
          );
          const functions = properties.result.find(
            (p) => p.name === "nativeFunctions",
          )?.value?.objectId;
          if (!functions) continue;
          const refs = await this.cdp(
            "Runtime.getProperties",
            { objectId: functions, ownProperties: true },
            sessionId,
          );
          const modified = refs.result.find(
            (p) =>
              !(
                ["writeText", "write", "getType"].includes(p.name) &&
                p.value?.type === "undefined"
              ) &&
              (p.value?.type !== "function" ||
                p.value.description !==
                  `function ${p.name.split("#", 1)[0]}() { [native code] }`),
          );
          if (modified) {
            skipped.push({
              contextId,
              reason: `Modified ${modified.name || "binding"}`,
            });
            continue;
          }
          const binding = await this.cdp(
            "Runtime.getProperties",
            {
              objectId: refs.result.find((p) => p.name === "").value.objectId,
              ownProperties: true,
            },
            sessionId,
          );
          if (
            binding.internalProperties?.some(
              (p) => p.name === "[[TargetFunction]]",
            )
          ) {
            skipped.push({ contextId, reason: "Modified binding" });
            continue;
          }
          const result = await this.cdp(
            "Runtime.callFunctionOn",
            {
              objectId: prepared.result.objectId,
              functionDeclaration: "function() { this.install(); }",
            },
            sessionId,
          );
          if (result.exceptionDetails)
            skipped.push({
              contextId,
              reason: "Clipboard observer installation failed",
            });
        } catch {
          skipped.push({
            contextId,
            reason: "Document changed during clipboard observer installation",
          });
        } finally {
          await this.cdp(
            "Runtime.releaseObjectGroup",
            { objectGroup: group },
            sessionId,
          ).catch(() => {});
        }
      }
      await this.cdp(
        "Target.setAutoAttach",
        {
          autoAttach: true,
          waitForDebuggerOnStart: false,
          flatten: true,
          filter: [{ type: "iframe" }, { exclude: true }],
        },
        sessionId,
      );
      if (this.copyContexts.has(tabId))
        await this.#applyContext(
          sessionId,
          state,
          this.copyContexts.get(tabId),
        );
      return { skipped };
    } catch (error) {
      this.sessions.delete(sessionId);
      throw error;
    }
  }

  async setContext(id, context) {
    if (typeof context !== "string" || context.length > 64)
      throw new Error("Invalid clipboard context");
    this.copyContexts.set(id, context);
    const applied = new Set();
    for (;;) {
      const sessions = [...this.sessions].filter(
        ([, state]) => state.tabId === id && !applied.has(state),
      );
      if (!sessions.length) return;
      await Promise.all(
        sessions.map(async ([sessionId, state]) => {
          await state.ready;
          if (this.sessions.get(sessionId) === state)
            await this.#applyContext(sessionId, state, context);
          applied.add(state);
        }),
      );
      // Auto-attach may discover nested frames while an earlier frame installs.
    }
  }

  async #applyContext(sessionId, state, context) {
    const updates = [];
    for (const contextId of state.contexts.keys()) {
      updates.push(
        this.cdp(
          "Runtime.evaluate",
          {
            expression: `globalThis[${JSON.stringify(state.contextSetter)}]?.(${JSON.stringify(context)})`,
            contextId,
          },
          sessionId,
        ).catch(() => {}),
      );
    }
    await Promise.all(updates);
  }

  event(event) {
    const state = this.sessions.get(event.sessionId);
    if (!state) return;
    const p = event.params || {};
    if (
      event.method === "Target.attachedToTarget" &&
      p.targetInfo?.type === "iframe"
    ) {
      void this.attach(p.sessionId, state.tabId).catch(() => {});
    } else if (event.method === "Target.detachedFromTarget") {
      this.detachSession(p.sessionId);
    } else if (event.method === "Runtime.executionContextCreated") {
      if (p.context?.auxData?.isDefault)
        state.contexts.set(p.context.id, p.context.auxData.frameId);
    } else if (event.method === "Runtime.executionContextDestroyed") {
      state.contexts.delete(p.executionContextId);
    } else if (event.method === "Runtime.executionContextsCleared") {
      state.contexts.clear();
    } else if (
      event.method === "Page.frameDetached" ||
      event.method === "Page.frameNavigated"
    ) {
      const id = p.frameId || p.frame?.id;
      for (const [context, frame] of state.contexts)
        if (frame === id) state.contexts.delete(context);
    } else if (
      event.method === "Runtime.bindingCalled" &&
      p.name === state.name &&
      state.contexts.has(p.executionContextId)
    ) {
      if (
        typeof p.payload !== "string" ||
        p.payload.length > 6 * (65536 + 64) + 200
      )
        return;
      let value;
      try {
        value = JSON.parse(p.payload);
      } catch {
        return;
      }
      if (
        value?.token !== state.token ||
        typeof value.context !== "string" ||
        value.context.length > 64 ||
        !(typeof value.text === "string" && value.text.length <= 65536 || this.includeChanges && value.changed === true && value.text === undefined)
      )
        return;
      this.onCopy({
        id: state.tabId,
        ...(typeof value.text === "string" ? { text: value.text } : { changed: true }),
        context: value.context,
      });
    }
  }

  detachSession(id) {
    this.sessions.delete(id);
  }

  detachTab(id) {
    this.copyContexts.delete(id);
    for (const [session, state] of this.sessions)
      if (state.tabId === id) this.sessions.delete(session);
  }
}
