import { shortcut } from "./platform.mjs";
import React, { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import {
  FileCode2,
  Archive,
  LoaderCircle,
  ChevronDown,
  Globe2,
  Settings,
  Pencil,
  Pin,
  PinOff,
  TerminalSquare,
  X,
} from "lucide-react";
import { tabName } from "./workspace-state.mjs";
import { AgentIcon } from "./agent-icon";

export function TabIcon({ kind, favicon, agent, accountNumber, size = 16 }) {
  const [failed, setFailed] = useState("");
  if (kind === "terminal" && agent) return <AgentIcon accountNumber={accountNumber} size={Math.max(20, size)} />;
  if (kind === "terminal") return <TerminalSquare size={Math.max(20, size)} />;
  if (kind === "editor") return <FileCode2 size={size} />;
  if (kind === "browser-settings") return <Settings size={size} />;
  if (typeof favicon === "string" && favicon.length <= 8192 &&
      /^data:image\/png;base64,[A-Za-z0-9+/=]+$/.test(favicon) && failed !== favicon)
    return <img src={favicon} className="tab-favicon" alt="" width={size} height={size}
      draggable={false} onError={() => setFailed(favicon)} />;
  return <Globe2 size={size} />;
}

function TerminalAction({ label, icon: Icon, disabled, onClick }) {
  return (
    <button className="icon-button" aria-label={label} disabled={disabled} onClick={onClick}>
      <span className="terminal-action-label" aria-hidden="true">{label}</span>
      <Icon size={14} />
    </button>
  );
}

// Active content carries the strongest emphasis; titles and actions use the
// existing paper/selected/muted tokens, 14px type and 8px spacing in both layouts.
export function WorkspaceTabs({
  tabs,
  saved,
  active,
  label,
  horizontal = false,
  vertical = false,
  busy,
  ready,
  onSelect,
  onClose,
  onRename,
  onPin,
  onReorder,
  splitDrag,
  openingSession,
  sessionOpenError,
}) {
  const [menu, setMenu] = useState(null);
  const [drag, setDrag] = useState(null);
  const [allOpen, setAllOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [width, setWidth] = useState(0);
  const pointer = useRef(null);
  const suppressClick = useRef(false);
  const trigger = useRef();
  const list = useRef();
  useEffect(() => {
    if (!horizontal || !list.current) return;
    const observer = new ResizeObserver(([entry]) =>
      setWidth(entry.contentRect.width),
    );
    observer.observe(list.current);
    return () => observer.disconnect();
  }, [horizontal]);
  const gap = tabs.length > 20 ? 1 : 4;
  const available = Math.max(0, width - gap * Math.max(0, tabs.length - 1));
  const regularWidth = Math.min(220, available / Math.max(1, tabs.length));
  const activeWidth =
    tabs.length > 1 && tabs.some((tab) => tab.id === active)
      ? Math.min(220, Math.max(regularWidth, Math.min(160, available * 0.4)))
      : regularWidth;
  const otherWidth = Math.max(
    0,
    (available - activeWidth) / Math.max(1, tabs.length - 1),
  );
  const tabWidth = (id) =>
    id === active ? activeWidth : Math.min(220, otherWidth);
  const matchingTabs = tabs.filter((tab) =>
    `${tabName(tab, saved)} ${tab.url || ""}`
      .toLowerCase()
      .includes(query.toLowerCase()),
  );
  const firstFocusable = tabs.findIndex(tab => tab.historySession
    ? !busy && openingSession !== tab.cli_session_id && !tab.sessionRunningElsewhere
    : ready || tab.kind === "terminal");
  const finishDrag = (commit, event) => {
    const current = pointer.current;
    pointer.current = null;
    setDrag(null);
    splitDrag.clear();
    if (!current?.moved) return;
    suppressClick.current = true;
    if (!commit) return;
    const { clientX, clientY } = event;
    if (!tabs.find(tab => tab.id === current.id)?.historySession && splitDrag.drop(current.id, clientX, clientY)) return;
    const over = document.elementFromPoint(clientX, clientY);
    if (!list.current?.contains(over)) return;
    if (horizontal) onReorder(current.id, null, current.order);
    else {
      const target = over?.closest("[data-tab-id]")?.dataset.tabId;
      if (target && target !== current.id) onReorder(current.id, target);
    }
  };
  useEffect(() => {
    if (!drag) return;
    const cancel = (event) => {
      if (event.key !== "Escape") return;
      event.preventDefault();
      event.stopImmediatePropagation();
      finishDrag(false);
    };
    document.addEventListener("keydown", cancel, true);
    return () => document.removeEventListener("keydown", cancel, true);
  }, [Boolean(drag)]);
  useEffect(() => () => splitDrag.clear(), []);
  const startDrag = (event, tab) => {
    suppressClick.current = false;
    if (busy || (!ready && tab.kind !== "terminal") || event.button !== 0 ||
        !event.isPrimary || event.target.closest(".tab-close, .terminal-row-actions")) return;
    const items = [...list.current.querySelectorAll(":scope > [data-tab-id]")].map((node) => ({
      id: node.dataset.tabId,
      left: node.offsetLeft,
      width: node.getBoundingClientRect().width,
    }));
    const group = horizontal || vertical ? items.filter(item =>
      Boolean(saved[item.id]?.pinned) === Boolean(saved[tab.id]?.pinned)) : items;
    pointer.current = {
      id: tab.id, startX: event.clientX, startY: event.clientY,
      items, group, order: items.map(item => item.id), moved: false,
    };
    (event.target.closest("button") || event.currentTarget).setPointerCapture(event.pointerId);
  };
  const moveDrag = (event) => {
    const current = pointer.current;
    if (!current) return;
    if (!event.buttons) return finishDrag(false);
    let dx = event.clientX - current.startX;
    if (!current.moved && Math.hypot(dx, event.clientY - current.startY) < 5) return;
    current.moved = true;
    const dropSide = tabs.find(tab => tab.id === current.id)?.historySession ? null : splitDrag.hover(current.id, event.clientX, event.clientY);
    if (!horizontal || dropSide) {
      setDrag({ id: current.id, offsets: {}, widths: {} });
      return;
    }
    const item = current.items.find(item => item.id === current.id);
    // Clamp the tab's drawing to its group, but let the pointer cross the last tab's center.
    const center = item.left + item.width / 2 + dx;
    dx = Math.max(
      current.group[0].left - item.left,
      Math.min(
        dx,
        current.group.at(-1).left +
          current.group.at(-1).width -
          item.left -
          item.width,
      ),
    );
    const rest = current.group.filter((other) => other.id !== item.id);
    const target = rest.filter(
      (other) => center > other.left + other.width / 2,
    ).length;
    const groupOrder = rest.map((other) => other.id);
    groupOrder.splice(target, 0, item.id);
    let at = 0;
    current.order = current.items.map((other) =>
      current.group.includes(other) ? groupOrder[at++] : other.id,
    );
    let left = current.items[0].left;
    const offsets = {};
    for (const id of current.order) {
      const original = current.items.find((other) => other.id === id);
      offsets[id] = id === item.id ? dx : left - original.left;
      left += original.width + gap;
    }
    setDrag({
      id: item.id,
      offsets,
      widths: Object.fromEntries(
        current.items.map((item) => [item.id, item.width]),
      ),
    });
  };
  const showMenu = (tab, event) => {
    event.preventDefault();
    if (!horizontal && !vertical) {
      event.currentTarget.focus();
      event.currentTarget.parentElement.querySelector(".terminal-row-actions button:not(:disabled)")?.focus();
      return;
    }
    trigger.current = event.currentTarget;
    const rect = event.currentTarget.getBoundingClientRect();
    setMenu({
      tab,
      left: Math.max(8, Math.min(rect.left, window.innerWidth - 224)),
      top: Math.max(8, Math.min(rect.bottom + 4, window.innerHeight - 152)),
    });
  };
  useEffect(() => {
    if (horizontal) return;
    list.current
      ?.querySelector('[aria-selected="true"]')
      ?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [active, horizontal, tabs.length]);
  useEffect(() => {
    if (!menu && !allOpen) return;
    const dismiss = (event) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        setMenu(null);
        setAllOpen(false);
        trigger.current?.focus();
      }
    };
    const outside = (event) => {
      if (event.target.closest(".all-tabs-menu") || trigger.current?.contains(event.target)) return;
      setAllOpen(false);
    };
    document.addEventListener("keydown", dismiss);
    document.addEventListener("click", outside, true);
    return () => {
      document.removeEventListener("keydown", dismiss);
      document.removeEventListener("click", outside, true);
    };
  }, [menu, allOpen]);
  const actions = menu && (
    <div className="menu workspace-tab-menu"
      style={{ top: menu.top, left: menu.left }}
      role="menu" aria-label="标签操作">
              <button
                role="menuitem"
                onClick={() => {
                  setMenu(null);
                  onRename(menu.tab);
                }}
              >
                重命名标签
              </button>
              <button
                role="menuitem"
                onClick={() => {
                  setMenu(null);
                  onPin(menu.tab.id);
                }}
              >
                <Pin size={14} />
                {saved[menu.tab.id]?.pinned ? "取消置顶" : "置顶标签"}
              </button>
              <button
                role="menuitem"
                onClick={() => {
                  setMenu(null);
                  onClose(menu.tab);
                }}
              >
                <X size={15} />
                关闭标签<kbd>{shortcut("⌘W")}</kbd>
              </button>
    </div>
  );
  return (
    <>
      <div
        ref={list}
        role="tablist"
        aria-label={label}
        aria-orientation={horizontal ? "horizontal" : "vertical"}
        className={
          horizontal || vertical
            ? `browser-tabs ${vertical ? "browser-tabs-vertical" : ""} ${drag ? "is-dragging" : ""}`
            : "sidebar-tabs"
        }
        style={
          horizontal
            ? {
                gap,
                width: tabs.length * 220 + gap * Math.max(0, tabs.length - 1),
                flex: "0 1 auto",
              }
            : undefined
        }
      >
        {tabs.map((tab, index) => {
          const title = tabName(tab, saved);
          const opening = Boolean(openingSession) && openingSession === tab.cli_session_id;
          const runningElsewhere = tab.sessionRunningElsewhere;
          const runningNotice = "其他窗口运行中，退出原会话后可在 Cage 接续";
          const openError = tab.historySession && sessionOpenError?.id === tab.cli_session_id ? sessionOpenError.message : "";
          const errorID = `session-open-error-${tab.cli_session_id}`;
          return (
            <React.Fragment key={tab.id}>
            <div
              data-tab-id={tab.id}
              title={runningElsewhere ? runningNotice : undefined}
              className={`${horizontal || vertical ? "browser-tab" : "sidebar-tab"} ${tab.id === active ? "active" : ""} ${tab.closed ? "ended" : ""} ${drag?.id === tab.id ? "dragging" : ""} ${horizontal && tabWidth(tab.id) < 96 ? "compact" : ""} ${horizontal && tabWidth(tab.id) < 40 ? "tiny" : ""} ${horizontal && tabWidth(tab.id) < 24 ? "sliver" : ""}`}
              style={
                horizontal
                  ? {
                      width: drag?.widths[tab.id] ?? tabWidth(tab.id),
                      transform: drag
                        ? `translateX(${drag.offsets[tab.id] || 0}px)`
                        : undefined,
                    }
                  : undefined
              }
              onPointerDown={(event) => startDrag(event, tab)}
              onPointerMove={moveDrag}
              onPointerUp={(event) => finishDrag(true, event)}
              onPointerCancel={() => finishDrag(false)}
              onLostPointerCapture={() => {
                if (pointer.current) finishDrag(false);
              }}
              onClickCapture={(event) => {
                if (suppressClick.current) {
                  event.preventDefault();
                  event.stopPropagation();
                  suppressClick.current = false;
                }
              }}
            >
              <button
                role={tab.historySession ? undefined : "tab"}
                className={tab.historySession ? "sidebar-session" : undefined}
                aria-busy={opening || undefined}
                aria-describedby={openError ? errorID : undefined}
                aria-description={runningElsewhere ? runningNotice : undefined}
                aria-label={tab.claudeLabel ? `${title} · ${tab.claudeLabel}` : title}
                aria-selected={tab.historySession ? undefined : tab.id === active}
                tabIndex={
                  tab.id === active ||
                  (!tabs.some((t) => t.id === active) && index === firstFocusable)
                    ? 0
                    : -1
                }
                disabled={tab.historySession ? busy || opening || runningElsewhere : !ready && tab.kind !== "terminal"}
                title={runningElsewhere ? `${title}\n${runningNotice}` : tab.url ? `${title}\n${tab.url}` : [title, tab.claudeAccountNumber && `账号 ${tab.claudeAccountNumber}`, tab.claudeLabel].filter(Boolean).join(" · ")}
                onClick={() => onSelect(tab.id)}
                onContextMenu={(event) => showMenu(tab, event)}
                onKeyDown={(event) => {
                  if (!horizontal && !vertical && event.altKey && ["ArrowUp", "ArrowDown"].includes(event.key)) {
                    event.preventDefault();
                    const target = tabs[index + (event.key === "ArrowUp" ? -1 : 1)];
                    if (target) onReorder(tab.id, target.id);
                    return;
                  }
                  if (event.key === "F10" && event.shiftKey) {
                    showMenu(tab, event);
                    return;
                  }
                  const prev = horizontal ? "ArrowLeft" : "ArrowUp";
                  const next = horizontal ? "ArrowRight" : "ArrowDown";
                  if (![prev, next, "Home", "End"].includes(event.key)) return;
                  event.preventDefault();
                  const buttons = [...list.current.querySelectorAll('[role="tab"], .sidebar-session')];
                  const enabled = buttons.map((button, at) => ({button, at})).filter(item => !item.button.disabled);
                  const from = enabled.findIndex(item => item.at === index);
                  const target = enabled[
                    event.key === "Home"
                      ? 0
                      : event.key === "End"
                        ? enabled.length - 1
                        : (from +
                            (event.key === prev ? -1 : 1) +
                            enabled.length) %
                          enabled.length];
                  target?.button.focus();
                  if (target && !tabs[target.at].historySession) onSelect(tabs[target.at].id, false);
                }}
              >
                <TabIcon kind={tab.kind} favicon={tab.favicon} size={15}
                  agent={tab.terminalKind === "claude" || tab.terminalKind === "claude-login" || Boolean(tab.cli_session_id) || Boolean(tab.claude_account_id)}
                  accountNumber={tab.claudeAccountNumber} />
                <span>{title}</span>
                {tab.claudeLabel && <small className="claude-tab-profile">{tab.claudeLabel}</small>}
                {saved[tab.id]?.pinned && <Pin size={11} className="tab-pin" />}
                {!horizontal && !vertical && (runningElsewhere || tab.sessionActive || opening) &&
                  <LoaderCircle size={13} className="spin session-activity" role="img" aria-label={runningElsewhere ? "其他窗口运行中" : opening ? "正在打开会话" : "会话正在运行"} />}
              </button>
              {horizontal || vertical ? (
                <button
                  className="tab-close icon-button"
                  aria-label={`关闭 ${title}`}
                  disabled={busy}
                  onClick={() => onClose(tab)}
                >
                  <X size={14} />
                </button>
              ) : (
                <div className="terminal-row-actions" role="group" aria-label={`${title} 的操作`}>
                  <TerminalAction label={saved[tab.id]?.pinned ? "取消置顶" : "置顶"}
                    icon={saved[tab.id]?.pinned ? PinOff : Pin} disabled={busy} onClick={() => onPin(tab.id)} />
                  <TerminalAction label="重命名" icon={Pencil} disabled={busy} onClick={() => onRename(tab)} />
                  <TerminalAction label={tab.cli_session_id ? "归档" : "关闭终端"} icon={tab.cli_session_id ? Archive : X} disabled={busy} onClick={event => onClose(tab, event.currentTarget)} />
                </div>
              )}
            </div>
            {openError && <p id={errorID} className="sidebar-session-open-error" role="alert">{openError}</p>}
            </React.Fragment>
          );
        })}
      </div>
      {horizontal && (
        <div className="browser-all-tabs">
          <button
            className="icon-button"
            aria-label="搜索标签页"
            title="搜索标签页"
            aria-expanded={allOpen}
            onClick={(event) => {
              trigger.current = event.currentTarget;
              setAllOpen(!allOpen);
              setQuery("");
            }}
          >
            <ChevronDown size={16} />
          </button>
          {allOpen && (
            <>

              <div
                className="menu all-tabs-menu"
                role="dialog"
                aria-label="搜索标签页"
              >
                <input
                  autoFocus
                  aria-label="搜索已打开的标签页"
                  placeholder="搜索标签页"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                />
                <div className="all-tabs-results">
                  {matchingTabs.length === 0 && (
                    <p className="favorites-empty">没有匹配的标签页</p>
                  )}
                  {matchingTabs.map((tab) => (
                    <button
                      key={tab.id}
                      title={tab.url}
                      onClick={() => {
                        setAllOpen(false);
                        onSelect(tab.id);
                      }}
                    >
                      <TabIcon kind={tab.kind} favicon={tab.favicon}
                        agent={tab.terminalKind === "claude" || tab.terminalKind === "claude-login" || Boolean(tab.cli_session_id) || Boolean(tab.claude_account_id)}
                        accountNumber={tab.claudeAccountNumber} />
                      <span>{tabName(tab, saved)}</span>
                      {tab.id === active && (
                        <span className="current-tab-marker">当前</span>
                      )}
                    </button>
                  ))}
                </div>
              </div>
            </>
          )}
        </div>
      )}
      {menu &&
        createPortal(
          <>
            <div className="menu-dismiss" onClick={() => setMenu(null)} />
            {actions}
          </>,
          document.body,
        )}
    </>
  );
}
