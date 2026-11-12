import React, { useEffect, useRef, useState } from "react";
import { Check, LoaderCircle } from "lucide-react";
import { api, native, nativeAvailable } from "./api";
import { useAccount } from "./account";
import { MacOSLogin } from "./macos-login";
import { DiagnosticSettings } from "./diagnostics.jsx";
import { Modal } from "./modal";
import "./initialization.css";

const steps = [
  { id: "screen", label: "窗口录制", hint: "打开 Cage Guest Window 的窗口录制开关。如果提示「退出并重新打开」，按提示操作。" },
  { id: "input", label: "辅助功能", hint: "打开 Cage Guest Window 的辅助功能开关，允许 Cage 操作客机窗口。" },
  { id: "projects", label: "文件系统扩展", hint: "打开 macFUSE 和 macFUSE (local) 两个开关，用于在客机中访问你选择的项目。" },
];

export function MacOSFirstSetup() {
  const { account, refresh } = useAccount();
  const [permissionPage, setPermissionPage] = useState("screen");
  const [state, setState] = useState(null);
  const [permissions, setPermissions] = useState({});
  const [error, setError] = useState("");
  const [readError, setReadError] = useState("");
  const [completionFailed, setCompletionFailed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [diagnostics, setDiagnostics] = useState(false);
  const [details, setDetails] = useState(false);
  const current = useRef({ account, refresh });
  current.current = { account, refresh };
  const control = useRef({ active: false, inFlight: false, started: false, finished: false, requested: "", pending: "" });

  async function request(action) {
    const lifecycle = action === "start" || action === "stop";
    const signal = AbortSignal.timeout(action === "finish" ? 60000 : 30000);
    let result;
    try {
      result = await api(lifecycle ? `/${action}` : "/macos/first-setup", action === "start" ? { guest_system: "macos" } : action === "stop" ? {} : { action }, undefined, {}, { signal });
    } catch (e) {
      if (signal.aborted) throw new Error("等待客机响应超时。请重新检查状态，确认刚才的操作是否已完成后继续。");
      // A finish response can be lost after the server has closed this entry.
      if (e.status === 410) await current.current.refresh();
      throw e;
    }
    if (result.error) throw new Error(result.error);
    return result;
  }

  // Automatic probes, page changes and explicit retries share one operation slot.
  // A failed probe retains the previous display but cannot advance the workflow.
  async function check() {
    const c = control.current;
    if (c.inFlight || !c.active) return;
    c.inFlight = true;
    setBusy(true);
    try {
      const next = await api("/status", undefined, undefined, {}, { signal: AbortSignal.timeout(15000) });
      if (!c.active) return;
      setState(next);
      const credentialsPending = current.current.account.macos_password_pending;
      if ((!next.vm_running || credentialsPending) && !next.job?.running && !next.job?.error && !next.job?.canceled && !c.started) {
        c.started = true;
        setBusy(true);
        try { await request("start"); }
        catch (e) { if (c.active) setError(e.message); }
        return;
      }
      if (!next.ready || next.job?.running || credentialsPending) { setReadError(""); return; }
      const measured = await request("permissions");
      if (!c.active) return;
      setReadError("");
      setPermissions(measured);
      const pending = steps.find(({ id }) => !measured[id]);
      if (pending && c.pending !== pending.id) {
        c.pending = pending.id;
        setPermissionPage(pending.id);
      }
      if (measured.managed_credential && pending && c.requested !== pending.id) {
        c.requested = pending.id;
        setPermissionPage(pending.id);
        setBusy(true);
        try { await request(pending.id); setError(""); }
        catch (e) { if (c.active) setError(e.message); }
      }
      if (!pending && !c.finished) {
        c.finished = true;
        setBusy(true);
        try { await request("finish"); if (c.active) await current.current.refresh(); }
        catch (e) { if (c.active) { setCompletionFailed(true); setError(e.message); } }
      }
    } catch (e) { if (c.active) setReadError(e.name === "TimeoutError" ? "读取客机状态超时。请重新检查状态；已有安装进度会保留。" : e.message); }
    finally { c.inFlight = false; if (c.active) setBusy(false); }
  }

  useEffect(() => {
    const c = control.current;
    c.active = true;
    let timer;
    async function poll() {
      await check();
      if (c.active) timer = setTimeout(poll, 2000);
    }
    poll();
    window.cageWorkbenchAction = async command => {
      if (command === "closeTab") document.dispatchEvent(new Event("cage-close-modal"));
      if (command === "quit") await native("quitNow");
    };
    window.cageEditAction = () => false;
    if (nativeAvailable()) native("workbenchReady").catch(() => {});
    return () => { c.active = false; clearTimeout(timer); delete window.cageWorkbenchAction; delete window.cageEditAction; };
  }, []);

  async function act(action) {
    const c = control.current;
    if (c.inFlight) return;
    c.inFlight = true;
    if (action === "stop") c.started = true;
    if (steps.some(({ id }) => id === action)) { setPermissionPage(action); c.requested = action; }
    setBusy(true); setError("");
    try {
      await request(action);
      if (action === "finish") { await current.current.refresh(); setCompletionFailed(false); }
    } catch (e) {
      if (action === "finish") setCompletionFailed(true);
      setError(e.message);
    } finally { c.inFlight = false; if (c.active) { setBusy(false); void check(); } }
  }

  const pending = steps.find(({ id }) => !permissions[id]);
  const selected = steps.find(({ id }) => id === permissionPage);
  const running = Boolean(state?.job?.running);
  const canOperate = Boolean(state?.vm_running && !running && !busy);
  const canFinish = Boolean(state?.ready && !account.macos_password_pending && !pending && !readError);
  const progress = !state ? "正在读取安装进度…" : running ? state.job.message || "正在启动和配置虚拟 Mac…" :
    account.macos_password_pending ? "客机密码尚未同步，请重试同步后继续授权。" : !state.vm_running ? "虚拟 Mac 尚未启动，可从已有磁盘继续。" :
    !state.ready ? "虚拟 Mac 已启动，正在等待客机连接。若停在登录画面，请打开客机画面完成登录。" :
    readError ? "暂时无法确认权限状态，已显示的结果供参考。恢复连接后会继续检查。" :
    pending ? `当前需要确认「${pending.label}」。完成后 Cage 会自动检查并继续。` :
    completionFailed ? "三项权限已验证，最后的设置收尾未完成。请重试完成设置。" : "三项权限已验证，正在保存设置；随后继续工作区授权和联网检查。";
  const controls = <button type="button" className="primary" disabled={!canOperate} onClick={() => act(pending?.id || permissionPage)}>
    {busy ? "正在处理…" : `打开${pending?.label || selected.label}`}
  </button>;
  const failure = error || readError || (!running && state?.job?.error) || (!state?.ready && state?.environment_update_error);

  return <main className="initialization"><section className="initialization-content mac-setup-content">
    <div className="initialization-title"><p className="mac-setup-eyebrow">安装与检查 · macOS 系统权限</p><h1>完成虚拟 Mac 的系统授权</h1>
      <p>安装文件和客机磁盘已保留。下面三项授权在虚拟 Mac 内完成，每项验证通过后会自动继续。</p></div>
    <ol className="mac-setup-checklist" aria-label="系统授权进度">
      {steps.map(({ id, label }, index) => <li key={id} data-complete={Boolean(permissions[id])} aria-current={pending?.id === id ? "step" : undefined}>
        <span className="mac-setup-marker">{permissions[id] ? <Check size={16} aria-hidden="true" /> : index + 1}</span>
        <span>{label}</span><small>{permissions[id] ? "已验证" : pending?.id === id ? "等待确认" : "稍后确认"}</small>
      </li>)}
    </ol>
    <section className="initialization-operation mac-setup-task" aria-label="当前设置步骤">
      <h2>{!state?.ready ? "连接虚拟 Mac" : pending ? `确认${pending.label}` : "完成系统设置"}</h2>
      <p role="status">{busy && <LoaderCircle size={16} className="spin" aria-hidden="true" />} {progress}</p>
      {pending && state?.ready && <><p>{selected.hint}</p>{pending.id === "projects" && permissions.projects_error && <p>{permissions.projects_error}</p>}</>}
      <div className="initialization-recovery">
        {state && !running && (!state.ready || account.macos_password_pending || state.job?.error) && <button className="primary" disabled={busy} onClick={() => act("start")}>{account.macos_password_pending ? "重试同步客机密码" : "继续启动并连接"}</button>}
        {pending && state?.ready && controls}
        {completionFailed && <button className="primary" disabled={busy || !canFinish} onClick={() => act("finish")}>重试完成设置</button>}
        <button disabled={busy || running} onClick={() => void check()}>重新检查状态</button>
      </div>
      {running && <button disabled={busy} onClick={() => act("stop")}>停止当前操作，保留进度</button>}
      {state?.vm_running && <MacOSLogin initialSetup autoOpen={Boolean(permissions.managed_credential)} managedCredential={permissions.managed_credential} disabled={!canOperate}
        label="打开客机画面" setupActions={controls} setupHint={`${selected.hint} 系统要求管理员时，用户名填 cagesetup；选中密码框后可点击「填写授权密码」。`}
        setupStatus={failure || progress} setupError={Boolean(failure)} />}
    </section>
    {failure && <section className="initialization-notice" role="alert"><h2>这一步尚未完成</h2><p>{failure}</p><p>已有安装进度会保留。可以重新打开当前设置页，或重新检查状态后继续。</p></section>}
    <details className="mac-setup-help"><summary>管理员密码与找不到设置页时的帮助</summary>
      <p>系统要求管理员时，用户名填 <strong>cagesetup</strong>，{account.macos_password_pending ? "密码同步完成前可能仍需原客机密码。" : account.macos_credentials_ready ? "密码与 Cage 账户相同。" : "在客机画面中选中密码框，再点击「填写授权密码」。"}你确认授权后继续；此临时管理员在收尾时停用。</p>
      <p>文件系统扩展的手动入口：客机「系统设置 → 通用 → 登录项与扩展 → 按类别 → 文件系统扩展」，同时打开 macFUSE 和 macFUSE (local)。</p>
      <div className="initialization-recovery">{steps.map(({ id, label }) => <button key={id} disabled={!canOperate} onClick={() => act(id)}>重新打开{label}</button>)}</div>
    </details>
    <p className="mac-setup-retention">系统授权完成后，还会确认共享工作区和网络隔离。全部检查通过后即可进入工作台。</p>
    <div className="initialization-support"><button className="text-button" onClick={() => setDetails(true)}>查看操作详情</button><button className="text-button" onClick={() => setDiagnostics(!diagnostics)}>日志与诊断</button></div>
    {diagnostics && <DiagnosticSettings />}
    {details && <Modal title="系统设置操作详情" onClose={() => setDetails(false)}><div className="modal-body"><p>{progress}</p>{failure && <p role="alert">{failure}</p>}<pre className="job-log">{state?.job?.log || "当前步骤没有额外操作日志。可通过「日志与诊断」导出完整记录。"}</pre></div></Modal>}
  </section></main>;
}
