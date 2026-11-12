import React, { useEffect, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { ArrowLeft, Maximize, ZoomIn } from "lucide-react";
import { api } from "./api";
import "./macos-login.css";

// DOM physical codes and the matching ANSI key codes expected by the Apple VM keyboard.
const keys = {
  KeyA: 0,
  KeyS: 1,
  KeyD: 2,
  KeyF: 3,
  KeyH: 4,
  KeyG: 5,
  KeyZ: 6,
  KeyX: 7,
  KeyC: 8,
  KeyV: 9,
  KeyB: 11,
  KeyQ: 12,
  KeyW: 13,
  KeyE: 14,
  KeyR: 15,
  KeyY: 16,
  KeyT: 17,
  Digit1: 18,
  Digit2: 19,
  Digit3: 20,
  Digit4: 21,
  Digit6: 22,
  Digit5: 23,
  Equal: 24,
  Digit9: 25,
  Digit7: 26,
  Minus: 27,
  Digit8: 28,
  Digit0: 29,
  BracketRight: 30,
  KeyO: 31,
  KeyU: 32,
  BracketLeft: 33,
  KeyI: 34,
  KeyP: 35,
  Enter: 36,
  KeyL: 37,
  KeyJ: 38,
  Quote: 39,
  KeyK: 40,
  Semicolon: 41,
  Backslash: 42,
  Comma: 43,
  Slash: 44,
  KeyN: 45,
  KeyM: 46,
  Period: 47,
  Tab: 48,
  Space: 49,
  Backquote: 50,
  Backspace: 51,
  Escape: 53,
};
const charKeys = {};
for (const [name, code] of Object.entries(keys)) {
  if (name.startsWith("Key")) {
    charKeys[name.slice(-1).toLowerCase()] = [code, false];
    charKeys[name.slice(-1)] = [code, true];
  }
  if (name.startsWith("Digit")) charKeys[name.slice(-1)] = [code, false];
}
for (const [code, pair] of Object.entries({ Digit1:"1!", Digit2:"2@", Digit3:"3#", Digit4:"4$", Digit5:"5%", Digit6:"6^", Digit7:"7&", Digit8:"8*", Digit9:"9(", Digit0:"0)", Minus:"-_", Equal:"=+", BracketLeft:"[{", BracketRight:"]}", Backslash:"\\|", Semicolon:";:", Quote:"'\"", Comma:",<", Period:".>", Slash:"/?", Backquote:"`~", Space:" " })) {
  [...pair].forEach((ch, i) => { charKeys[ch] = [keys[code], i === 1]; });
}
const setupKeys = {ArrowLeft:123, ArrowRight:124, ArrowDown:125, ArrowUp:126, PageUp:116, PageDown:121};
// Apple receives logical view coordinates; Retina PNGs may have more pixels.
const guestWidth = 1024;
const guestHeight = 640;

function closeSession(session, endpoint, maintenance) {
  if (session) api(endpoint, { action: "close", session, ...(maintenance ? { maintenance } : {}) }).catch(() => {});
}

export function MacOSLogin({ disabled, initialSetup = false, maintenance = "", label, setupActions, setupHint, setupStatus, setupError, autoOpen = false, managedCredential = false, onSessionClosed, onSettings }) {
  const endpoint = initialSetup ? "/macos/first-setup" : maintenance ? "/macos/maintenance" : "/macos/login";
  const scope = maintenance ? { maintenance } : {};
  const [opened, setOpened] = useState(false);
  const [session, setSession] = useState("");
  const [frame, setFrame] = useState("");
  const [message, setMessage] = useState("");
  const [frameError, setFrameError] = useState("");
  const [recovery, setRecovery] = useState("");
  const [administrator, setAdministrator] = useState("");
  const [frameTarget, setFrameTarget] = useState(null);
  const [authorizationPassword, setAuthorizationPassword] = useState("");
  const [zoomed, setZoomed] = useState(!initialSetup && !maintenance);
  const [size, setSize] = useState({ width: 0, height: 0 });
  const [aspect, setAspect] = useState(guestWidth / guestHeight);
  const lease = useRef(null);
  const chain = useRef(Promise.resolve());
  const stage = useRef(null);
  const keyboard = useRef(null);
  const imageRef = useRef(null);
  const back = useRef(null);
  const opener = useRef(null);
  const drag = useRef(null);
  const dragged = useRef(false);
  const autoOpened = useRef(false);
  const shown = useRef({ region: [0, 0, guestWidth, guestHeight], geometry: "" });

  const fitWidth = Math.min(size.width, size.height * aspect);
  // Keep a 320 × 200 logical region around the account controls visible.
  const imageWidth = !zoomed ? fitWidth : initialSetup || maintenance ? fitWidth * 2 :
    Math.min(size.width * guestWidth / 320, size.height * aspect * guestHeight / 200);
  const imageHeight = imageWidth / aspect;
  const hasFrame = Boolean(frame);
  const visibleMessage = message || frameError;

  function end(show = "") {
    if (!lease.current) return;
    const old = lease.current;
    lease.current = null;
    closeSession(old.session, endpoint, maintenance);
    setOpened(false);
    setSession("");
    setFrame("");
    setFrameTarget(null);
    shown.current = { region: [0, 0, guestWidth, guestHeight], geometry: "" };
    setAuthorizationPassword("");
    setMessage(show);
    setFrameError("");
  }

  async function open() {
    if (lease.current) return;
    const next = { session: "" };
    lease.current = next;
    chain.current = Promise.resolve();
    setMessage("");
    setFrameError("");
    setRecovery("");
    setZoomed(!initialSetup && !maintenance);
    setOpened(true);
    try {
      const r = await api(endpoint, { action: "open", ...scope });
      if (lease.current !== next) {
        closeSession(r.session, endpoint, maintenance);
        return;
      }
      if (!r.session) { setRecovery(r.recovery || ""); throw new Error(r.error || "暂时无法打开客机登录界面"); }
      setAdministrator(r.administrator || "");
      next.session = r.session;
      setSession(r.session);
    } catch (e) {
      if (lease.current === next) end(e.message);
    }
  }

  useEffect(() => {
    if (autoOpen && !disabled && !autoOpened.current) {
      autoOpened.current = true;
      open();
    }
  }, [autoOpen, disabled]);

  useEffect(() => {
    if (!authorizationPassword) return;
    const timer = setTimeout(() => setAuthorizationPassword(""),30000);
    return () => clearTimeout(timer);
  }, [authorizationPassword]);

  useEffect(() => () => {
    if (lease.current) {
      closeSession(lease.current.session, endpoint, maintenance);
      lease.current = null;
    }
  }, []);

  useLayoutEffect(() => {
    if (!opened) return;
    const control = stage.current;
    const app = document.getElementById("root");
    const wasInert = app?.inert;
    const overflow = document.body.style.overflow;
    if (app) app.inert = true;
    document.body.style.overflow = "hidden";
    const trigger = opener.current;
    keyboard.current?.focus({ preventScroll: true });
    const dismiss = () => end();
    document.addEventListener("cage-close-modal", dismiss);
    function measure() {
      const { width, height } = control.getBoundingClientRect();
      setSize((old) => old.width === width && old.height === height ? old : { width, height });
    }
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(control);
    return () => {
      observer.disconnect();
      document.removeEventListener("cage-close-modal", dismiss);
      document.body.style.overflow = overflow;
      if (app) app.inert = wasInert;
      if (trigger?.isConnected) trigger.focus({ preventScroll: true });
    };
  }, [opened]);

  // The account choices and password field sit near the bottom of loginwindow.
  useLayoutEffect(() => {
    if (!opened || !stage.current) return;
    stage.current.scrollLeft = zoomed ? Math.max(0, (imageWidth - size.width) / 2) : 0;
    stage.current.scrollTop = zoomed ? Math.max(0, imageHeight * (maintenance ? 0.5 : 0.85) - size.height * 0.5) : 0;
  }, [opened, hasFrame, zoomed, imageWidth, imageHeight, size.width, size.height]);

  useEffect(() => {
    if (!session) return;
    let timer;
    let alive = true;
    const poll = async () => {
      try {
        const r = await api(endpoint, { action: "frame", session, ...scope });
        if (!alive || lease.current?.session !== session) return;
        if (r.closed) { setRecovery(r.recovery || ""); end(r.error || ""); onSessionClosed?.(); return; }
        if (r.image) {
          if (maintenance && (!r.geometry || !Array.isArray(r.region) || r.region.length !== 4 || !r.region.every(Number.isInteger)))
            throw new Error("客机维护画面缺少窗口定位，请更新组件后重试");
          setFrameTarget(maintenance ? { geometry: r.geometry, region: r.region } : null);
          setFrame(`data:${r.mime === "image/jpeg" ? "image/jpeg" : "image/png"};base64,${r.image}`);
          setFrameError("");
        }
        if (r.error) setFrameError(r.error);
        timer = setTimeout(poll, 550);
      } catch (e) {
        if (alive && lease.current?.session === session) end(e.message);
      }
    };
    poll();
    return () => { alive = false; clearTimeout(timer); };
  }, [session]);

  function enqueue(action, data) {
    const owner = lease.current;
    if (!owner?.session) return;
    const geometry = shown.current.geometry;
    if (maintenance && !geometry) { setMessage("请等待授权窗口画面后再操作"); return; }
    chain.current = chain.current.then(async () => {
      if (lease.current !== owner) return;
      const r = await api(endpoint, { action, session: owner.session, ...scope, ...(maintenance ? { geometry } : {}), ...data });
      if (lease.current !== owner) return;
      if (r.closed) { end(r.error || ""); onSessionClosed?.(); }
      else setMessage(r.error || "");
    }).catch((e) => { if (lease.current === owner) end(e.message); });
  }

  function sendClick(e) {
    if (dragged.current || e.button !== 0) return;
    const rect = imageRef.current?.getBoundingClientRect();
    if (!rect || !rect.width || !rect.height || e.clientX < rect.left || e.clientX >= rect.right ||
        e.clientY < rect.top || e.clientY >= rect.bottom) return;
    keyboard.current?.focus({ preventScroll: true });
    const [x, y, width, height] = shown.current.region;
    enqueue("click", {
      x: x + Math.floor((e.clientX - rect.left) * width / rect.width),
      y: y + Math.floor((e.clientY - rect.top) * height / rect.height),
    });
  }

  function guestKey(e) {
    if (maintenance && (e.metaKey || e.ctrlKey) && !e.altKey && e.code === "KeyA") {
      e.preventDefault(); enqueue("select-all", {}); return;
    }
    const code = keys[e.code] ?? (initialSetup || maintenance ? setupKeys[e.code] : undefined);
    if (e.metaKey || e.ctrlKey || e.altKey || code === undefined) return;
    e.preventDefault();
    enqueue("key", { code, shift: e.shiftKey });
  }

  useEffect(() => {
    const element = stage.current;
    if (!opened || (!initialSetup && !maintenance) || !element) return;
    const scroll = (event) => {
      const rect = imageRef.current?.getBoundingClientRect();
      if (!rect?.width || !rect.height || event.clientX < rect.left || event.clientX >= rect.right ||
          event.clientY < rect.top || event.clientY >= rect.bottom) return;
      event.preventDefault();
      const unit = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? rect.height : 1;
      const bounded = (delta, scale) => Math.max(-512, Math.min(512, Math.round(delta * unit * scale)));
      const [x, y, width, height] = shown.current.region;
      enqueue("scroll", {
        x: x + Math.floor((event.clientX - rect.left) * width / rect.width),
        y: y + Math.floor((event.clientY - rect.top) * height / rect.height),
        delta_x: bounded(event.deltaX, width / rect.width),
        delta_y: bounded(event.deltaY, height / rect.height),
      });
    };
    element.addEventListener("wheel", scroll, { passive: false });
    return () => element.removeEventListener("wheel", scroll);
  }, [opened, initialSetup, maintenance, session]);

  function changeZoom(zoom) {
    setZoomed(zoom);
    keyboard.current?.focus({ preventScroll: true });
  }

  return <>
    <button type="button" ref={opener} disabled={disabled || opened} onClick={open}>{label || (initialSetup ? "打开客机系统设置" : maintenance ? "打开客机授权设置" : "登录客机账户")}</button>
    {!opened && message && <span role="status">{message}</span>}
    {!opened && ["environment-update", "environment-restart"].includes(recovery) && onSettings &&
      <button type="button" onClick={onSettings}>{recovery === "environment-update" ? "查看环境组件" : "打开环境设置"}</button>}
    {opened && createPortal(
      <div className="mac-login-fullscreen" data-mode={initialSetup ? "setup" : maintenance ? "maintenance" : "login"} role="dialog" aria-modal="true" aria-label={initialSetup ? "首次设置虚拟 Mac" : maintenance ? "恢复客机系统权限" : "登录虚拟 Mac"}
        onKeyDownCapture={(e) => {
          if (e.key === "F6") {
            e.preventDefault();
            (document.activeElement === keyboard.current ? back.current : keyboard.current)?.focus({ preventScroll: true });
          } else if (e.key === "Tab" && e.shiftKey && e.target === back.current) {
            e.preventDefault();
            keyboard.current?.focus({ preventScroll: true });
          } else if (e.key === "Escape" && (e.shiftKey || e.target !== keyboard.current)) {
            e.preventDefault();
            end();
          }
        }}>
        <div className="mac-login-panel">
        <header className="mac-login-heading">
          <h2>{initialSetup ? "虚拟 Mac 的系统设置" : maintenance ? "恢复客机系统权限" : "登录 macOS 客机"}</h2>
          <p>{initialSetup ? "按下方提示完成当前授权，Cage 会自动检查。可以随时返回设置指引继续处理。" : maintenance ? "在当前系统设置窗口中完成授权。切换到其他客机应用或锁定后，画面会结束。" : "输入客机账户密码，按回车登录。"}</p>
        </header>
        <div className="mac-login-tools" role="toolbar" aria-label="画面工具">
          <button ref={back} onClick={() => end()}><ArrowLeft size={16} aria-hidden="true" />{initialSetup ? "返回设置指引" : "返回工作台"}</button>
          <button aria-pressed={!zoomed} onClick={() => changeZoom(false)}><Maximize size={16} aria-hidden="true" />{initialSetup || maintenance ? "适配" : "完整画面"}</button>
          <button aria-pressed={zoomed} onClick={() => changeZoom(true)}><ZoomIn size={16} aria-hidden="true" />{initialSetup || maintenance ? "放大" : "登录区域"}</button>
          {initialSetup && setupActions}
          {initialSetup && managedCredential && <button disabled={!session} onClick={() => enqueue("fill-password",{})}>填写授权密码</button>}
          {!maintenance && (!initialSetup || managedCredential) && <button onClick={async () => {
            const owner = lease.current;
            try {
              const result = await api(endpoint,{action:"show-password"});
              if (lease.current !== owner) return;
              setAuthorizationPassword(result.password); setMessage("");
            }
            catch (e) { if (lease.current === owner) setMessage(e.message); }
          }}>{initialSetup ? "查看授权密码" : "查看客机密码"}</button>}
          {authorizationPassword && <span className="mac-login-password">{authorizationPassword}<button onClick={() => setAuthorizationPassword("")}>隐藏</button></span>}
        </div>
        {maintenance && <div className="mac-setup-hint">
          {maintenance === "projects" && <p>Cage 会直接打开「文件系统扩展」面板。打开 macFUSE 和 macFUSE (local) 两个开关，Cage 会自动检查并重新连接项目。</p>}
          {administrator && <p>系统要求管理员时，用户名填写 {administrator}。选中系统授权窗口的密码框后，点击「填写管理员密码」，再由你确认授权。</p>}
          {administrator && <button type="button" onClick={() => enqueue("fill-password", {})}>填写管理员密码</button>}
        </div>}
        {initialSetup && setupHint && <div className="mac-setup-hint">
          <p>{setupHint}</p>
          <p role={setupError ? "alert" : "status"}>{setupStatus} 查看完整步骤或进入工作台时，点击「返回设置指引」。</p>
        </div>}
        <input ref={keyboard} className="mac-login-keyboard" type="text" autoComplete="off" autoCapitalize="none"
          autoCorrect="off" spellCheck={false}
          aria-label="客机登录键盘。按 F6 切换工具栏，按 Shift 加 Escape 返回工作台。"
          onKeyDown={guestKey}
          onBeforeInput={(e) => e.preventDefault()}
          onInput={(e) => { e.currentTarget.value = ""; }}
          onPaste={(e) => {
            e.preventDefault();
            const text = e.clipboardData.getData("text/plain");
            if (text.length > 256 || [...text].some((c) => !charKeys[c])) {
              setMessage("请用键盘输入该密码。");
              return;
            }
            for (const c of text) {
              const [code, shift] = charKeys[c];
              enqueue("key", { code, shift });
            }
          }} />
        <div ref={stage} className="mac-login-stage" data-zoom={zoomed ? "account" : "fit"}
          onPointerDown={(e) => {
            keyboard.current?.focus({ preventScroll: true });
            dragged.current = false;
            if (!zoomed || e.button !== 0 || !e.isPrimary) return;
            const el = e.currentTarget;
            drag.current = { id: e.pointerId, x: e.clientX, y: e.clientY, left: el.scrollLeft, top: el.scrollTop };
          }}
          onPointerMove={(e) => {
            const start = drag.current;
            if (!start || start.id !== e.pointerId || !(e.buttons & 1)) return;
            const dx = e.clientX - start.x, dy = e.clientY - start.y;
            if (!dragged.current && Math.hypot(dx, dy) < 5) return;
            dragged.current = true;
            const el = e.currentTarget;
            if (!el.hasPointerCapture(e.pointerId)) el.setPointerCapture(e.pointerId);
            el.scrollLeft = start.left - dx;
            el.scrollTop = start.top - dy;
          }}
          onPointerUp={() => { drag.current = null; }}
          onPointerCancel={() => { drag.current = null; dragged.current = true; }}>
          {frame && <div className="mac-login-canvas" style={{
            width: Math.max(size.width, imageWidth), height: Math.max(size.height, imageHeight),
          }}>
            <img ref={imageRef} key={maintenance ? frameTarget?.geometry : undefined} alt={initialSetup ? "Apple 客机首次系统设置" : maintenance ? "客机系统设置或授权窗口" : "Apple 客机登录屏幕"} draggable={false} src={frame}
              style={{ width: imageWidth, height: imageHeight }}
              onLoad={(e) => {
                if (e.currentTarget.naturalHeight) setAspect(e.currentTarget.naturalWidth / e.currentTarget.naturalHeight);
                if (maintenance && frameTarget) shown.current = frameTarget;
              }} onClick={sendClick} />
          </div>}
        </div>
        {(visibleMessage || !frame) && <p className="mac-login-status" role={visibleMessage ? "alert" : "status"}>
          {visibleMessage || (initialSetup ? "正在读取客机首次设置画面…" : maintenance ? "正在确认系统授权窗口…" : "正在确认客机登录画面…")}
        </p>}
        {!initialSetup && !maintenance && <p className="mac-login-help">拖动画面可查看其他账户或提示</p>}
        </div>
      </div>, document.body)}
  </>;
}
