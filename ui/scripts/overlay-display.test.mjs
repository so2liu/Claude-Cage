import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { readFile } from 'node:fs/promises';
import { resolve, sep } from 'node:path';
import { pathToFileURL } from 'node:url';

const enabled = process.env.CAGE_BROWSER_TEST_PLAYWRIGHT && process.env.CAGE_BROWSER_TEST_CHROMIUM;
(enabled ? test : test.skip)('noVNC overlays preserve alpha, physical coordinates and removal at HiDPI', async t => {
  const { chromium } = await import(pathToFileURL(process.env.CAGE_BROWSER_TEST_PLAYWRIGHT));
  const root = resolve('ui/vendor/novnc');
  const server = http.createServer(async (req, res) => {
    const path = resolve(root, '.' + new URL(req.url, 'http://localhost').pathname);
    if (!path.startsWith(root + sep)) { res.end('<html></html>'); return; }
    try { res.setHeader('Content-Type', 'text/javascript'); res.end(await readFile(path)); }
    catch { res.writeHead(404); res.end(); }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await chromium.launch({ executablePath: process.env.CAGE_BROWSER_TEST_CHROMIUM, headless: true });
  t.after(async () => { await browser.close(); await new Promise(resolve => server.close(resolve)); });
  for (const dpr of [1, 1.5, 2]) {
    const context = await browser.newContext({ deviceScaleFactor: dpr });
    const page = await context.newPage();
    await page.goto(`http://127.0.0.1:${server.address().port}/`);
    const results = await page.evaluate(async () => {
      const { default: Display } = await import('/core/display.js');
      const output = document.createElement('canvas'); document.body.append(output);
      const display = new Display(output); display.resize(160, 120);
      const image = (width, height, color, mime) => {
        const canvas = document.createElement('canvas'); canvas.width = width; canvas.height = height;
        const ctx = canvas.getContext('2d'); ctx.fillStyle = color; ctx.fillRect(0, 0, width, height);
        return Uint8Array.from(atob(canvas.toDataURL(mime).split(',')[1]), c => c.charCodeAt(0));
      };
      const base = image(160, 120, '#ffffff', 'image/jpeg');
      const popup = image(40, 30, 'rgba(255,0,0,0.5)', 'image/png');
      const front = image(12, 12, '#0000ff', 'image/png');
      const pixel = (x, y) => [...output.getContext('2d').getImageData(x, y, 1, 1).data];
      const values = [];
      for (const zoom of [0.5, 0.75, 1, 1.5]) {
        display.scale = zoom;
        display.imageRect(0, 0, 160, 120, 'image/jpeg', base);
        display.imageRect(60, 40, 40, 30, 'image/png', popup);
        display.imageRect(70, 50, 12, 12, 'image/png', front);
        display.flip(); await display.flush();
        values.push({ outside: pixel(59, 40), alpha: pixel(60, 40), front: pixel(70, 50),
          after: pixel(100, 70), x: display.absX(60 * zoom), y: display.absY(40 * zoom) });
        display.imageRect(0, 0, 160, 120, 'image/jpeg', base);
        display.flip(); await display.flush();
        values.at(-1).removed = pixel(70, 50);
      }
      return values;
    });
    for (const result of results) {
      assert.deepEqual(result.outside, [255,255,255,255]);
      assert.deepEqual(result.front, [0,0,255,255]);
      assert.equal(result.alpha[0], 255); assert(Math.abs(result.alpha[1] - 127) <= 1);
      assert.deepEqual(result.after, [255,255,255,255]);
      assert.deepEqual(result.removed, [255,255,255,255]);
      assert.equal(result.x, 60); assert.equal(result.y, 40);
    }
    await context.close();
  }
});
