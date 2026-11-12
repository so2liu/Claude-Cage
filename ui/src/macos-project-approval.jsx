import React, { useEffect, useRef, useState } from "react";
import { api } from "./api";
import { Modal } from "./modal";
import { MacOSLogin } from "./macos-login";
import "./macos-project-approval.css";

const permissionMessages = {
  project_screen_permission: "无法显示文件夹授权窗口。请在虚拟 Mac 的“系统设置 → 隐私与安全性 → 屏幕与系统音频录制”中允许 Cage Guest Window，并按系统提示重新打开助手。",
  accessibility_permission: "无法点击文件夹授权窗口。请在虚拟 Mac 的“系统设置 → 隐私与安全性 → 辅助功能”中允许 Cage Guest Window，然后重试。",
  requires_gui_login: "请先登录并解锁虚拟 Mac 的普通账户，然后重新打开项目。",
};

export function InitializationProjectApproval({ workspace, jobRunning, onClose, onApproved = onClose }) {
  const [pending, setPending] = useState(null);
  const [error, setError] = useState("");
  const [requesting, setRequesting] = useState(false);
  const approved = useRef(onApproved);
  approved.current = onApproved;

  useEffect(() => {
    if (jobRunning || pending || error) return;
    let live = true, timer;
    async function open() {
      setRequesting(true);
      try {
        const result = await api("/projects/open", { workspace, add: false, activate: false });
        if (!live) return;
        if (result.approval) setPending({ workspace: result.workspace, grant: result.approval });
        else approved.current();
      } catch (e) {
        if (!live) return;
        if (e.recovery === "project-operation-busy") timer = setTimeout(open, 1200);
        else setError(e.message);
      } finally {
        if (live) setRequesting(false);
      }
    }
    open();
    return () => { live = false; clearTimeout(timer); setRequesting(false); };
  }, [workspace, jobRunning, pending, error]);

  if (pending) return <MacOSProjectApproval pending={pending} onClose={onClose} onCheck={async () => {
    const result = await api("/projects/open", { workspace, add: false, activate: false });
    if (result.approval) setPending({ workspace: result.workspace, grant: result.approval });
    else onApproved();
  }} />;
  return <Modal title="允许客机访问所选项目" onClose={requesting ? undefined : onClose} footer={<>
    <button disabled={requesting} onClick={onClose}>取消</button>
    {error && <button onClick={() => setError("")}>重新请求授权</button>}
  </>}>
    <p className="macos-approval-path">{workspace}</p>
    {error ? <p role="alert">{error}</p> : <p role="status">{jobRunning ? "当前客机操作结束后，会显示项目授权窗口。" : "正在连接客机的项目授权窗口…"}</p>}
  </Modal>;
}

export function MacOSProjectApproval({ pending, onCheck, onClose, onSettings }) {
  const [frame, setFrame] = useState(null),
    [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState("");
  const [ended, setEnded] = useState(false);
  const [maintenance, setMaintenance] = useState("");
  const active = useRef(false),
    inFlight = useRef(null);
  const call = (action, fields = {}) =>
    api("/macos/project-approval", {
      workspace: pending.workspace,
      grant: pending.grant,
      action,
      ...fields,
    });

  useEffect(() => {
    let live = true,
      timer;
    async function poll() {
      if (!active.current) {
        try {
          const request = call("frame");
          inFlight.current = request;
          const result = await request;
          if (live && !active.current) {
            if (result.error === "project_screen_permission") setMaintenance("screen");
            if (result.error === "accessibility_permission") setMaintenance("input");
            const valid = Boolean(
              !result.error &&
              result.image &&
              result.window &&
              result.pid &&
              result.nonce,
            );
            setFrame(valid ? result : null);
            setNotice(
              valid
                ? ""
                : permissionMessages[result.error] || "等待虚拟 Mac 的项目授权窗口。已允许此文件夹时，可以直接验证授权。",
            );
          }
        } catch (e) {
          if (live && !active.current) {
            setFrame(null);
            setNotice(e.message);
            if (e.recovery === "project-approval-ended") {
              setEnded(true);
              return;
            }
          }
        } finally {
          inFlight.current = null;
        }
      }
      if (live) timer = setTimeout(poll, 650);
    }
    setFrame(null);
    setEnded(false);
    setMaintenance("");
    setNotice("");
    setActionError("");
    poll();
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [pending.workspace, pending.grant]);

  async function act(fn) {
    if (active.current) return;
    active.current = true;
    setBusy(true);
    setActionError("");
    try {
      await inFlight.current?.catch(() => {});
      await fn();
    } catch (e) {
      setFrame(null);
      if (e.recovery === "project-approval-ended") setEnded(true);
      setActionError(e.message);
    } finally {
      active.current = false;
      setBusy(false);
    }
  }
  const cancel = () =>
    act(async () => {
      try {
        await call("cancel");
      } catch (error) {
        if (error.recovery !== "project-approval-ended") throw error;
      }
      onClose();
    });
  const click = (event) => {
    if (!frame) return;
    const rect = event.currentTarget.getBoundingClientRect();
    const x = (event.clientX - rect.left) / rect.width,
      y = (event.clientY - rect.top) / rect.height;
    if (!(x >= 0 && x < 1 && y >= 0 && y < 1)) return;
    void act(async () => {
      const result = await call("click", {
        window: frame.window,
        pid: frame.pid,
        nonce: frame.nonce,
        x,
        y,
      });
      setFrame(null);
      if (result.error === "accessibility_permission") setMaintenance("input");
      if (result.error) throw new Error(permissionMessages[result.error] || "客机授权窗口已变化，请等待新画面。");
      await onCheck();
    });
  };
  return (
    <Modal
      title="允许客机访问所选项目"
      onClose={busy ? undefined : cancel}
      footer={
        <>
          <button disabled={busy} onClick={cancel}>
            取消
          </button>
          <button disabled={busy} onClick={() => act(onCheck)}>
            {ended ? "重新请求授权" : "验证授权"}
          </button>
        </>
      }
    >
      <p>在下方的 macOS 授权窗口中确认所选项目。</p>
      <p className="macos-approval-path">{pending.workspace}</p>
      {frame && (
        <img
          src={"data:image/png;base64," + frame.image}
          alt="虚拟 Mac 的当前项目授权窗口"
          onClick={click}
          className="macos-approval-frame"
          draggable={false}
        />
      )}
      {notice && <p role="status">{notice}</p>}
      {maintenance && !ended && <MacOSLogin maintenance={maintenance} onSettings={onSettings} onSessionClosed={() => void act(onCheck)} />}
      {actionError && <p role="alert">{actionError}</p>}
    </Modal>
  );
}
