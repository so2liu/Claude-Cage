import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

test('empty cursor uses opt-in fallback and real cursor shapes still win', async () => {
  const source = (await readFile(new URL('../vendor/novnc/core/util/cursor.js', import.meta.url), 'utf8'))
    .replace(/^import .*;$/m, 'const supportsCursorURIs = true, isTouchDevice = false;')
    .replace('export default class Cursor', 'globalThis.Cursor = class Cursor');
  const context = vm.createContext({
    document: { createElement: () => ({ style: {}, width: 0, height: 0,
      getContext: () => ({ clearRect() {}, putImageData() {} }),
      toDataURL: () => 'data:image/png;base64,test' }) },
    ImageData: class {}, Uint8ClampedArray,
  });
  vm.runInContext(source, context);
  for (const fallback of ['default', 'none']) {
    const target = { style: {} };
    const cursor = new context.Cursor(fallback);
    cursor.attach(target);
    assert.equal(target.style.cursor, fallback);
    cursor.change(new Uint8Array([0, 0, 0, 255]), 0, 0, 1, 1);
    assert.match(target.style.cursor, /^url\(/);
    cursor.change(new Uint8Array(), 0, 0, 0, 0);
    assert.equal(target.style.cursor, fallback);
  }
});
