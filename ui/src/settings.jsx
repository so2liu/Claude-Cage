import { MobilePairSettings } from "./mobile-pair-settings";
import { ArchivedSessions } from "./cli-sessions";
import { Smartphone } from "lucide-react";
import { PasswordVaultSettings } from "./password-vault";
import { KeyRound } from "lucide-react";
import { InlineSelect } from "./inline-select";
import { clientKeyboard, isRemoteClient, saveClientKeyboard } from "./client-context.mjs";
import { shortcut, usesControl } from "./platform.mjs";
import React, { useEffect, useMemo, useRef, useState } from "react";
import {
  Globe2,
  Clock3,
  FolderOpen,
  Check,
  LoaderCircle,
  RotateCcw,
  ArrowLeft,
  Settings,
  Palette,
  UserRound,
  UsersRound,
  Archive,
} from "lucide-react";
import { api } from "./api";
import { SafetyMessage } from "./safety-message";
import { AppearanceSettings } from "./appearance-settings";
import { ProxySettings } from "./proxy-settings";
import { ExitPolicyFields } from "./exit-policy";
import { canonicalExitPolicy } from "./safety-state.mjs";
import { EnvironmentVersions } from "./environment-versions";
import { EnvironmentStorage } from "./environment-storage";
import { ResourceSettings } from "./resources";
import { DiagnosticSettings } from "./diagnostics.jsx";
import { MacOSLogin } from "./macos-login";
import { AccountSettings } from "./account";
import { ClaudeAccountsPanel } from "./claude-accounts";
const sections = [
  ["account", "账户", UserRound],
  ["vault", "密码保险箱", KeyRound],
  ["claude-accounts", "Claude 账号", UsersRound],
  ["archived-sessions", "已归档会话", Archive],
  ["keyboard", "通用", Settings],
  ["appearance", "外观", Palette],
  ["mobile", "远程连接", Smartphone],
  ["network", "网络与代理", Globe2],
  ["region", "时区与语言", Clock3],
  ["project", "环境与恢复", RotateCcw],
];
export function SettingsPanel({
  current,
  status,
  onClose,
  onSaved,
  onConfirm,
  claudeProfiles,
  terminalTabs,
  onClaudeLogin,
  initialSection = "keyboard",
}) {
  const [localKeyboard, setLocalKeyboard] = useState(() => clientKeyboard(current.keyboard));
  const [accountDirty, setAccountDirty] = useState(false);
  const [proxyDirty, setProxyDirty] = useState(false);
  const [resourceDirty, setResourceDirty] = useState(false);
  const [draft, setDraft] = useState({ ...current }),
    [section, setSection] = useState(initialSection),
    [probe, setProbe] = useState(null),
    [probing, setProbing] = useState(false),
    [probeFailed, setProbeFailed] = useState(false),
    [error, setError] = useState(""),
    [jobId, setJobId] = useState(""),
    [submitting, setSubmitting] = useState(false);
  const changed =
    JSON.stringify({
      ...draft,
      browser_home_url: undefined,
      exit_policy: canonicalExitPolicy(draft.exit_policy),
    }) !==
    JSON.stringify({
      ...current,
      browser_home_url: undefined,
      exit_policy: canonicalExitPolicy(current.exit_policy),
    });
  const environmentChanged = [
    "dns_url",
    "timezone",
    "locale",
    "browser_language",
    "auto_timezone",
    "auto_language",
    "exit_policy",
  ].some((k) =>
    k === "exit_policy"
      ? JSON.stringify(canonicalExitPolicy(draft[k])) !==
        JSON.stringify(canonicalExitPolicy(current[k]))
      : draft[k] !== current[k],
  );
  const busy =
    Boolean(status.job?.running) || Boolean(status.storage_recovery?.required) || probing || submitting || Boolean(jobId);
  const update = (key, value) => {
    setDraft((p) => ({ ...p, [key]: value }));
    if (["dns_url"].includes(key)) setProbe(null);
    setError("");
  };
  const close = async () => {
    if (submitting) return;
    if (status.job?.running && jobId) {
      onClose();
      return;
    }
    if (
      (!changed && !accountDirty && !proxyDirty && !resourceDirty) ||
      (await onConfirm({
        title: "放弃尚未保存的修改？",
        body: "当前环境不会改变。关闭后会丢弃这次表单修改。",
        confirm: "放弃修改",
      }))
    )
      onClose();
  };
  useEffect(() => {
    if (jobId && status.job?.id === jobId && !status.job.running) {
      setJobId("");
      if (status.job.error || status.job.canceled) {
        if (status.job.settings_saved) onSaved();
        setError(status.job.error || status.job.message);
      } else {
        onSaved();
        if (!proxyDirty) onClose();
      }
    }
  }, [status.job, jobId]);
  const check = async () => {
    setProbing(true);
    setProbeFailed(false);
    setError("");
    try {
      const result = await api("/settings/probe", draft);
      setProbe(result);
      if (draft.auto_timezone)
        setDraft((previous) => ({ ...previous, timezone: result.timezone }));
      if (draft.auto_language && result.suggested_locale)
        setDraft((previous) => ({
          ...previous,
          locale: result.suggested_locale,
          browser_language: result.suggested_browser_language,
        }));
    } catch (e) {
      setError(e.message);
      setProbeFailed(true);
    } finally {
      setProbing(false);
    }
  };
  const save = async () => {
    if (busy) return;
    if (
      environmentChanged &&
      status.vm_running &&
      !(await onConfirm({
        title: "应用设置并重启环境？",
        body: "请先保存编辑器中的文件。当前终端会话会结束，浏览器的页面和登录状态将保留。",
        confirm: "保存并重启",
      }))
    )
      return;
    setError("");
    setSubmitting(true);
    try {
      const payload = { ...draft };
      delete payload.browser_home_url;
      const result = await api("/settings", payload);
      setJobId(result.id);
    } catch (e) {
      setError(e.message);
    } finally {
      setSubmitting(false);
    }
  };
  const zones = useMemo(
    () => [
      ...new Set([
        "Etc/UTC",
        current.timezone,
        ...(Intl.supportedValuesOf?.("timeZone") || []),
      ]),
    ],
    [],
  );
  const contentRef = useRef();
  useEffect(() => {
    contentRef.current?.scrollTo(0, 0);
  }, [section]);
  const currentLoc = probe || status.location;
  const closeRef = useRef(close);
  closeRef.current = close;
  useEffect(() => {
    const previous = document.activeElement;
    document.getElementById("settings-back")?.focus();
    const dismiss = (event) => {
      if (document.querySelector('[role="dialog"]')) return;
      if (event.type === "cage-close-modal" || event.key === "Escape") {
        event.preventDefault();
        closeRef.current();
      }
    };
    document.addEventListener("keydown", dismiss);
    document.addEventListener("cage-close-modal", dismiss);
    return () => {
      document.removeEventListener("keydown", dismiss);
      document.removeEventListener("cage-close-modal", dismiss);
      previous?.focus?.();
    };
  }, []);
  return (
    <section className="settings-page" aria-label="设置">
      <aside className="settings-sidebar">
        <button
          id="settings-back"
          className="back-to-workspace"
          onClick={close}
        >
          <ArrowLeft size={17} />
          返回工作台
        </button>
        <h2>设置</h2>
        <nav aria-label="设置分组">
          {sections.map(([id, label, Icon]) => (
            <button
              key={id}
              className={section === id ? "selected" : ""}
              aria-current={section === id ? "page" : undefined}
              onClick={async () => {
                if (id !== section && accountDirty && !(await onConfirm({ title: "放弃尚未保存的修改？", body: "已保存的资料和密码保持不变。", confirm: "放弃修改" }))) return;
                if (id !== section && proxyDirty && !(await onConfirm({ title: "放弃尚未应用的代理修改？", body: "当前代理仍按已应用的链路运行。离开会丢弃这次节点选择。", confirm: "放弃修改" }))) return;
                setSection(id);
              }}
            >
              <Icon size={17} />
              {label}
            </button>
          ))}
        </nav>
      </aside>
      <div className="settings-main">
        <div className={section === "archived-sessions" ? "settings-content settings-content--archives" : section === "claude-accounts" ? "settings-content settings-content--accounts" : section === "mobile" ? "settings-content settings-content--remote" : "settings-content"} ref={contentRef}>
          {section === "appearance" && <AppearanceSettings />}
          {section === "archived-sessions" && <ArchivedSessions ready={status.ready && !busy} />}
          {section === "vault" && <PasswordVaultSettings onConfirm={onConfirm} onDirty={setAccountDirty} />}
          {section === "account" && <AccountSettings busy={busy} onDirty={setAccountDirty} />}
          {section === "claude-accounts" && <ClaudeAccountsPanel store={claudeProfiles} tabs={terminalTabs} onConfirm={onConfirm}
            running={status.vm_running} environmentBusy={busy} login={async (id) => {
              if (changed && !(await onConfirm({ title: "放弃尚未保存的设置并登录？",
                body: "打开登录终端会离开设置页。", confirm: "打开登录终端" }))) return;
              await onClaudeLogin(id);
              onClose();
            }} />}
          <fieldset className="settings-fields" disabled={busy && !(section === "project" && status.storage_recovery?.required)}>
            {section === "mobile" && (isRemoteClient()
              ? <section className="section-title"><h3>连接这台电脑</h3><p>请先切换到这台电脑的本机环境，再生成或管理手机配对码。</p></section>
              : <MobilePairSettings relay={draft.mobile_relay ?? ""} savedRelay={current.mobile_relay ?? ""} onChange={value => update("mobile_relay",value)} onConfirm={onConfirm} />)}
            {section === "network" && (
              <>
                <ProxySettings
                  onDirty={setProxyDirty}
                  status={status}
                  onConfirm={onConfirm}
                  onApplied={async () => {
                    setProbe(null);
                    const applied = await api("/settings");
                    if (JSON.stringify(canonicalExitPolicy(applied.exit_policy)) !==
                        JSON.stringify(canonicalExitPolicy(current.exit_policy))) {
                      setDraft(previous => ({ ...previous, exit_policy: applied.exit_policy }));
                    }
                    await onSaved();
                  }}
                />
                <label>
                  加密 DNS
                  <input
                    value={draft.dns_url}
                    onChange={(e) => update("dns_url", e.target.value)}
                    spellCheck="false"
                  />
                </label>
                <div className="probe-line">
                  <button disabled={busy} onClick={check}>
                    {probing ? (
                      <LoaderCircle size={15} className="spin" />
                    ) : (
                      <Globe2 size={15} />
                    )}
                    验证出口
                  </button>
                  <span>通过当前节点查询 IP 和位置</span>
                </div>
                {currentLoc && (
                  <div className={`location-result ${probe ? "fresh" : ""}`}>
                    <span className="result-icon">
                      <Globe2 size={19} />
                    </span>
                    <div>
                      <strong>
                        {currentLoc.country} · {currentLoc.city}
                      </strong>
                      <code>{currentLoc.ip}</code>
                      <small>
                        {currentLoc.timezone} ·{" "}
                        {new Date(currentLoc.checked_at).toLocaleTimeString(
                          "zh-CN",
                          { hour: "2-digit", minute: "2-digit" },
                        )}{" "}
                        {probe ? "验证通过" : "当前环境 · 最近检测"}
                      </small>
                    </div>
                    {probe && <Check size={18} />}
                  </div>
                )}
                <p className="footnote">
                  代理不可用时，Cage 会停止对外联网。修改表单后需要重新验证。
                </p>
                <section className="network-exit-policy" aria-label="IP 变化规则">
                  <ExitPolicyFields
                    value={draft.exit_policy}
                    onChange={(value) => update("exit_policy", value)}
                    observation={probe}
                    onProbe={check}
                    busy={busy}
                    probing={probing}
                  />
                </section>
              </>
            )}
            {section === "region" && (
              <>
                <div className="section-title">
                  <h3>时区与语言</h3>
                  <p>这些设置作用于 Cage 内的系统、CLI 和浏览器。</p>
                </div>
                {!draft.auto_timezone && !draft.auto_language && (
                  <p className="field-help">
                    时区和语言将使用下方手动选择的值。所有出口仍须查询国家或地区，确认不属于中国大陆；静态 IP 和不限地区也不能跳过此检查。
                  </p>
                )}
                <label className="toggle-row">
                  <span>
                    <strong>跟随出口 IP 的时区</strong>
                    <small>
                      在启动或应用设置时重新查询，不会静默重启正在运行的任务。
                    </small>
                  </span>
                  <input
                    role="switch"
                    type="checkbox"
                    checked={draft.auto_timezone}
                    onChange={(e) => update("auto_timezone", e.target.checked)}
                  />
                </label>
                <label>
                  {draft.auto_timezone ? "将应用的时区" : "系统时区"}
                  <InlineSelect editable
                    aria-label={draft.auto_timezone ? "将应用的时区" : "系统时区"}
                    value={draft.timezone}
                    disabled={draft.auto_timezone}
                    onValueChange={(next) => update("timezone", next)}
                  >
                    {zones.map((zone) => (
                      <option key={zone} value={zone} />
                    ))}
                  </InlineSelect>
                </label>
                {draft.auto_timezone && (
                  <div className="inline-info">
                    <Clock3 size={16} />
                    <span>
                      {probe
                        ? `出口建议时区：${probe.timezone}，保存时应用。`
                        : "保存时经代理查询所在地；查询失败会保留原配置。"}
                    </span>
                    <button
                      className="text-button"
                      onClick={check}
                      disabled={busy}
                    >
                      现在检测
                    </button>
                  </div>
                )}
                <label className="toggle-row">
                  <span>
                    <strong>跟随出口地区的语言建议</strong>
                    <small>
                      启动或应用设置时更新。多语言地区使用内置默认值；没有建议时保留已选语言。
                    </small>
                  </span>
                  <input
                    role="switch"
                    type="checkbox"
                    checked={Boolean(draft.auto_language)}
                    onChange={(e) => update("auto_language", e.target.checked)}
                  />
                </label>
                {draft.auto_language && (
                  <div className="inline-info">
                    <Globe2 size={16} />
                    <span>
                      {currentLoc?.suggested_locale
                        ? `地区建议：${currentLoc.suggested_locale} / ${currentLoc.suggested_browser_language}`
                        : "检测出口后获取地区建议，也可以关闭自动并手动选择。"}
                    </span>
                    <button
                      className="text-button"
                      onClick={check}
                      disabled={busy}
                    >
                      现在检测
                    </button>
                  </div>
                )}
                <label>
                  系统语言
                  <InlineSelect
                    aria-label="系统语言"
                    value={draft.locale}
                    disabled={draft.auto_language}
                    onValueChange={(next) => update("locale", next)}
                  >
                    {[
                      ["en_US.UTF-8", "English (United States)"],
                      ["zh_CN.UTF-8", "简体中文"],
                      ["zh_TW.UTF-8", "繁體中文"],
                      ["ja_JP.UTF-8", "日本語"],
                      ["de_DE.UTF-8", "Deutsch"],
                      ["fr_FR.UTF-8", "Français"],
                    ].map(([value, label]) => (
                      <option key={value} value={value}>
                        {label}
                      </option>
                    ))}
                    {![
                      "en_US.UTF-8",
                      "zh_CN.UTF-8",
                      "zh_TW.UTF-8",
                      "ja_JP.UTF-8",
                      "de_DE.UTF-8",
                      "fr_FR.UTF-8",
                    ].includes(draft.locale) && (
                      <option value={draft.locale}>{draft.locale}</option>
                    )}
                  </InlineSelect>
                </label>
                <label>
                  网页首选语言
                  <InlineSelect
                    aria-label="网页首选语言"
                    value={draft.browser_language}
                    disabled={draft.auto_language}
                    onValueChange={(next) => update("browser_language", next)}
                  >
                    {[
                      ["en-US", "English (United States)"],
                      ["zh-CN", "简体中文"],
                      ["zh-TW", "繁體中文"],
                      ["ja-JP", "日本語"],
                      ["de-DE", "Deutsch"],
                      ["fr-FR", "Français"],
                    ].map(([value, label]) => (
                      <option key={value} value={value}>
                        {label}
                      </option>
                    ))}
                    {![
                      "en-US",
                      "zh-CN",
                      "zh-TW",
                      "ja-JP",
                      "de-DE",
                      "fr-FR",
                    ].includes(draft.browser_language) && (
                      <option value={draft.browser_language}>
                        {draft.browser_language}
                      </option>
                    )}
                  </InlineSelect>
                </label>
                <p className="footnote">
                  网页仍可按自己的规则选择语言。工作台界面使用中文。
                </p>
              </>
            )}
            {section === "keyboard" && (
              <>
                <div className="section-title">
                  <h3>通用</h3>
                  <p>管理工作台的按键习惯与日常操作。</p>
                </div>
                <label>
                  Claude 终端启动命令
                  <input
                    value={draft.claude_terminal_command ?? "claude"}
                    onChange={(e) => update("claude_terminal_command", e.target.value)}
                    placeholder="claude"
                    maxLength={4096}
                    spellCheck="false"
                    autoComplete="off"
                  />
                </label>
                <p className="field-help">
                  选择“Claude 终端”后，在所选项目中运行此命令，例如 claude --continue。
                  留空使用 claude；保存后仅影响新建的 Claude 终端，退出 Claude 后可继续使用 shell。
                </p>
                {typeof current.balanced_display === "boolean" && <label className="toggle-row">
                  <span>
                    <strong>优先流畅</strong>
                    <small>降低浏览器和 Claude Desktop 的清晰度。下次启动环境时生效。</small>
                  </span>
                  <input type="checkbox" role="switch" checked={draft.balanced_display}
                    onChange={(event) => update("balanced_display", event.target.checked)} />
                </label>}
                <h4>键盘习惯</h4>
                <div className="choice-list">
                  {[
                    [
                      "mac",
                      "Mac 快捷键",
                      "使用 ⌘ 操作网页和编辑器。Linux、Windows 客机使用对应的 Ctrl 快捷键，macOS 客机保留 Command。",
                    ],
                    [
                      "native",
                      "客机原生快捷键",
                      "按客机系统的习惯使用 Control、Command 和其他组合键。",
                    ],
                  ].map(([value, title, help]) => (
                    <label
                      className={`choice ${(isRemoteClient() ? localKeyboard : draft.keyboard) === value ? "checked" : ""}`}
                      key={value}
                    >
                      <input
                        type="radio"
                        name="keyboard"
                        value={value}
                        checked={(isRemoteClient() ? localKeyboard : draft.keyboard) === value}
                        onChange={() => {
                          if (!isRemoteClient()) { update("keyboard", value); return; }
                          try { saveClientKeyboard(value); setLocalKeyboard(value); setError(""); }
                          catch { setError("未能保存本机键盘习惯，请检查本地存储空间。"); }
                        }}
                      />
                      <span>
                        <strong>{title}</strong>
                        <small>{help}</small>
                      </span>
                    </label>
                  ))}
                </div>
                <p className="field-help">
                  终端使用 {usesControl ? "Ctrl+Shift+C / Ctrl+Shift+V" : "⌘C / ⌘V"} 复制粘贴；Ctrl+C 始终用于中断命令。
                  {isRemoteClient() && "键盘习惯立即生效，仅保存在当前客户端，不修改云端设置。"}
                </p>
                <h4>工作台快捷键</h4>
                <div className="shortcut-list">
                  {[
                    ["打开项目", "⌘O"],
                    ["新建网页", "⌘T"],
                    ["新建终端", "⌘⇧↵"],
                    ["地址栏", "⌘L"],
                    ["关闭当前标签", "⌘W"],
                    ["切换标签", "⌘1–9"],
                    ["设置", "⌘,"],
                  ].map(([label, key]) => (
                    <div key={label}>
                      <span>{label}</span>
                      <kbd>{shortcut(key)}</kbd>
                    </div>
                  ))}
                </div>
              </>
            )}
            {section === "project" && (
              <>
                <div className="section-title">
                  <h3>环境与恢复</h3>
                  <p>{isRemoteClient() ? "项目位于云端，Cage 中的改动会直接保存到云端文件。" : "本机与 Cage 中的改动会双向同步。"}</p>
                </div>
                {status.local_vm_storage && !isRemoteClient() && <EnvironmentStorage status={status} onConfirm={onConfirm} />}
                <div className="setting-summary-row">
                  <span>当前项目</span>
                  <strong title={current.workspace}>
                    {status.environment_workspace
                      ? "Cage 默认工作区"
                      : current.workspace || "尚未选择项目"}
                  </strong>
                </div>
                <p className="field-help">
                  {isRemoteClient() ? "点击侧栏“项目”标题旁的加号，可浏览并选择云端文件夹。" : "点击侧栏“项目”标题旁的加号，可选择其他本地文件夹。"}
                </p>
                <div className="project-explanation">
                  <FolderOpen size={22} />
                  <div>
                    <strong>所有项目共用一个 Cage 环境</strong>
                    <p>
                      明确打开的项目文件夹会共享到同一个环境。主机的 Claude
                      登录、SSH 密钥和个人配置不会自动导入。浏览器下载统一保存到
                      Cage 默认工作区的 Downloads 文件夹。
                    </p>
                  </div>
                </div>
                <p className="footnote">
                  切换项目只改变新终端和文件编辑器的起始位置；已有终端、网页和登录态会保留。
                </p>
                {!status.storage_recovery?.required && <EnvironmentVersions status={status} onConfirm={onConfirm} />}
                {status.guest_system === "macos" && <section aria-label="客机系统权限">
                  <h4>客机系统权限</h4>
                  <p className="field-help">在受限的客机系统设置窗口中完成授权，Cage 会自动检查并重新连接项目。</p>
                  <div className="project-recovery-actions">
                    <MacOSLogin maintenance="projects" label="文件共享权限" disabled={!status.ready || busy} />
                    <MacOSLogin maintenance="screen" label="屏幕录制权限" disabled={!status.ready || busy} />
                    <MacOSLogin maintenance="input" label="辅助功能权限" disabled={!status.ready || busy} />
                  </div>
                </section>}
                {!status.storage_recovery?.required && <ResourceSettings status={status} onConfirm={onConfirm} onDirtyChange={setResourceDirty} />}
 <DiagnosticSettings />
                {status.can_restore && (
                  <button
                    className="text-button"
                    disabled={busy}
                    onClick={async () => {
                      if (
                        await onConfirm({
                          title: "恢复上一份配置？",
                          body: "会停止当前环境并恢复上次应用前的设置。已经结束的终端进程无法恢复。",
                          confirm: "恢复并重启",
                        })
                      ) {
                        try {
                          const result = await api("/restore", {});
                          setJobId(result.id);
                        } catch (e) {
                          setError(e.message);
                        }
                      }
                    }}
                  >
                    <RotateCcw size={14} />
                    恢复上一份配置
                  </button>
                )}
              </>
            )}
          </fieldset>
          {error && (
            <div role="alert" className="error-message">
              <SafetyMessage message={error} />
              {probeFailed && (draft.auto_timezone || draft.auto_language) && (
                <button
                  className="text-button"
                  disabled={busy}
                  onClick={() => {
                    setDraft((previous) => ({
                      ...previous,
                      auto_timezone: false,
                      auto_language: false,
                    }));
                    setProbeFailed(false);
                    setError("");
                    setSection("region");
                  }}
                >
                  手动设置时区和语言
                </button>
              )}
            </div>
          )}
        </div>
        {(!["appearance", "browser", "account", "claude-accounts", "vault", "mobile", "archived-sessions", "project"].includes(section) || changed) && (
          <footer className="settings-footer">
            {" "}
            <>
              <span className="save-note">
                {status.job?.running
                  ? status.job.message
                  : proxyDirty
                    ? "代理修改待应用，请点击「应用代理」"
                    : changed
                    ? environmentChanged && status.vm_running
                      ? "网络和系统设置需要重启"
                      : "有尚未保存的修改"
                    : "没有未保存的修改"}
              </span>
              <button onClick={close}>返回工作台</button>
              <button
                className="primary"
                disabled={!changed || busy}
                onClick={save}
              >
                {busy ? (
                  <LoaderCircle size={15} className="spin" />
                ) : (
                  <Check size={15} />
                )}{" "}
                {environmentChanged && status.vm_running
                  ? "保存并重启"
                  : "保存设置"}
              </button>
            </>
          </footer>
        )}
      </div>
    </section>
  );
}
