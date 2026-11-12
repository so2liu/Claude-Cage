import React, { useEffect, useRef, useState } from "react";
import { api, native } from "./api";
import { InlineSelect } from "./inline-select";
import "./guest-system-picker.css";

const names = { "debian-12": "Debian 12", "debian-13": "Debian 13", "ubuntu-22.04": "Ubuntu 22.04", "ubuntu-24.04": "Ubuntu 24.04", macos: "macOS", "windows-11": "Windows 11" };
const editions = { Professional: "专业版", ProfessionalN: "专业版 N", Enterprise: "企业版", EnterpriseN: "企业版 N", Education: "教育版", EducationN: "教育版 N", Core: "家庭版", CoreSingleLanguage: "家庭单语言版" };
const languages = { "zh-CN": "简体中文", "zh-TW": "繁体中文", "en-US": "英语（美国）" };

export function HostCloneSummary({ pending, onChange }) {
  const [result, setResult] = useState(null);
  const [error, setError] = useState("");
  const [attempt, retry] = useState(0);
  useEffect(() => {
    let active = true;
    api("/environment/guest-systems/clone").then((next) => {
      if (!active) return;
      setResult(next);
      onChange((current) => current?.clone ? { ...current, clone_key: next.key, clone_ready: next.enough_space } : current);
    }).catch((failure) => { if (active) setError(failure.message); });
    return () => { active = false; };
  }, [attempt, onChange]);
  if (error) return <div className="guest-install-fields" role="alert"><small>{error}</small>
    <button type="button" onClick={() => { setError(""); retry((value) => value + 1); }}>重新检测并匹配</button></div>;
  if (!result) return <small className="guest-install-fields" role="status">正在检测本机系统版本并匹配官方镜像…</small>;
  const plan = result.plan;
  const host = plan.host;
  const download = result.download_bytes > 0 ? `${(result.download_bytes / 1e9).toFixed(2)} GB` : "";
  const source = host.system === "macos" ? "Apple" : host.system === "windows-11" ? "微软" : "发行版官方源";
  const cached = Boolean(plan.path || result.download_bytes === 0 && plan.bytes > 0);
  return <div className="guest-install-fields host-clone-summary" aria-live="polite">
    <strong>将安装：{names[host.system]} {host.system === "windows-11" ? `${host.version} · ${editions[host.edition] || host.edition} · ${languages[host.language] || host.language}` : host.system === "macos" ? host.version : ""}</strong>
    <small>{pending ? "沿用首次选择的系统镜像，继续安装独立客机。" : "根据本机系统版本匹配官方镜像，安装到独立客机中。"}</small>
    {host.build && <small>{pending ? "首次检测的本机构建号" : "本机构建号"} {host.build} · 镜像构建号 {plan.build}</small>}
    {host.system === "windows-11" && host.build !== plan.build && <small>Windows 的发行版本、版本类型和语言与本机一致，补丁版本有所不同。</small>}
    <small>本机的应用、个人文件和登录状态不会复制；客机中的工具、账户和文件会持续保存。</small>
    <small>{cached ? "已找到匹配的本地镜像，校验后直接使用，无需重新下载系统镜像。" : `从 ${source} 下载官方系统镜像，${download ? `预计还需下载 ${download}` : "下载大小将在连接后显示"}。`}</small>
    <small>{result.installation_started ? "继续使用原虚拟磁盘，保留已有安装进度。" : `至少需要 ${result.minimum_free_gib} GiB 可用空间，虚拟磁盘按实际使用增长。`}浏览器、编辑器等工作台组件按需下载。</small>
    {!result.enough_space && <small role="alert">当前可用 {result.free_gib} GiB，请在存储设置中选择空间充足的位置，再重新检查。</small>}
    {!pending && <button type="button" onClick={() => {
      setResult(null);
      onChange((current) => current?.clone ? { ...current, clone_key: "", clone_ready: false } : current);
      retry((value) => value + 1);
    }}>重新检测并匹配</button>}
  </div>;
}

export function GuestSystemPicker({ system, onChange, preserveOnUnmount = false, deferMatch = false }) {
  const [view, setView] = useState(null);
  const [error, setError] = useState("");
  const [attempt, retry] = useState(0);
  const activated = useRef(false);
  useEffect(() => {
    if (!system?.activate || activated.current) return;
    activated.current = true;
    api("/environment/guest-systems/activate",{}).then(() => native("reconnect")).catch(e => setError(e.message));
  }, [system?.activate]);
  useEffect(() => {
    let active = true;
    setError("");
    api("/environment/guest-systems").then((next) => {
      if (!active) return;
      setView(next);
      if (next.fresh || next.pending) {
        onChange(current => !next.pending && current && !current.pending ? current : ({ guest_system: next.pending?.system || "debian-13", media: next.pending?.media || "",
          sha256: next.pending?.sha256 || "", editor_terms: Boolean(next.pending?.editor_terms),
          clone: Boolean(next.pending?.source && !next.pending?.source_kind), clone_key: "", clone_ready: false,
          activate: next.activate, pending: Boolean(next.pending) }));
      } else onChange(null);
    }).catch((e) => {
      if (!active) return;
      if (e.status === 403 || e.status === 404) onChange(null);
      else setError(e.message);
    });
    return () => { active = false; if (!preserveOnUnmount) onChange(undefined); };
  }, [onChange, attempt, preserveOnUnmount]);
  if (!view || !system) return error ? <div className="guest-system-picker"><span role="alert">{error}</span>
    <button type="button" onClick={() => retry((n) => n + 1)}>重新检查安装选项</button></div> : null;
  const os = system.guest_system;
  const change = (changes) => onChange((current) => current?.guest_system === os ? { ...current, ...changes } : current);
  async function choose() {
    try {
      const path = await native("chooseGuestMedia");
      if (path) change({ media: path });
    } catch (e) { setError(e.message); }
  }
  async function reference(kind) {
    try { await native("guestInstallReference", { kind }); }
    catch (e) { setError(e.message); }
  }
  return (
    <div className="guest-system-picker">
      <span>客户机系统</span>
      <InlineSelect aria-label="客户机系统" value={system.clone ? "host" : os} disabled={system.pending}
        onValueChange={(value) => onChange({ guest_system: value === "host" ? view.system : value, clone: value === "host", clone_key: "", clone_ready: false, media: "", sha256: "", editor_terms: false })}>
        {Boolean(view.system) && <option value="host" disabled={!view.available}>自动匹配本机系统版本{view.available ? "" : "（暂不可用）"}</option>}
        <option value="debian-13">Debian 13</option>
        <option value="ubuntu-22.04" disabled={!view.linux_systems?.includes("ubuntu-22.04")}>Ubuntu 22.04{view.linux_systems?.includes("ubuntu-22.04") ? "" : "（此构建暂不可用）"}</option>
        <option value="macos" disabled={view.system !== "macos" || !view.available}>macOS{view.system === "macos" ? view.available ? "" : "（暂不可用）" : "（需要 Apple Silicon Mac）"}</option>
        <option value="windows-11" disabled={view.system !== "windows-11" || !view.available}>Windows 11{view.system === "windows-11" ? view.available ? "" : "（暂不可用）" : "（需要 Windows 11 与 Hyper-V）"}</option>
      </InlineSelect>
      {system.clone && !system.activate && (deferMatch ? <small className="guest-install-fields">Cage 会检测本机系统版本，在下一步显示匹配的官方镜像、下载量与空间要求。</small> : <HostCloneSummary pending={system.pending} onChange={onChange} />)}
      {!system.activate && <small className="guest-install-fields">系统镜像通过主机网络下载，遵循主机系统代理或 TUN 设置，不使用 Cage 的客机代理。使用 Clash 等软件时，请先开启系统代理或 TUN；代理规则决定镜像网站的实际出口。</small>}
      {!system.clone && os !== "debian-13" && !system.activate && <div className="guest-install-fields">
        {os === "ubuntu-22.04" ? <>
          <small>从 Ubuntu 官方源下载 Ubuntu 22.04 LTS 镜像，并校验 SHA-256。Cage 按主机架构选择 ARM64 或 x86_64。</small>
          <small>自动安装开发工具、浏览器和编辑器。客机账户、文件和登录状态保存在独立的持久虚拟磁盘中。</small>
          <small>至少需要 17 GiB 可用空间。失败后可继续原安装，不会改用其他系统或覆盖已有环境。</small>
        </> : os === "macos" ? <>
          <small>{system.media || "将从 Apple 下载 macOS 26.5.2（19.77 GB），并创建 64 GiB 稀疏虚拟磁盘。"}</small>
          {!system.pending && <div>
            <button type="button" onClick={choose}>选择本机 IPSW</button>
            {system.media && <button type="button" onClick={() => change({ media: "" })}>改为下载</button>}
          </div>}
          <small>客机账户和独立密码由 Cage 自动准备并加密保管，启动时自动登录。</small>
        </> : <>
          <small>Windows 11 x64 需要 Hyper-V 和至少 64 GiB 可用磁盘空间。安装前在授权窗口设置客机本地管理员，Cage 会生成离线账户应答。进入客机桌面后回 Cage 继续，使用该账户确认组件安装。Cage 随后创建专用普通账户并重启客机。</small>
          {!system.pending && <div>
            <button type="button" onClick={() => reference("windows-media")}>从微软下载 ISO</button>
            <button type="button" onClick={choose}>选择本机 ISO</button>
          </div>}
          {system.media && <small>{system.media}</small>}
          <label>ISO 的 SHA-256<input value={system.sha256} readOnly={system.pending} spellCheck={false}
            onChange={(e) => change({ sha256: e.target.value.trim() })} placeholder="微软公布的 64 位十六进制摘要" /></label>
          <button type="button" onClick={() => reference("editor-license")}>阅读 Microsoft VS Code Server 许可条款</button>
          <label><input type="checkbox" checked={system.editor_terms} disabled={system.pending}
            onChange={(e) => change({ editor_terms: e.target.checked })} />我接受上述编辑器许可条款</label>
        </>}
      </div>}
      {system.clone && os === "windows-11" && <div className="guest-install-fields">
        <small>安装时需要你确认 Windows 管理员授权，Cage 随后自动安装系统并创建独立客机账户。</small>
        <button type="button" onClick={() => reference("editor-license")}>阅读 Microsoft VS Code Server 许可条款</button>
        <label><input type="checkbox" checked={system.editor_terms} disabled={system.pending}
          onChange={(e) => change({ editor_terms: e.target.checked })} />我接受上述编辑器许可条款</label>
      </div>}
      {view.reason && !view.available && <small className="guest-install-fields">{view.reason}</small>}
      {view.reselect && <button type="button" className="guest-install-fields" onClick={async () => {
        try {
          await api("/environment/guest-systems", { id: view.pending.id }, "DELETE");
          setView(await api("/environment/guest-systems"));
          onChange({ guest_system: "debian-13", media: "", sha256: "", editor_terms: false });
        } catch (e) { setError(e.message); }
      }}>重新选择系统或镜像</button>}
      {system.activate && <small className="guest-install-fields">客机已安装，可以打开工作台。</small>}
      {error && <p role="alert" className="guest-install-fields">{error}</p>}
    </div>
  );
}

export function validGuestChoice(value) {
  if (value?.clone && !value.activate) return Boolean(value.clone_key && value.clone_ready && (value.guest_system !== "windows-11" || value.editor_terms));
  if (!value || value.activate || value.guest_system === "debian-13" || value.guest_system === "ubuntu-22.04") return true;
  if (value.guest_system === "macos") return true;
  return value.media && /^[a-f\d]{64}$/i.test(value.sha256) && value.editor_terms;
}
