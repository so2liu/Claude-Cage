// Check emitted pages, not just the locale configuration: catch missing routes,
// broken project subpaths and languages drifting away from the README menu.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
const root = fileURLToPath(new URL('../../', import.meta.url));
const readme = fs.readFileSync(path.join(root,'README.md'),'utf8');
const menu = readme.split('\n').find(line => line.includes('(README.md)') && line.includes('(README.'));
assert.ok(menu, 'README language navigation is missing');
const codes = [...menu.matchAll(/\]\(README(?:\.([\w-]+))?\.md\)/g)].map(m => m[1] || 'zh');
// README.md has no dot before the base name's extension in the optional group.
assert.equal(codes[0], 'zh');
assert.ok(!codes.includes('en'));
const dist = path.join(root,'www/dist');
const published = JSON.parse(fs.readFileSync(path.join(root, 'www/src/data/release.json'), 'utf8'));
for (const code of codes) {
  const html = fs.readFileSync(path.join(dist,code === 'zh' ? '' : code,'index.html'),'utf8');
  const picker = html.match(/<details\b[^>]*id="site-language"[^>]*>([\s\S]*?)<\/details>/)?.[1];
  assert.ok(picker, `${code}: missing language picker`);
  const options = [...picker.matchAll(/<a\b[^>]*href="([^"]+)"/g)].map(m=>m[1]);
  assert.equal(options.length,codes.length, `${code}: language dropdown missing options`);
  assert.deepEqual(options, codes.map(locale => `/Claude-Cage/${locale === 'zh' ? '' : locale + '/'}`), `${code}: incorrect language destinations`);
  assert.equal([...picker.matchAll(/aria-current="page"/g)].length, 1, `${code}: current language must be marked once`);
  assert.ok(!html.includes('<select'), `${code}: system popup selector remains`);
  assert.ok(!options.some(url=>url.endsWith('/en/')), `${code}: unexpected English route`);
  assert.ok(html.includes('id="site-language"'), `${code}: missing language selector`);
  assert.ok(html.includes(`dir="${['ar','fa','ps','prs'].includes(code)?'rtl':'ltr'}"`), `${code}: incorrect reading direction`);
  for (const match of html.matchAll(/(?:src|href)="(\/Claude-Cage\/[^"#]+)"/g)) {
    const relative = decodeURIComponent(match[1].slice('/Claude-Cage/'.length));
    const target = path.join(dist,relative.endsWith('/') ? relative+'index.html' : relative);
    assert.ok(fs.existsSync(target), `${code}: broken local asset or link ${match[1]}`);
  }
  assert.ok(!html.includes('/Users/'), `${code}: local developer path leaked`);
  assert.ok(html.includes(`href="${published.downloadURL}"`), `${code}: macOS download is missing`);
  const downloadSection = html.match(/<section\b[^>]*id="download"[^>]*>([\s\S]*?)<\/section>/)?.[1];
  assert.ok(downloadSection?.includes(`CLAUDE CAGE · v${published.version}`), `${code}: download release label must follow published metadata`);
  assert.ok(!downloadSection?.includes('CLAUDE CAGE · ALPHA'), `${code}: stale release channel label`);
  assert.equal([...html.matchAll(/<a\b[^>]*\bdata-download-open(?:\s|>)/g)].length, 3, `${code}: all download entries must open the platform chooser`);
  assert.ok(html.includes('aria-labelledby="platform-title"'), `${code}: platform dialog has no accessible title`);
  for (const platform of ['macOS', 'Windows', 'Linux', 'Android', 'iOS']) {
    assert.ok(html.includes(`data-platform="${platform}"`), `${code}: missing ${platform} choice`);
  }
  const windowsLinks = [...html.matchAll(/href="([^"]*windows-amd64-setup\.exe)"/g)].map(match => match[1]);
  assert.deepEqual(windowsLinks, published.windowsDownloadURL ? [published.windowsDownloadURL, published.windowsDownloadURL] : [], `${code}: Windows download must match the verified optional release metadata`);
  for (const [suffix, url] of [['linux-amd64.deb', published.linuxDownloadURL], ['android-arm64.apk', published.androidDownloadURL], ['windows-amd64-setup-unsigned.exe', published.windowsDownloadURL ? undefined : published.windowsPreview?.downloadURL]]) {
    const links = [...html.matchAll(/href="([^"]+)"/g)].map(match => match[1]).filter(link => link.endsWith(suffix));
    assert.deepEqual(links, url ? [url, url] : [], `${code}: ${suffix} must match verified metadata`);
  }
  const ios = html.match(/<article\b[^>]*data-platform="iOS"[^>]*>([\s\S]*?)<\/article>/)?.[1];
  assert.ok(ios?.includes('href="/Claude-Cage/docs/mobile/"'), `${code}: iOS needs its local build guide`);
  assert.ok(ios?.includes('/Claude-Cage/platforms/apple.svg'), `${code}: iOS platform icon is missing`);
  const android = html.match(/<article\b[^>]*data-platform="Android"[^>]*>([\s\S]*?)<\/article>/)?.[1];
  assert.ok(android?.includes('/Claude-Cage/platforms/android.svg'), `${code}: Android platform icon is missing`);
}
assert.ok(!fs.existsSync(path.join(dist,'en/index.html')), 'Old English page still present');
console.log(`Verified ${codes.length} README languages, dropdowns, directions and local assets.`);

// Check actual guide pages and every cross-page anchor, including project bases.
const guideDir = path.join(dist, 'docs');
const guideFiles = fs.readdirSync(guideDir, { recursive: true }).filter(file => file.endsWith('index.html'));
assert.ok(guideFiles.length >= 12, 'User guide chapters are missing');
const guideSourceFiles = fs.readdirSync(path.join(root, 'docs/guide')).filter(file => file.endsWith('.md'));
assert.equal(guideFiles.length, guideSourceFiles.length, 'Guide source and routes have drifted');
for (const relative of guideFiles) {
  const filename = path.join(guideDir, relative);
  const html = fs.readFileSync(filename, 'utf8');
  assert.ok(html.includes('id="guide-search"'), `${relative}: missing search`);
  assert.ok(html.includes('id="guide-article"'), `${relative}: missing article`);
  assert.ok(html.includes('aria-label="前后章节"'), `${relative}: missing chapter navigation`);
  assert.ok(!html.includes('/Users/'), `${relative}: local developer path leaked`);
  assert.ok(!html.includes('<select'), `${relative}: system popup selector remains`);
  assert.equal([...html.matchAll(/<h1\b/g)].length, 1, `${relative}: expected one page title`);
  const current = `/Claude-Cage/docs/${relative.replace(/index\.html$/, '').replaceAll(path.sep, '/')}`;
  for (const match of html.matchAll(/href="([^\"]+)"/g)) {
    const href = match[1].replaceAll('&amp;', '&');
    if (!href.startsWith('/Claude-Cage/') && !href.startsWith('#')) continue;
    const url = new URL(href, `https://example.test${current}`);
    const local = decodeURIComponent(url.pathname.slice('/Claude-Cage/'.length));
    const target = path.join(dist, !local || local.endsWith('/') ? local + 'index.html' : local);
    assert.ok(fs.existsSync(target), `${relative}: broken guide link ${href}`);
    if (url.hash) {
      const targetHTML = fs.readFileSync(target, 'utf8');
      const id = decodeURIComponent(url.hash.slice(1));
      assert.ok(targetHTML.includes(`id="${id}"`), `${relative}: missing anchor ${href}`);
    }
  }
}
for (const code of codes) {
  const html = fs.readFileSync(path.join(dist, code === 'zh' ? '' : code, 'index.html'), 'utf8');
  assert.ok(html.includes('href="/Claude-Cage/docs/"'), `${code}: homepage needs a guide entry`);
}
console.log(`Verified ${guideFiles.length} user guide pages, local links, anchors, search and homepage entries.`);

const agentGuide = fs.readFileSync(path.join(dist, 'agent.md'), 'utf8');
assert.equal(agentGuide, fs.readFileSync(path.join(dist, 'llms-full.txt'), 'utf8'), 'Agent text copies must share the same source');
assert.ok(agentGuide.includes('agent capabilities') && agentGuide.includes('job_not_found'), 'Agent guide lacks discovery or recovery contract');
assert.ok(!agentGuide.includes('title: 让 Agent'), 'Agent Markdown must omit frontmatter');
for (const name of ['llms.txt', 'agent-bootstrap.mjs', 'cage-install.sh']) assert.ok(fs.existsSync(path.join(dist, name)), `Missing Agent entry ${name}`);
assert.ok(fs.readFileSync(path.join(dist, 'docs/agent/index.html'), 'utf8').includes('JSON 结果和异步任务'), 'Rendered Agent guide missing');
console.log('Verified Agent Markdown, discovery, download helper and rendered guide.');

const screenshotManifest = JSON.parse(fs.readFileSync(path.join(root, 'www/public/docs/screenshots/manifest.json'), 'utf8'));
for (const shot of screenshotManifest) {
  assert.ok(shot.version && shot.platform && shot.capturedAt && shot.source, `${shot.id}: screenshot provenance missing`);
  assert.ok(fs.existsSync(path.join(dist, 'docs/screenshots', shot.file)), `${shot.id}: screenshot was not shipped`);
  const article = fs.readFileSync(path.join(guideDir, shot.chapter, 'index.html'), 'utf8');
  assert.ok(article.includes(`src="/Claude-Cage/docs/screenshots/${shot.file}"`), `${shot.id}: broken screenshot URL`);
  assert.ok(article.includes(shot.version), `${shot.id}: version caption missing`);
  assert.ok(article.includes(`width="${shot.width}" height="${shot.height}"`), `${shot.id}: original dimensions missing`);
}
console.log(`Verified ${screenshotManifest.length} screenshots, original dimensions and version captions.`);
