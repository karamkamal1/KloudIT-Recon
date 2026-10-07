// Smoke test for tools/latency-rig/flash.html in headless Chromium.
//
//   node tools/latency-rig/test/flash_smoke.mjs
//
// Uses the Playwright install of the browser E2E (test/e2e/node_modules), or
// PLAYWRIGHT_MODULE. Reads real screen pixels from screenshots: the page must be
// black at rest, white while a mouse button or key is held (also with chorded
// buttons and key auto-repeat), and black again after the last release.

import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { join, dirname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { inflateSync } from 'node:zlib';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..', '..', '..');
const page_url = pathToFileURL(join(here, '..', 'flash.html')).href;

async function loadPlaywright() {
  if (process.env.PLAYWRIGHT_MODULE) return import(process.env.PLAYWRIGHT_MODULE);
  try {
    const req = createRequire(join(root, 'test', 'e2e', 'package.json'));
    return await import(pathToFileURL(req.resolve('playwright')).href);
  } catch {
    return import('playwright');
  }
}
const pw = await loadPlaywright();
const { chromium } = pw.default || pw;

let failed = 0;
function check(name, ok, detail = '') {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${name}${detail ? '  (' + detail + ')' : ''}`);
  if (!ok) failed++;
}

// Decodes the single pixel of a 1x1 8-bit RGB/RGBA PNG. With no left or upper
// neighbour every PNG row filter reduces to "none", so the bytes are the pixel.
function pngPixel(buf) {
  let off = 8;
  let ihdr = null;
  const idat = [];
  while (off < buf.length) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString('latin1', off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    if (type === 'IHDR') ihdr = { w: data.readUInt32BE(0), h: data.readUInt32BE(4), depth: data[8], color: data[9] };
    if (type === 'IDAT') idat.push(data);
    off += 12 + len;
  }
  if (!ihdr || ihdr.w !== 1 || ihdr.h !== 1 || ihdr.depth !== 8 || (ihdr.color !== 2 && ihdr.color !== 6)) {
    throw new Error('unexpected PNG ' + JSON.stringify(ihdr));
  }
  const raw = inflateSync(Buffer.concat(idat));
  return [raw[1], raw[2], raw[3]];
}

const W = 640, H = 360;
async function color(page, x = W / 2, y = H / 2) {
  // Two animation frames so the last input has been painted.
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  const [r, g, b] = pngPixel(await page.screenshot({ clip: { x, y, width: 1, height: 1 } }));
  if (r > 240 && g > 240 && b > 240) return 'white';
  if (r < 15 && g < 15 && b < 15) return 'black';
  return `rgb(${r},${g},${b})`;
}
async function screen(page) {
  // Centre and two corners: the flash must cover the whole viewport.
  const c = [await color(page), await color(page, 2, 2), await color(page, W - 3, H - 3)];
  return c.every((v) => v === c[0]) ? c[0] : c.join('/');
}

const browser = await chromium.launch();
try {
  for (const mode of ['canvas', 'css']) {
    const page = await browser.newPage({ viewport: { width: W, height: H } });
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
    await page.goto(`${page_url}?mode=${mode}`);

    const st = () => page.evaluate(() => ({ ...window.flashState }));
    const s0 = await st();
    check(`${mode}: mode`, mode === 'css' ? s0.mode === 'css' : s0.mode.startsWith('canvas'), s0.mode);
    check(`${mode}: instructions shown first`, await page.isVisible('#help'));
    await page.click('#start');
    check(`${mode}: instructions hidden after Start`, !(await page.isVisible('#help')));
    check(`${mode}: Start click did not flash`, (await st()).presses === 0 && (await screen(page)) === 'black');

    await page.mouse.move(W / 2, H / 2);
    await page.mouse.down();
    check(`${mode}: white while the left button is held`, (await screen(page)) === 'white');
    await page.mouse.down({ button: 'right' });
    await page.mouse.up({ button: 'left' });
    check(`${mode}: stays white while another button is still held`, (await screen(page)) === 'white');
    await page.mouse.up({ button: 'right' });
    check(`${mode}: black after the last release`, (await screen(page)) === 'black');

    await page.keyboard.down('KeyA');
    await page.keyboard.down('KeyA'); // auto-repeat (repeat: true) must not count as a new press
    check(`${mode}: white while a key is held`, (await screen(page)) === 'white');
    await page.keyboard.up('KeyA');
    check(`${mode}: black after key release`, (await screen(page)) === 'black');

    // A rapid click (no frame in between) must still end black.
    await page.mouse.down();
    await page.mouse.up();
    check(`${mode}: black after a fast click`, (await screen(page)) === 'black');

    const s1 = await st();
    check(`${mode}: press/release counters`, s1.presses === 4 && s1.releases === 4 && s1.white === false,
      `presses ${s1.presses}, releases ${s1.releases}`);
    check(`${mode}: no page errors`, errors.length === 0, errors.join('; '));
    await page.close();
  }

  const page = await browser.newPage({ viewport: { width: W, height: H } });
  await page.goto(`${page_url}?bare=1`);
  check('bare=1: no instructions, black screen', !(await page.isVisible('#help')) && (await screen(page)) === 'black');
  await page.close();
} finally {
  await browser.close();
}

// The page must stay self-contained (no external requests).
const html = readFileSync(join(here, '..', 'flash.html'), 'utf8');
check('flash.html has no external resources', !/\b(src|href)\s*=\s*["']?(https?:)?\/\//i.test(html));

console.log(failed ? `${failed} check(s) failed` : 'all checks passed');
process.exit(failed ? 1 : 0);
