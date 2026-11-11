import React, { useEffect, useRef } from "react";
import { api } from "./api";

// The HTTP channel controls window lifetime only. Pixels and input stay in VZ.
export function useMacNativeDesktop() {
  const session = useRef("");
  const opening = useRef(null);
  useEffect(() => {
    let disposed = false;
    let pending = false;
    const timer = setInterval(async () => {
      const current = session.current;
      if (!current || pending) return;
      pending = true;
      try {
        const result = await api("/macos/desktop", { action: "desktop-status", session: current });
        if (!disposed && session.current === current && (result.closed || result.error)) session.current = "";
      } catch {
        if (!disposed && session.current === current) session.current = "";
      } finally { pending = false; }
    }, 10000);
    return () => {
      disposed = true;
      clearInterval(timer);
      const current = session.current;
      session.current = "";
      if (current) void api("/macos/desktop", { action: "desktop-close", session: current }).catch(() => {});
    };
  }, []);
  return () => {
    if (opening.current) return opening.current;
    opening.current = api("/macos/desktop", { action: "desktop-open" }).then(result => {
      if (result.error || !result.session) throw new Error(result.error || "虚拟 Mac 桌面无法打开");
      session.current = result.session;
      return result;
    }).finally(() => { opening.current = null; });
    return opening.current;
  };
}

export function MacNativeDesktop({ onOpen, onStatus }) {
  const open = useRef(onOpen);
  open.current = onOpen;
  useEffect(() => { void open.current().catch(error => onStatus(error.message)); }, []);
  return <section className="desktop-empty">
    <h2>虚拟 Mac 桌面</h2>
    <p>浏览器和 Claude 在独立的客机窗口中运行，可直接使用键盘、鼠标和系统快捷键。</p>
    <button onClick={() => void onOpen().catch(error => onStatus(error.message))}>打开客机窗口</button>
  </section>;
}
