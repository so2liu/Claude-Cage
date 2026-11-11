import React, { useEffect, useState } from 'react';
import { Globe2, Info, LayoutPanelLeft, Puzzle, Search, ShieldCheck, ExternalLink } from 'lucide-react';
import { api } from './api';
import { InlineSelect } from './inline-select';
import { BrowserCookies } from './browser-cookies';
import { BrowserExtensions } from './browser-extensions';
import './browser-settings.css';

const sections = [
  { id: 'general', title: '新标签页', icon: Globe2, search: '启动 新标签页 主页 默认 地址 网址' },
  { id: 'appearance', title: '外观', icon: LayoutPanelLeft, search: '外观 标签栏 布局 横排 竖排 缩放 比例 大小' },
  { id: 'privacy', title: '隐私与网站数据', icon: ShieldCheck, search: '隐私 cookie 网站数据 权限 清除 浏览记录' },
  { id: 'extensions', title: '扩展', icon: Puzzle, search: '扩展 插件 管理 安装 导入 edge chrome' },
  { id: 'about', title: '关于浏览器', icon: Info, search: '关于 浏览器 chromium 版本' },
];
export function BrowserSettingsPage({ tab, tabs, currentHome, layout, onLayoutChange, zoom = 1, onZoomChange, onSaved, onInternal, onOpenPage, onConfirm, onSectionChange }) {
  const [section, setSection] = useState(tab.url.split('#')[1] || 'general'), [query, setQuery] = useState('');
  const [home, setHome] = useState(currentHome || 'https://www.bing.com/'), [savedHome, setSavedHome] = useState(currentHome || 'https://www.bing.com/');
  const [saving, setSaving] = useState(false), [error, setError] = useState(''), [saved, setSaved] = useState(false);
  const [loading, setLoading] = useState(true);
  const [cookieSession, setCookieSession] = useState('normal');
  const [info, setInfo] = useState(null);
  useEffect(() => { let live = true; api('/browser/info', {}).then(value => { if (live) setInfo(value); }).catch(() => { if (live) setInfo({ error: true }); }); return () => { live = false; }; }, []);
  useEffect(() => { const next = tab.url.split('#')[1]; if (sections.some(item => item.id === next)) { setSection(next); setQuery(''); } }, [tab.url]);
  useEffect(() => {
    let live = true;
    api('/browser/preferences').then(result => { if (live) { setHome(result.browser_home_url); setSavedHome(result.browser_home_url); } }).catch(error => { if (live) setError(error.message); }).finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, []);
  const changeSection = id => { setSection(id); setQuery(''); onSectionChange?.(id); };
  const save = async event => {
    event.preventDefault(); setSaving(true); setError(''); setSaved(false);
    try { const result = await api('/browser/preferences', { browser_home_url: home }); setHome(result.browser_home_url); setSavedHome(result.browser_home_url); setSaved(true); onSaved(result); }
    catch (error) { setError(error.message); }
    finally { setSaving(false); }
  };
  const privateTab = tabs.find(item => item.incognito && !item.login);
  const cookieTab = cookieSession === 'private' && privateTab ? privateTab : tab;
  const visible = query.trim() ? sections.filter(item => `${item.title} ${item.search}`.toLowerCase().includes(query.trim().toLowerCase())) : sections.filter(item => item.id === section);
  return <section className="browser-settings-page" aria-label="浏览器设置">
    <header className="browser-settings-top"><h1>设置</h1><label className="browser-settings-search"><Search size={16} /><input aria-label="搜索浏览器设置" placeholder="搜索设置" value={query} onChange={event => setQuery(event.target.value)} /></label></header>
    <div className="browser-settings-layout">
      <nav className="browser-settings-nav" aria-label="浏览器设置分类">{sections.map(({ id, title, icon: Icon }) => <button key={id} aria-label={title} aria-description={id === 'general' && home !== savedHome ? '有未保存的更改' : undefined} aria-current={!query && section === id ? 'page' : undefined} onClick={() => changeSection(id)}><Icon size={17} /><span>{title}</span>{id === 'general' && home !== savedHome && <span className="settings-unsaved-dot" title="有未保存的更改" />}</button>)}</nav>
      <div className="browser-settings-compact-nav"><InlineSelect aria-label="浏览器设置分类" value={section} onValueChange={changeSection}>
        {sections.map(item => <option value={item.id} key={item.id}>{item.title}</option>)}
      </InlineSelect></div>
      <div className="browser-settings-content">
        {visible.length === 0 && <div className="browser-settings-no-results"><Search size={24} /><h2>没有找到相关设置</h2><p>试试“标签页”“Cookie”或“扩展”。</p></div>}
        {visible.map(item => <section key={item.id} className="browser-settings-section" aria-label={item.title}>
          <h2>{item.title}</h2>
          {item.id === 'general' && <><p>设置打开新网页时访问的网站。</p><form className="browser-setting-form" onSubmit={save}>
            <label className="browser-setting-field">新标签页地址<input type="url" aria-label="新标签页地址" required maxLength={8192} value={home} disabled={saving || loading} spellCheck="false" autoComplete="off" onChange={event => { setHome(event.target.value); setSaved(false); }} /></label>
            <p>普通标签与无痕标签使用同一默认地址。保存后，新建网页立即生效。</p>
            <div className="browser-setting-actions"><button className="primary" disabled={saving || loading || home === savedHome}>{saving ? '正在保存…' : '保存地址'}</button>{saved && <span role="status">已保存</span>}</div>
            {error && <p className="browser-settings-error" role="alert">{error}</p>}
          </form></>}
          {item.id === 'appearance' && <><p>调整网页与文件编辑器的标签栏。</p><div className="browser-setting-row"><div><h3>标签栏布局</h3><p>立即生效，保存在本机。</p></div><InlineSelect aria-label="标签栏布局" value={layout} onValueChange={onLayoutChange}><option value="horizontal">横排（顶部）</option><option value="vertical">竖排（左侧）</option></InlineSelect></div><div className="browser-setting-row"><div><h3>网页显示比例</h3><p>较小的比例可显示更多内容，立即生效并保存在本机。</p></div><InlineSelect aria-label="网页显示比例" value={zoom} onValueChange={onZoomChange}>{[0.67, 0.75, 0.8, 0.9, 1, 1.1, 1.25].map(value => <option key={value} value={value}>{Math.round(value * 100)}%</option>)}</InlineSelect></div><p>浏览器设置页跟随 Cage 当前的字体与配色。</p></>}
          {item.id === 'privacy' && <><p>管理网站保存在当前浏览会话中的数据和访问权限。</p>
            <div className="browser-setting-row"><div><h3>Cookie 与登录状态</h3><p>普通会话与无痕会话分别管理。</p></div><InlineSelect aria-label="Cookie 浏览会话" value={cookieSession} onValueChange={setCookieSession}><option value="normal">普通会话</option><option value="private" disabled={!privateTab}>无痕会话</option></InlineSelect></div>
            <BrowserCookies key={cookieTab.id} tab={cookieTab} onClose={() => changeSection('general')} />
            <div className="browser-settings-links"><button onClick={() => onInternal('permissions')}><ExternalLink size={15} />网站权限</button><button onClick={() => onInternal('clear')}><ExternalLink size={15} />清除普通会话浏览数据</button></div>
          </>}
          {item.id === 'extensions' && <><p>添加与管理 Cage 浏览器使用的扩展。</p><BrowserExtensions onInternal={onInternal} onOpenPage={onOpenPage} onConfirm={onConfirm} /></>}
          {item.id === 'about' && <><p>Cage 浏览器由环境内的 Chromium 承载。网页、扩展和网站数据随当前 Cage 环境保存。</p><div className="browser-setting-row"><div><h3>浏览器引擎</h3><p>{info?.product || (info?.error ? '版本信息暂时不可用' : '正在读取版本…')}</p></div></div><p>下载文件通过 Cage 保存到默认工作区的 Downloads，可从下载面板另存为。</p></>}

        </section>)}
      </div>
    </div>
  </section>;
}
