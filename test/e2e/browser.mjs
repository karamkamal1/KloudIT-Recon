// End-to-end test: real gateway + real host agent + headless Chromium.
//
//   node test/e2e/browser.mjs            (after `make build`)
//   E2E_WALLCLOCK_SECONDS=600 node test/e2e/browser.mjs   (+ a 10-minute latency probe run on the
//                                        test page; the export lands in results/latency-wallclock.json)
//
// Drives the actual UI: first-run setup, pairing a host, connecting, and
// streaming over direct WebTransport, relayed WebTransport and WebSocket.
// Verifies decoded video, audio, keyboard/mouse delivery to the host, the
// loss handling (late and dropped frames from the host's fault-injection
// hook), and reports the measured latencies. The test pattern carries 16 rows
// of white padding below it (host "testPad"), announced in the video config
// like the padding of an AV1 encoder on RDNA3: every scenario checks the
// client crops it. Decoder hygiene (step 4.1): every scenario checks the
// decode queue bound, that flush() is never called and that no VideoFrame is
// left open; the startup decoder self-test is checked on the real decoders
// and, with a wrapper that holds frames back, on its logic. Codec selection
// (step 4.2): the hello carries each family's decode time on the 1080p
// timing clip, which the host logs with its codec choice. Presentation
// (step 4.3): the transport scenarios draw with the 2D canvas, then the same
// checks (frames drawn, crop, frame barcode) run with WebGL2 and, in a headed
// browser on Xvfb, WebGPU; every renderer scenario checks the canvas is sized
// to device pixels with nothing on top of it, and element fullscreen; the
// renderer "auto" bake-off runs on the live stream, its stored pick is used
// by the next connection and given up for the 2D canvas when it stops
// drawing; Auto's pick rule is checked on made-up numbers. Frame pacing
// (step 4.4): the transport scenarios draw in "Lowest latency" (the
// default); the 2D, WebGL2 and WebGPU scenarios then switch to "Smooth" and
// back live, checking one draw per display refresh, the hold stage and the
// stage bookkeeping, and the 2D one Smooth's fallbacks (the main thread's
// ticks, the watchdog timer); the bake-off runs in Smooth; the pacer's rule
// is checked on a fake clock. Client-side upscaling (Phase 5): the WebGPU
// renderer's FSR 1 passes against a CPU reference written from ffx_fsr1.h
// (unit), and a scenario streaming the 960x540 picture onto a 1920x1080
// canvas with upscaling Auto (FSR 1 draws), then Off, live.

import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, existsSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import net from 'node:net';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const pw = await import(process.env.PLAYWRIGHT_MODULE || 'playwright');
const { chromium } = pw.default || pw;

const results = [];
const outDir = process.env.E2E_OUT || join(root, 'test', 'e2e', 'results');
mkdirSync(outDir, { recursive: true });

function freePort() {
  return new Promise((res) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); });
  });
}

// Optional: run the host agent through a wrapper, e.g. the Windows build under Wine:
//   E2E_HOST_BIN=dist/host-windows-amd64/recon-host.exe
//   E2E_HOST_CMD='["xvfb-run","-a","/usr/lib/wine/wine64"]'  E2E_HOST_FFMPEG=/path/to/ffmpeg.exe
const hostCmd = process.env.E2E_HOST_CMD ? JSON.parse(process.env.E2E_HOST_CMD) : [];
const hostBin = process.env.E2E_HOST_BIN ? join(root, process.env.E2E_HOST_BIN) : null;
const nativeInputLog = !hostBin; // the logging input backend only exists on non-Windows hosts

const procs = [];
function run(bin, args, env = {}, name) {
  let cmd = join(root, 'dist', bin);
  if (bin === 'recon-host' && hostBin) {
    args = [...hostCmd.slice(1), hostBin, ...args];
    cmd = hostCmd.length ? hostCmd[0] : hostBin;
    if (!hostCmd.length) args = args.slice(1);
  }
  const p = spawn(cmd, args, { env: { ...process.env, ...env }, stdio: ['ignore', 'pipe', 'pipe'] });
  p.log = '';
  const onData = (d) => {
    p.log += d;
    if (process.env.E2E_VERBOSE) process.stderr.write(`[${name}] ${d}`);
  };
  p.stdout.on('data', onData);
  p.stderr.on('data', onData);
  procs.push(p);
  return p;
}

function cleanup() { for (const p of procs) { try { p.kill('SIGTERM'); } catch {} } }
process.on('exit', cleanup);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function until(fn, ms, what) {
  const end = Date.now() + ms;
  let last;
  while (Date.now() < end) {
    try { last = await fn(); if (last) return last; } catch (e) { last = e; }
    await sleep(150);
  }
  throw new Error(`timeout waiting for ${what}${last instanceof Error ? ': ' + last.message : ''}`);
}

function check(name, ok, detail = '') {
  results.push({ name, ok, detail });
  console.log(`${ok ? '✔' : '✘'} ${name}${detail ? ' — ' + detail : ''}`);
}

// Per-stage latency (frame header extension + client marks): (a) every stage
// is reported and non-negative, (b) bookkeeping: per frame the stages add up
// to end-to-end (mean within ±2 ms), recomputed here from the raw timestamps.
// (b) holds by construction (each stage is the difference of neighbouring
// marks of the same frame, so the sum telescopes): it catches a stage that is
// dropped, counted twice or taken from the wrong marks, not a wrong clock
// offset or wrong host stamps. The host-log check further down compares the
// client's capture and queue rows with the host's own measurement; the 0.2
// frame barcode is the independent reference for end-to-end.
const STAGES = ['capture', 'queue', 'network', 'transfer', 'wait', 'decode', 'hold', 'draw', 'display'];
const DISPLAY = STAGES.indexOf('display');
async function checkStages(name, st) {
  const lat = st?.stages;
  const missing = STAGES.filter((k) => !lat?.stages?.[k]);
  const negative = STAGES.filter((k) => lat?.stages?.[k] && ['p50', 'p95', 'p99'].some((q) => lat.stages[k][q] < 0));
  const p = (r) => (r ? `${r.p50}/${r.p95}/${r.p99}` : '—');
  check(`${name}: per-stage latency (all stages, non-negative)`, !!lat && lat.from === 'capture' && !missing.length && !negative.length && lat.e2e.p50 >= 0,
    lat ? `${lat.from}→draw ${p(lat.e2e)} ms; ${STAGES.map((k) => `${k} ${p(lat.stages[k])}`).join(', ')}${missing.length ? '; missing ' + missing : ''}${negative.length ? '; negative ' + negative : ''}` : 'no stage stats');
  const overlay = await page.textContent('#stats').catch(() => '');
  check(`${name}: overlay shows the stage table`, overlay.includes('End-to-end (capture→draw)') && overlay.includes('host queue') && overlay.includes('hold (frame pacing)') &&
    overlay.includes('display (est.)'));
  const pill = await page.getAttribute('#latency', 'title').catch(() => '');
  check(`${name}: latency pill labelled capture→draw`, pill.startsWith('End-to-end latency (capture→draw)'), pill);

  await page.evaluate(() => { window.__recon.stageDump = null; window.__recon.worker.postMessage({ type: 'stageDump' }); });
  const dump = await until(() => page.evaluate(() => window.__recon.stageDump), 3000, 'stage dump').catch(() => []);
  let n = 0; let sum = 0; let e2e = 0; let dn = 0; let dsum = 0; let de2e = 0;
  for (const r of dump) {
    const start = (r.fromCapture ? r.captureUs : r.sendUs) / 1000 - r.offset;
    let s = 0;
    for (let i = r.fromCapture ? 0 : 2; i < DISPLAY; i++) s += r.stages[i];
    n++; sum += s; e2e += r.drawn - start;
    if (r.displayed !== undefined) { dn++; dsum += s + r.stages[DISPLAY]; de2e += r.displayed - start; }
  }
  const diff = n ? Math.abs(sum - e2e) / n : Infinity;
  const ddiff = dn ? Math.abs(dsum - de2e) / dn : Infinity;
  check(`${name}: stage bookkeeping: per-frame stages sum to end-to-end (structural, ±2 ms)`, n >= 20 && diff <= 2 && ddiff <= 2,
    `${n} frames: mean sum ${(sum / n).toFixed(2)} vs end-to-end ${(e2e / n).toFixed(2)} ms; ${dn} with display est: ${(dsum / dn).toFixed(2)} vs ${(de2e / dn).toFixed(2)} ms`);
  results.push({ stages: name, summary: lat, frames: dump.length });
}

// Latency probe, seq mode (step 0.2): the test pattern carries each frame's seq
// as a barcode; the client reads it back from 1 in 30 decoded frames. At least
// 90 % of the sampled frames must show a valid barcode equal to the frame's seq
// (the picture drawn is the frame its header describes), and the capture->drawn
// histogram must have data.
async function checkProbe(name, method = null, rate = 30) {
  // A loaded machine can stall decoding for a while: give it up to 10 s more
  // for 10 samples (1 in 30 frames, fewer when readbacks are still in flight:
  // longer for a stream below 60 fps).
  await until(() => page.evaluate(() => window.__recon.probe?.sampled >= 10), 10000 * Math.max(1, 60 / rate), '10 probe samples').catch(() => {});
  const pr = await page.evaluate(() => window.__recon.probe);
  const matched = pr ? pr.valid - pr.mismatched : 0;
  const share = pr?.sampled ? matched / pr.sampled : 0;
  const hist = pr?.histogram ? Object.values(pr.histogram).reduce((a, b) => a + b, 0) : 0;
  const l = pr?.latency;
  check(`${name}: frame barcode = seq on >= 90 % of sampled frames, capture→drawn histogram${method ? ` (read back by ${method})` : ''}`,
    pr?.mode === 'seq' && pr.sampled >= 10 && share >= 0.9 && hist > 0 && l?.n === hist && (!method || pr.method === method),
    pr ? `${pr.sampled} sampled (${pr.method}): ${matched} match seq (${(100 * share).toFixed(0)} %), ${pr.invalid} invalid, ${pr.mismatched} mismatched, ` +
      `${pr.implausible} implausible, ${pr.skipped} skipped; capture→drawn p50/p95/p99 ${l ? `${l.p50}/${l.p95}/${l.p99} ms (n ${l.n})` : '—'}` +
      `${pr.lastInvalid ? `; last invalid: seq ${pr.lastInvalid.seq}, cells ${pr.lastInvalid.luma}` : ''}` : 'no probe state');
  results.push({ probe: name, summary: pr });
  return pr;
}

// Coded-size crop (step 1.7): the host pads the 960x540 test pattern with
// TEST_PAD white rows and announces them (video config cropBottom). The client
// must show exactly 960x540: the bottom rows on screen are the pattern's
// colour bars (yellow at 5/12, blue at 7/12 of the width; x outside the
// toasts, the stats overlay hidden meanwhile), not white padding, and not the
// bars squeezed together with the padding. testsrc2's moving shape crosses
// those rows for single frames (13 of 2400): up to five screenshots 200 ms
// apart, one must show both bars.
const TEST_PAD = 16;
async function checkCrop(name) {
  const cfg = await page.evaluate(() => window.__recon.videoCfg);
  const vid = await page.evaluate(() => window.__recon.video);
  const renderer = await page.evaluate(() => window.__recon.conn?.renderer);
  const r = await page.evaluate(() => { const b = document.getElementById('screen').getBoundingClientRect(); return { x: b.x, y: b.y, w: b.width, h: b.height }; });
  const scale = Math.min(r.w / vid.w, r.h / vid.h); // object-fit: contain
  const cw = vid.w * scale;
  const ch = vid.h * scale;
  const clip = { x: r.x + (r.w - cw) / 2, y: r.y + (r.h - ch) / 2 + ch - 4, width: cw, height: 3 };
  let px = null;
  let bars = false;
  let shots = 0;
  // The stats overlay (semi-transparent) reaches the bottom rows in a small viewport.
  const statsShown = await page.evaluate(() => {
    const el = document.getElementById('stats');
    const shown = !el.classList.contains('hidden');
    el.classList.add('hidden');
    return shown;
  });
  while (!bars && shots < 5) {
    if (shots++) await sleep(200);
    const png = await page.screenshot({ clip });
    px = await page.evaluate(async (bytes) => {
      const bmp = await createImageBitmap(new Blob([new Uint8Array(bytes)], { type: 'image/png' }));
      const g = new OffscreenCanvas(bmp.width, bmp.height).getContext('2d');
      g.drawImage(bmp, 0, 0);
      const d = g.getImageData(0, 0, bmp.width, bmp.height).data;
      const at = (fx) => {
        const x = Math.floor(fx * bmp.width);
        const sum = [0, 0, 0];
        for (let y = 0; y < bmp.height; y++) for (let c = 0; c < 3; c++) sum[c] += d[(y * bmp.width + x) * 4 + c];
        return sum.map((v) => Math.round(v / bmp.height));
      };
      return { yellow: at(5 / 12), blue: at(7 / 12) };
    }, [...png]);
    const [yr, yg, yb] = px.yellow;
    const [br, bg, bb] = px.blue;
    bars = yr > 150 && yg > 150 && yb < 110 && br < 110 && bg < 110 && bb > 150;
  }
  if (statsShown) await page.evaluate(() => document.getElementById('stats').classList.remove('hidden'));
  // The worker logs what the decoder outputs for a padded stream.
  const logLine = (await page.evaluate(() => window.__recon.logs)).filter((l) => l.includes('padded picture:')).pop() || '';
  check(`${name}: padded picture cropped to the announced size (video config crop)`,
    cfg?.cropBottom === TEST_PAD && cfg.codedHeight === cfg.height + TEST_PAD && vid.w === cfg.width && vid.h === cfg.height && bars &&
      logLine.includes(`decoder output ${cfg.codedWidth}x${cfg.codedHeight}`),
    `config ${cfg?.width}x${cfg?.height} coded ${cfg?.codedWidth}x${cfg?.codedHeight} cropBottom ${cfg?.cropBottom}; shown ${vid.w}x${vid.h} (${renderer}); ` +
      `bottom rows (screenshot ${shots}) at 5/12 rgb(${px.yellow}) (yellow bar), at 7/12 rgb(${px.blue}) (blue bar); log: ${logLine.replace(/^\S+ /, '')}`);
}

// Presentation (step 4.3): the expected path drew the frames, its context
// reports whether desynchronized was granted (the 2D canvas must have it in
// Chromium), the canvas backing store is the device-pixel size of its box
// (no compositor scaling), and nothing transforms the canvas or sits on top
// of it at rest (sampled on an 8x8 grid; the stats overlay, diagnostics, is
// hidden for the check and toasts are transient).
async function checkPresentation(name, path) {
  const st = await page.evaluate(() => window.__recon.lastStats);
  const r = st?.renderer;
  const d = await page.evaluate(() => {
    const c = document.getElementById('screen');
    const stats = document.getElementById('stats');
    const shown = !stats.classList.contains('hidden');
    stats.classList.add('hidden');
    const b = c.getBoundingClientRect();
    const styled = [];
    for (let e = c; e; e = e.parentElement) {
      const cs = getComputedStyle(e);
      const bad = [cs.transform !== 'none' && `transform ${cs.transform}`, cs.filter !== 'none' && `filter ${cs.filter}`,
        (cs.backdropFilter || 'none') !== 'none' && `backdrop-filter ${cs.backdropFilter}`, cs.opacity !== '1' && `opacity ${cs.opacity}`,
        cs.mixBlendMode !== 'normal' && `mix-blend-mode ${cs.mixBlendMode}`].filter(Boolean);
      if (bad.length) styled.push(`${e.id || e.tagName.toLowerCase()}: ${bad.join(', ')}`);
    }
    const covered = new Set();
    for (let i = 0; i < 8; i++) {
      for (let j = 0; j < 8; j++) {
        const top = document.elementFromPoint(b.left + ((i + 0.5) * b.width) / 8, b.top + ((j + 0.5) * b.height) / 8);
        if (top !== c && !top?.closest('#toasts')) covered.add(top ? top.id || top.className || top.tagName : 'nothing');
      }
    }
    if (shown) stats.classList.remove('hidden');
    return { css: [b.width, b.height], dpr: devicePixelRatio, box: window.__recon.box, canvases: document.querySelectorAll('#stage canvas').length, styled, covered: [...covered] };
  });
  const want = d.css.map((v) => Math.round(v * d.dpr));
  const sizeOk = !!r && r.canvas[0] === d.box?.w && r.canvas[1] === d.box?.h && Math.abs(d.box.w - want[0]) <= 1 && Math.abs(d.box.h - want[1]) <= 1;
  check(`${name}: ${path} draws on a canvas sized to its box in device pixels, nothing transforms or covers it`,
    r?.name === path && r.drawErrors === 0 && sizeOk && !d.styled.length && !d.covered.length && d.canvases === 1 && (path !== 'canvas2d' || r.desynchronized === true),
    `${r?.name}${r?.gpu ? ` (${r.gpu})` : ''}, getContextAttributes().desynchronized ${r?.desynchronized}; canvas ${r?.canvas?.join('x')} for a box of ` +
      `${d.css.map((v) => +v.toFixed(1)).join('x')} CSS px at dpr ${d.dpr} (${d.box?.w}x${d.box?.h}); ${r?.drawErrors} render errors; ${d.canvases} canvas(es); ` +
      `transforms/filters: ${d.styled.join('; ') || 'none'}; covered by: ${d.covered.join(', ') || 'nothing'}`);
  results.push({ presentation: name, renderer: r, dom: d });
}

// Element fullscreen (the player: canvas stage + stream UI, navigationUI
// "hide"; the option itself is not observable from the page) from the
// toolbar, which appears when the pointer reaches the top edge (on a loaded
// machine the toolbar can hide again before the click lands: then the
// Ctrl+Alt+Shift+F hotkey), and back by the hotkey; the canvas follows its
// box both ways.
async function checkFullscreen(name) {
  const sized = async () => {
    await sleep(800); // a stats update after the resize
    return page.evaluate(() => ({ box: window.__recon.box, canvas: window.__recon.lastStats?.renderer?.canvas }));
  };
  const fsElement = (ms) => until(() => page.evaluate(() => document.fullscreenElement?.id), ms, 'fullscreen').catch(() => null);
  await page.mouse.move(100, 60);
  await page.mouse.move(100, 2, { steps: 3 });
  const shown = await until(() => page.evaluate(() => !document.getElementById('toolbar').classList.contains('hide')), 3000, 'toolbar').catch(() => false);
  await page.click('#btn-fullscreen', { timeout: 3000 }).catch(() => {});
  let via = 'toolbar button';
  let el = await fsElement(3000);
  if (!el) {
    via = 'hotkey (the button click missed)';
    await page.keyboard.press('Control+Alt+Shift+KeyF');
    el = await fsElement(6000);
  }
  const inFs = await sized();
  await page.keyboard.press('Control+Alt+Shift+KeyF');
  const left = await until(() => page.evaluate(() => !document.fullscreenElement), 6000, 'leaving fullscreen').catch(() => false);
  const out = await sized();
  const fits = (x) => x.canvas?.[0] === x.box?.w && x.canvas?.[1] === x.box?.h;
  const refused = (await page.evaluate(() => window.__recon.logs)).filter((l) => l.includes('fullscreen refused')).pop() || '';
  check(`${name}: toolbar at the top edge, element fullscreen of the player and back, the canvas follows its box`, shown && el === 'player' && left && fits(inFs) && fits(out),
    `toolbar shown ${shown}; fullscreen element ${el} via ${via}; in fullscreen box ${inFs.box?.w}x${inFs.box?.h}, canvas ${inFs.canvas?.join('x')}; ` +
      `after: box ${out.box?.w}x${out.box?.h}, canvas ${out.canvas?.join('x')}${refused ? `; ${refused.replace(/^\S+ /, '')}` : ''}`);
}

// Renderer "auto" (step 4.3). Without a stored result the first connection
// runs the bake-off on the live stream: every path that works here takes
// turns (two rounds, A B C C B A), its draw and display stages are measured,
// Auto's pick (renderers.js pickPath: a desynchronized context first, the 2D
// default unless another path draws clearly faster in every round) keeps
// drawing on the only canvas left and is stored for this browser
// (localStorage); the overlay lists the paths. While it runs nothing covers
// the canvas (start-up toolbar and game-mode hint come with the result), and
// the host logs the stage window as renderer=bakeoff (it mixes paths). The
// next connection uses the stored pick at once; "Measure renderers again"
// clears it. A stored WebGL2 pick whose context is lost while drawing is
// given up: forgotten, reconnected with the 2D canvas. In the headed browser
// (Xvfb) when there is one, where all three paths work; headless, WebGPU is
// unavailable and the bake-off runs with the other two. The bake-off runs
// with frame pacing Smooth (step 4.4): every path draws on the display
// refresh, across the switches.
const PRESENT_KEY = 'recon.present.v2';

async function checkBakeoff() {
  const mainPage = page;
  page = (await headedPage().catch(() => null)) || mainPage;
  const hostProc = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  try {
    const all = page === mainPage ? ['canvas2d', 'webgl2'] : ['canvas2d', 'webgl2', 'webgpu'];
    await page.goto(`${base}/`);
    await page.evaluate((k) => localStorage.removeItem(k), PRESENT_KEY);
    await page.mouse.move(200, 200); // away from the top edge (toolbar)
    const log0 = hostProc.log.length;
    await startStream({ path: 'auto', transport: 'auto', renderer: 'auto', fps: 30, mouse: 'game', pacing: 'smooth' });
    const calls = await watchDecoder();
    const t0 = Date.now();
    // Until the result: the start-up toolbar or game-mode hint over the
    // canvas while the paths are measured? (Other toasts, e.g. a decoder
    // warning on a loaded machine, are listed, not failed.)
    const covered = [];
    const other = new Set();
    let samples = 0;
    const result = await until(async () => {
      const x = await page.evaluate(() => ({
        r: window.__recon.bakeoff, bake: window.__recon.lastStats?.renderer?.bake,
        bar: !document.getElementById('toolbar').classList.contains('hide'), toasts: [...document.querySelectorAll('#toasts .toast')].map((t) => t.textContent),
      }));
      if (x.r) return x.r;
      if (x.bake && !x.bake.done && !x.bake.warming) {
        samples++;
        const hint = x.toasts.some((t) => t.startsWith('Game mode'));
        if (x.bar || hint) covered.push(`${x.bar ? 'toolbar ' : ''}${hint ? 'game-mode hint ' : ''}in slot ${x.bake.slot}`);
        for (const t of x.toasts) if (!t.startsWith('Game mode')) other.add(t);
      }
      return null;
    }, 60000, 'bake-off result').catch(() => null);
    const took = (Date.now() - t0) / 1000;
    const after = await page.evaluate(() => ({
      bar: !document.getElementById('toolbar').classList.contains('hide'),
      toasts: [...document.querySelectorAll('#toasts .toast')].map((t) => t.textContent),
    }));
    check('renderer auto: the start-up toolbar and game-mode hint stay off the canvas while the bake-off measures and follow its result',
      !!result && samples >= 5 && !covered.length && after.bar && after.toasts.some((t) => t.startsWith('Game mode')) && after.toasts.some((t) => t.startsWith('Renderer:')),
      `${samples} samples while measuring${covered.length ? `, covered: ${covered.slice(0, 3).join('; ')}` : ''}${other.size ? `, other toasts: ${JSON.stringify([...other])}` : ''}; ` +
        `after the result: toolbar ${after.bar ? 'shown' : 'hidden'}, toasts ${JSON.stringify(after.toasts)}`);
    // The winner takes over with its next frame, then the other canvases go.
    await until(() => page.evaluate(() => document.querySelectorAll('#stage canvas').length === 1), 5000, 'one canvas').catch(() => {});
    await sleep(600); // a stats update from the winner
    const st = await page.evaluate(() => window.__recon.lastStats);
    const stored = await page.evaluate((k) => JSON.parse(localStorage.getItem(k) || 'null'), PRESENT_KEY);
    const canvases = await page.evaluate(() => [...document.querySelectorAll('#stage canvas')].map((c) => `${c.dataset.path}${c.hidden ? ' (hidden)' : ''}`));
    const overlay = await page.textContent('#stats').catch(() => '');
    const res = result?.results || {};
    const rule = result?.rule || {};
    const row = (p) => (res[p]?.error ? `${p}: unavailable (${res[p].error})`
      : `${p}: draw p50/p95 ${res[p]?.draw?.p50}/${res[p]?.draw?.p95} ms (n ${res[p]?.draw?.n}, rounds ${res[p]?.drawRounds?.join('/')}), ` +
        `display p50 ${res[p]?.display?.p50} ms (n ${res[p]?.display?.n}), ${res[p]?.fps} fps, ${res[p]?.errors} errors, desynchronized ${res[p]?.desynchronized}` +
        `${res[p]?.out ? `, out: ${res[p].out}` : ''}`);
    // The rule, from the numbers: out (lost, errors, too few samples, < minShare
    // of the best fps, display p50 more than a refresh above the best), then
    // desynchronized first, then the first path unless another's draw p50 is
    // lower by more than marginMs in every round.
    const measured = all.filter((p) => res[p] && !res[p].lost && !res[p].errors && res[p].draw?.n >= rule.minDraw && res[p].display?.n >= rule.minDisplay);
    let pool = measured;
    const fps = Math.max(0, ...pool.map((p) => res[p].fps));
    if (fps >= 10) pool = pool.filter((p) => res[p].fps >= rule.minShare * fps);
    const disp = Math.min(...pool.map((p) => res[p].display.p50));
    pool = pool.filter((p) => res[p].display.p50 <= disp + rule.refreshMs);
    if (pool.some((p) => res[p].desynchronized === true)) pool = pool.filter((p) => res[p].desynchronized === true);
    const def = pool[0];
    const d = res[def]?.drawRounds || [];
    const clear = pool.filter((p) => p !== def && res[p].drawRounds.every((v, i) => v !== null && d[i] !== null && v + rule.marginMs < d[i]))
      .sort((a, b) => res[a].draw.p50 - res[b].draw.p50);
    const want = clear[0] || def;
    check(`renderer auto: bake-off on the live stream measures ${all.join(', ')} in two rounds, Auto's pick keeps drawing and is stored`,
      !!result?.winner && result.winner === want && measured.length >= 2 && all.every((p) => res[p]?.draw?.n >= 30 && res[p].drawRounds?.length === 2 && res[p].display.n >= 1) &&
        !!result.why && st?.renderer?.name === result.winner && st.renderer.bake?.done && stored?.winner === result.winner && stored.why === result.why &&
        canvases.length === 1 && canvases[0] === result.winner && overlay.includes('bake-off') && overlay.includes('★ ') && overlay.includes(result.why) && st.fps > 20,
      `${page === mainPage ? 'headless' : 'headed'}, ${took.toFixed(1)} s: ${result?.winner} (${result?.why}; rule wants ${want}); ${['canvas2d', 'webgl2', 'webgpu'].map(row).join('; ')}; ` +
        `canvases left: ${canvases.join(', ')}; stored key ${stored?.key}; ${st?.fps?.toFixed(1)} fps after`);
    await checkHygiene('renderer auto (bake-off switches)', calls, st);
    const pc = st?.pacing?.counts;
    // Never on decode; from the worker's refresh callbacks (the watchdog's timer
    // where the emulated GPU starves them while WebGPU presents).
    check('renderer auto: the bake-off in frame pacing Smooth: every path drew on the display refresh, the result names the mode',
      result?.pacing === 'smooth' && stored?.pacing === 'smooth' && st?.pacing?.mode === 'smooth' && pc?.hop === 0 && pc.main === 0 && pc.raf >= 250,
      `result pacing ${result?.pacing}, stored ${stored?.pacing}; draws by tick source this session ${JSON.stringify(pc)}`);
    results.push({ bakeoff: result, stored });
    // The client's first stage report (10 s after the worker started) falls
    // in the bake-off: its frames come from several paths.
    const stagesLine = await until(() => (hostProc.log.slice(log0).match(/msg="latency stages[^\n]*/) || [])[0], 12000, 'stage line').catch(() => '');
    check('renderer auto: the host logs a stage window that mixes paths as renderer=bakeoff', / renderer=bakeoff /.test(stagesLine),
      stagesLine.replace(/^.*?msg=/, '').slice(0, 200));
    await page.evaluate(() => { window.__recon.userClosed = true; });

    await startStream({ path: 'auto', transport: 'auto', renderer: 'auto', fps: 30 });
    await sleep(3000);
    const st2 = await page.evaluate(() => window.__recon.lastStats);
    const canvases2 = await page.evaluate(() => document.querySelectorAll('#stage canvas').length);
    check('renderer auto: the next connection draws with the stored winner at once (no bake-off, one canvas)',
      !!stored && st2?.renderer?.name === stored.winner && st2.renderer.mode === 'auto' && !st2.renderer.bake && canvases2 === 1 && st2.fps > 20,
      `${st2?.renderer?.name} (mode ${st2?.renderer?.mode}, bake-off ${JSON.stringify(st2?.renderer?.bake)}), ${canvases2} canvas, ${st2?.fps?.toFixed(1)} fps`);
    await page.evaluate(() => [...document.querySelectorAll('#drawer button')].find((b) => b.textContent.includes('Measure renderers again')).click());
    const cleared = await page.evaluate((k) => localStorage.getItem(k), PRESENT_KEY);
    check('renderer auto: "Measure renderers again" clears the stored result', cleared === null);
    await page.evaluate(() => { window.__recon.userClosed = true; });

    // A stored WebGL2 pick that stops drawing (its context lost: the worker's
    // test hook) is given up after 30 failed draws in a row.
    if (res.webgl2?.error || !stored) return;
    await page.goto(`${base}/`);
    await page.evaluate(([k, rec]) => localStorage.setItem(k, JSON.stringify(rec)), [PRESENT_KEY, { ...stored, winner: 'webgl2', why: 'set by the test' }]);
    await startStream({ path: 'auto', transport: 'auto', renderer: 'auto', fps: 30 });
    const gl = await until(() => page.evaluate(() => (window.__recon.lastStats?.renderer?.name === 'webgl2' ? window.__recon.lastStats : null)), 10000, 'webgl2 drawing').catch(() => null);
    await page.evaluate(() => window.__recon.worker.postMessage({ type: 'loseContext' }));
    const t1 = Date.now();
    const back = await until(() => page.evaluate(() => {
      const r = window.__recon;
      return r.streaming && r.lastStats?.renderer?.name === 'canvas2d' && r.lastStats.fps > 0 ? r.lastStats : null;
    }), 20000, '2D after the lost context').catch(() => null);
    const took2 = (Date.now() - t1) / 1000;
    await sleep(1500);
    const st3 = await page.evaluate(() => window.__recon.lastStats);
    const after3 = await page.evaluate((k) => ({
      stored: localStorage.getItem(k), canvases: document.querySelectorAll('#stage canvas').length, present: window.__recon.present,
      log: window.__recon.logs.filter((l) => /render error|reconnecting with the 2D canvas/.test(l)).map((l) => l.replace(/^\S+ /, '')),
    }), PRESENT_KEY);
    check('renderer auto: a picked path that stops drawing (WebGL2 context lost) is forgotten and the client reconnects with the 2D canvas',
      !!gl && !!back && after3.stored === null && after3.canvases === 1 && after3.present?.mode === 'auto' && st3?.renderer?.name === 'canvas2d' && st3.fps > 20 &&
        after3.log.some((l) => l.includes('reconnecting with the 2D canvas')),
      `before: ${gl ? `webgl2 at ${gl.fps?.toFixed(1)} fps` : 'webgl2 not drawing'}; 2D after ${took2.toFixed(1)} s at ${st3?.fps?.toFixed(1)} fps, stored ${after3.stored}, ` +
        `${after3.canvases} canvas; log: ${after3.log.slice(0, 2).join(' | ')}`);
    await page.evaluate(() => { window.__recon.userClosed = true; });
  } finally {
    page = mainPage;
  }
}

// Client-side upscaling in the stream (Phase 5): the WebGPU renderer shows the
// test stream at half the canvas size each way (FSR_STREAM: 480x270 on the
// headed page's 960x540 canvas) with upscaling Auto (2x, above 1.05x): FSR 1
// draws, as the stats and the overlay say (input -> output in device pixels,
// sharpness), and the scenario's checks before this one ran with it (steady
// real-time playback, stage bookkeeping, crop, the frame barcode, which is
// read from the frame's own texture, not from the upscaled canvas). The
// host logs the stage window with upscale=fsr. Then the drawer's Upscaling
// "Off", applied live: the plain bilinear path draws again at the stream's
// rate, and the choice is saved with the settings. The draw stage with and
// without FSR and the passes' GPU time (timestamp-query) go to the results.
async function checkUpscaleStream(sc, rate) {
  const name = sc.name;
  const st = await page.evaluate(() => window.__recon.lastStats);
  const u = st?.renderer?.upscale;
  const canvas = st?.renderer?.canvas;
  const size = sc.size.join('x');
  const overlay = await page.textContent('#stats').catch(() => '');
  const want = `FSR 1 · ${sc.size.join('×')} → ${canvas?.join('×')} (2×) · sharpness 0.2`;
  check(`${name}: upscaling Auto draws with FSR 1, ${size} -> the canvas (device pixels, 2x), sharpness 0.2, shown in the overlay`,
    !!u && u.mode === 'auto' && u.active && u.in?.join('x') === size && canvas?.[0] === 2 * sc.size[0] && canvas?.[1] === 2 * sc.size[1] &&
      u.out?.join('x') === canvas.join('x') && u.sharpness === 0.2 && overlay.includes(want),
    `${JSON.stringify({ mode: u?.mode, active: u?.active, in: u?.in, out: u?.out, scale: u?.scale, sharpness: u?.sharpness, input: u?.input, why: u?.why })}; ` +
      `canvas ${canvas?.join('x')}; overlay ${overlay.includes(want) ? `shows "${want}"` : 'lacks it'}`);
  const hostProc = procs.find((p) => p.spawnargs.includes('run'));
  const line = await until(() => (hostProc.log.match(/msg="latency stages[^\n]* renderer=webgpu [^\n]*upscale=fsr[^\n]*/) || [])[0], 12000, 'stage line with upscale=fsr').catch(() => '');
  check(`${name}: the host logs the client's stage window with upscale=fsr`, !!line, line.replace(/^.*?msg=/, '').slice(0, 220));
  // Off, from the drawer: applies at once.
  await page.evaluate(() => {
    const sel = [...document.querySelectorAll('#drawer label')].find((l) => l.textContent === 'Upscaling')?.parentElement.querySelector('select');
    sel.value = 'off';
    sel.dispatchEvent(new Event('change'));
  });
  const switched = await until(() => page.evaluate(() => {
    const u2 = window.__recon.lastStats?.renderer?.upscale;
    return u2 && u2.mode === 'off' && !u2.active ? true : null;
  }), 5000, 'upscaling off').catch(() => false);
  await sleep(2000);
  const fps = [];
  for (let i = 0; i < 4; i++) {
    await sleep(500);
    fps.push(+(await page.evaluate(() => window.__recon.lastStats?.fps ?? 0)).toFixed(1));
  }
  const st2 = await page.evaluate(() => window.__recon.lastStats);
  const u2 = st2?.renderer?.upscale;
  const overlay2 = await page.textContent('#stats').catch(() => '');
  const saved = await page.evaluate(() => JSON.parse(localStorage.getItem('recon.prefs.v1') || '{}').upscale);
  const avg = fps.slice(-3).reduce((a, x) => a + x, 0) / 3;
  const want2 = `Off: bilinear · ${sc.size.join('×')} → ${canvas?.join('×')}`;
  check(`${name}: upscaling Off, applied live: the plain bilinear path draws again at the stream's rate, saved with the settings`,
    switched && u2?.mode === 'off' && !u2.active && u2.why === 'off' && overlay2.includes(want2) && avg >= (rate * 5) / 6 && saved === 'off',
    `${JSON.stringify({ mode: u2?.mode, active: u2?.active, why: u2?.why })}; overlay ${overlay2.includes(want2) ? `shows "${want2}"` : 'lacks it'}; ` +
      `${fps.join(' / ')} fps of ${rate}; saved upscale ${saved}`);
  const ms = (x) => (x ? `mean ${x.mean}, p50 ${x.p50}, p95 ${x.p95} ms (n ${x.n})` : '—');
  const g = u2?.gpu;
  const c = u2?.cpu;
  console.log(`  ${name}: draw stage (CPU) with FSR ${ms(c?.fsr)}; plain ${ms(c?.plain)}. GPU (${g?.method || 'no timestamp-query'}, SwiftShader: ` +
    `not a GPU's cost) FSR ${ms(g?.fsr)}${g?.copy ? `, of which the copy ${ms(g.copy)}` : ''}; plain ${ms(g?.plain)}`);
  results.push({ upscale: name, fsr: { fps: st?.fps, stages: st?.stages, info: u }, off: { fps, info: u2 }, drawStage: { fsr: c?.fsr, plain: c?.plain }, gpu: g });
}

// A headed Chromium on its own Xvfb display (WebGPU works there, not in
// headless here), signed in like the main page; null without Xvfb.
let headed = null;
async function headedPage() {
  if (headed) return headed.page;
  if (spawnSync('sh', ['-c', 'command -v Xvfb']).status !== 0) return null;
  const disp = await startXvfb('1920x1080x24');
  const b = await chromium.launch({
    headless: false, env: { ...process.env, DISPLAY: disp },
    args: ['--autoplay-policy=no-user-gesture-required', '--enable-unsafe-webgpu', '--ignore-gpu-blocklist'],
  });
  const c = await b.newContext({ ignoreHTTPSErrors: true, viewport: HEADED_VIEWPORT, deviceScaleFactor: HEADED_DPR, storageState: await ctx.storageState() });
  const p = await c.newPage();
  p.on('console', onConsole);
  p.on('pageerror', onPageError);
  headed = { browser: b, page: p };
  return p;
}

// Decoder hygiene (step 4.1). The stream worker's VideoDecoder calls are
// counted by patching VideoDecoder.prototype in the worker itself (Playwright
// evaluates in workers), independent of the worker's own bookkeeping:
// decodeQueueSize right after every decode() (the client keeps it at 2 or
// less) and flush() calls (never while streaming). Returns a function that
// reads the counts.
async function watchDecoder() {
  const w = page.workers().filter((x) => x.url().endsWith('/js/stream-worker.js')).pop();
  if (!w) return null;
  await w.evaluate(() => {
    const h = { decodes: 0, maxQueue: 0, flushes: 0 };
    self.__decoderCalls = h;
    const { decode, flush } = VideoDecoder.prototype;
    VideoDecoder.prototype.decode = function (chunk) {
      decode.call(this, chunk);
      h.decodes++;
      h.maxQueue = Math.max(h.maxQueue, this.decodeQueueSize);
    };
    VideoDecoder.prototype.flush = function () { h.flushes++; return flush.call(this); };
  });
  return () => w.evaluate(() => self.__decoderCalls).catch(() => null);
}

// The counts above plus the worker's VideoFrame count (read from the frames:
// a closed frame has coded width 0): at most 5 open at once (the new output,
// one waiting to be drawn, the WebGPU renderer's previous frame, two latency
// probe clones), none leaked.
async function checkHygiene(name, calls, st) {
  const c = calls ? await calls() : null;
  const vf = st?.videoFrames;
  const ok = !!c && c.decodes >= 50 && c.maxQueue <= 2 && c.flushes === 0 && !!vf && vf.leaked === 0 && vf.max <= 5 && vf.open <= 4;
  check(`${name}: decoder hygiene: decodeQueueSize <= 2, no flush(), every VideoFrame closed`, ok,
    `${c ? `${c.decodes} decode() calls, decodeQueueSize max ${c.maxQueue}, ${c.flushes} flush()` : 'worker not instrumented'}; ` +
      `VideoFrames open ${vf?.open}, max ${vf?.max}, leaked ${vf?.leaked}; chunks waiting max ${st?.waitingMax}; superseded ${st?.superseded} decoded, ${st?.supersededChunks} undecoded; output lag ${st?.outputLag}`);
  results.push({ hygiene: name, calls: c, videoFrames: vf, waitingMax: st?.waitingMax, superseded: st?.superseded, supersededChunks: st?.supersededChunks, outputLag: st?.outputLag });
  return c;
}

// The startup decoder self-test on this browser's real decoders: every family
// it decodes outputs the first frame of the P-only clip after one chunk and
// holds nothing back; the overlay lists the results.
async function checkSelfTest(cfg) {
  const tests = await page.evaluate(() => window.__recon.decoderTest);
  const res = (t) => t.hw || t.sw;
  const ok = tests?.length >= 1 && tests.some((t) => t.family === cfg?.family) &&
    tests.every((t) => res(t)?.ok && res(t).firstAfter === 1 && res(t).held === 0 && res(t).outputs === res(t).frames && !t.software);
  check('decoder self-test at startup: first output after one chunk on every family this browser decodes', ok,
    tests ? tests.map((t) => `${t.text} (${res(t)?.accel}: first after ${res(t)?.firstAfter}, held ${res(t)?.held}, ${res(t)?.outputs}/${res(t)?.frames} out, first ${res(t)?.firstMs} ms)`).join('; ') : 'no result');
  const overlay = await page.textContent('#stats').catch(() => '');
  const took = await page.evaluate(() => window.__recon.decoderTestMs);
  check('overlay shows the decoder self-test with its duration, queue and VideoFrame rows', overlay.includes(`Decoder self-test (${took} ms)`) &&
    overlay.includes('Decoder queue') && overlay.includes('VideoFrames open'), `self-test took ${took} ms`);
  results.push({ selfTest: tests, selfTestMs: took });
  await checkHelloTiming(tests, overlay);
}

// Codec selection (step 4.2): every family whose decoder passed is timed on
// the 1920x1080 clip with the decoder the stream uses; the hello the worker
// sent carries those times, and the host received them: its "session started"
// line lists each family's time, and its "codec choice" line names the
// encoder this stream runs and why.
async function checkHelloTiming(tests, overlay) {
  const hello = await page.evaluate(() => window.__recon.helloDecoders);
  const res = (t) => (t.software ? t.sw : t.hw || t.sw);
  const timedOK = (tm, t) => !!tm && tm.ms > 0 && tm.w === 1920 && tm.h === 1080 && tm.n >= 4 && tm.accel === res(t)?.accel;
  check('decoder timing: every family that passed is timed on the 1920x1080 clip with the decoder the stream uses', tests?.length >= 1 &&
    tests.every((t) => !res(t)?.ok || timedOK(t.timing, t)) && overlay.includes('timed 1080p'),
  (tests || []).map((t) => `${t.family}: ${t.timing ? `${t.timing.ms} ms/frame (${t.timing.accel}, ${t.timing.n} frames)` : 'not timed'}`).join('; '));
  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
  check('the hello carries the decode times', !!hello?.length && tests.every((t) => same(hello.find((d) => d.family === t.family)?.timing ?? null, t.timing ?? null)) &&
    hello.some((d) => d.timing), JSON.stringify(hello));
  const hostProc = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  const started = (hostProc.log.match(/msg="session started"[^\n]*/g) || []).pop() || '';
  const choice = (hostProc.log.match(/msg="codec choice"[^\n]*/g) || []).pop() || '';
  const cfg = await page.evaluate(() => window.__recon.videoCfg);
  const inLog = (d) => started.includes(`${d.family}:${d.hw ? 'hw' : 'sw'}:${d.timing ? `${d.timing.ms.toFixed(2)}ms@${d.timing.w}x${d.timing.h}` : '-'}`);
  check('host logs the hello\'s decode times and its codec choice with the reason', !!hello?.length && hello.every(inLog) &&
    choice.includes(`encoder=${cfg?.encoder} `) && /reason="auto, [^"]+"/.test(choice),
  `${started.replace(/^.*?decoders=/, 'decoders=')}; ${choice.replace(/^.*?encoder=/, 'encoder=').slice(0, 220)}`);
  results.push({ helloDecoders: hello, hostCodecChoice: choice });
}

// The self-test's logic on a decoder that holds frames back: a wrapper around
// this browser's VideoDecoder that outputs frame i only once frame i + hold is
// out (claiming hardware support; with hwOnly it holds only when configured
// prefer-hardware; with delay every output comes that many ms late, a slow
// decoder that holds nothing). Also how deep a burst of the clip's chunks gets
// into the bare decoder's queue (why the client bounds it).
async function checkSelfTestLogic() {
  const ctx2 = await browser.newContext({ ignoreHTTPSErrors: true });
  try {
    const p = await ctx2.newPage();
    await p.goto(`${base}/login`);
    const res = await p.evaluate(async () => {
      const T = await import('/js/decoder-selftest.js');
      const cfgOf = (f) => ({ codec: T.CLIPS[f].codec, codedWidth: T.CLIPS[f].width, codedHeight: T.CLIPS[f].height, optimizeForLatency: true });
      let fam = null;
      for (const f of ['av1', 'h264', 'hevc']) {
        if ((await VideoDecoder.isConfigSupported(cfgOf(f)).catch(() => ({}))).supported) { fam = f; break; }
      }
      if (!fam) return { error: 'no clip decodable' };
      const fake = (hold, hwOnly = false, delay = 0) => class {
        static async isConfigSupported(c) { return { supported: true, config: c }; }
        constructor({ output, error }) {
          this.held = [];
          const out = delay ? (f) => setTimeout(() => output(f), delay) : output;
          this.d = new VideoDecoder({ output: (f) => { this.held.push(f); while (this.held.length > this.hold) out(this.held.shift()); }, error });
        }
        configure(c) {
          this.hold = !hwOnly || c.hardwareAcceleration === 'prefer-hardware' ? hold : 0;
          this.d.configure({ ...c, hardwareAcceleration: 'no-preference' });
        }
        decode(c) { this.d.decode(c); }
        close() { for (const f of this.held) f.close(); this.d.close(); }
      };
      const good = await T.selfTestDecoder(fam, ['prefer-hardware'], { Decoder: fake(0) });
      const slow = await T.selfTestDecoder(fam, ['prefer-hardware'], { Decoder: fake(0, false, 150) });
      const hold1 = await T.selfTestDecoder(fam, ['prefer-hardware'], { Decoder: fake(1) });
      const hold2 = await T.selfTestDecoder(fam, ['prefer-hardware'], { Decoder: fake(2) });
      const [choice] = await T.runSelfTests([{ family: fam, hw: true }], true, { Decoder: fake(1, true) });
      // Burst: all ten chunks at once into a bare decoder.
      const d = new VideoDecoder({ output: (f) => f.close(), error: () => {} });
      d.configure(cfgOf(fam));
      await new Promise((r) => setTimeout(r, 200));
      let burst = 0;
      T.CLIPS[fam].frames.forEach((b, i) => {
        const data = Uint8Array.from(atob(b), (ch) => ch.charCodeAt(0));
        d.decode(new EncodedVideoChunk({ type: i ? 'delta' : 'key', timestamp: i, data }));
        burst = Math.max(burst, d.decodeQueueSize);
      });
      d.close();
      // Timing (step 4.2), on a fake decoder that outputs each frame after a
      // fixed delay per codec at 1920x1080 (`small` ms at the hygiene clip's
      // size; no real decoding: every family): the median decode() ->
      // output, the families interleaved frame by frame, never two 1080p
      // decodes at once, within the budget.
      let order = [];
      let inFlight = 0;
      let maxInFlight = 0;
      const pure = (delays, small = 10) => class {
        static async isConfigSupported(c) { return { supported: true, config: c }; }
        constructor({ output }) { this.output = output; this.closed = false; }
        configure(c) {
          this.big = c.codedWidth === 1920;
          this.tag = c.codec.slice(0, 4);
          this.delay = this.big ? delays[this.tag] : small;
        }
        decode(chunk) {
          const timestamp = chunk.timestamp;
          const big = this.big;
          if (big) { order.push(this.tag); inFlight++; maxInFlight = Math.max(maxInFlight, inFlight); }
          setTimeout(() => { if (big) inFlight--; if (!this.closed) this.output({ timestamp, close() {} }); }, this.delay);
        }
        close() { this.closed = true; }
      };
      const fams = ['h264', 'hevc', 'av1'];
      const even = pure({ avc1: 24, hev1: 12, av01: 40 });
      const one = await T.timeDecoder('av1', 'prefer-hardware', { Decoder: even });
      order = [];
      const all = await T.runSelfTests(fams.map((family) => ({ family, hw: true })), true, { Decoder: even });
      const interleaved = order.length === 24 && new Set(order.slice(0, 3)).size === 3 && order.every((c, i) => i < 3 || c === order[i - 3]);
      const allOrder = order.join(' ');
      const allInFlight = maxInFlight;
      const hello = all.map((t) => T.helloDecoder({ family: t.family, hw: true }, t));
      // Budget: AV1 at 200 ms per 1080p frame runs out of its 500 ms (key
      // frame + one P frame); a decoder at 400 ms per frame in every family
      // (and 50 ms on the hygiene clip) costs at most the timing budget; a
      // clip import that never finishes costs the budget.
      let t0 = performance.now();
      const slowAV1 = await T.timeDecoders(fams.map((family) => ({ family, accel: 'prefer-hardware' })), { Decoder: pure({ avc1: 24, hev1: 12, av01: 200 }) });
      const slowAV1Ms = Math.round(performance.now() - t0);
      t0 = performance.now();
      const allSlow = await T.runSelfTests(fams.map((family) => ({ family, hw: true })), true, { Decoder: pure({ avc1: 400, hev1: 400, av01: 400 }, 50) });
      const allSlowMs = Math.round(performance.now() - t0);
      t0 = performance.now();
      const noClips = await T.timeDecoders([{ family: 'av1', accel: 'prefer-hardware' }], { Decoder: even, timingClips: new Promise(() => {}), timingBudgetMs: 300 });
      const noClipsMs = Math.round(performance.now() - t0);
      return {
        fam, good, slow, hold1, hold2, choice, burst,
        timing: {
          one, all: all.map((t) => ({ family: t.family, timing: t.timing, text: t.text })), order: allOrder, interleaved, maxInFlight: allInFlight, hello,
          slowAV1, slowAV1Ms, allSlow: allSlow.map((t) => ({ family: t.family, ok: (t.hw || t.sw)?.ok, timing: t.timing })), allSlowMs, noClips, noClipsMs,
          budget: T.TIMING_BUDGET_MS,
        },
      };
    });
    if (res.error) { check('decoder self-test logic', false, res.error); return; }
    const { good, slow, hold1, hold2, choice } = res;
    const row = (r) => `ok ${r.ok}, first output after ${r.firstAfter}, held ${r.held}, ${r.outputs}/${r.sent} out`;
    check('decoder self-test logic: passes a decoder that outputs at once or slowly (150 ms per frame), catches one that holds 1 or 2 frames back',
      good.ok && good.firstAfter === 1 && good.held === 0 && good.outputs === 10 &&
        slow.ok && slow.firstAfter === 1 && slow.held === 0 && slow.outputs === slow.sent &&
        !hold1.ok && hold1.firstAfter === 2 && hold1.held === 1 && !hold2.ok && hold2.firstAfter === 3 && hold2.held === 2,
      `${res.fam}: outputs at once: ${row(good)}; 150 ms late: ${row(slow)}, ${slow.decodeMs} ms/frame; holds 1: ${row(hold1)}; holds 2: ${row(hold2)}`);
    check('decoder self-test decision: a hardware decoder that holds frames back is reported as no hardware decoder; the family decodes in software',
      choice.software === true && choice.reportHW === false && choice.hw?.held === 1 && choice.sw?.ok === true, choice.text);
    console.log(`- the clip's 10 chunks at once into a bare decoder (${res.fam}) reach decodeQueueSize ${res.burst}`);
    const tm = res.timing;
    const near = (x, ms) => !!x && x.ms >= ms && x.ms < ms + 15 && x.w === 1920 && x.h === 1080 && x.n === 7;
    const by = (f) => tm.all.find((t) => t.family === f)?.timing;
    check('decoder timing logic: the median decode() -> output of the 1920x1080 clip, families interleaved frame by frame, one decode at a time, times in the hello',
      near(tm.one, 40) && tm.one.accel === 'prefer-hardware' && near(by('h264'), 24) && near(by('hevc'), 12) && near(by('av1'), 40) &&
        tm.interleaved && tm.maxInFlight === 1 && tm.hello.every((d) => d.hw === true && d.timing?.ms === by(d.family)?.ms),
      `fake decoder 40 ms: ${tm.one?.ms} ms/frame over ${tm.one?.n}; ${tm.all.map((t) => t.text).join('; ')}; decode order ${tm.order.slice(0, 29)}…, at most ${tm.maxInFlight} in flight`);
    const slowOK = near(tm.slowAV1.h264, 24) && near(tm.slowAV1.hevc, 12) && !tm.slowAV1.av1 && tm.slowAV1Ms < tm.budget;
    const allSlowOK = tm.allSlow.length === 3 && tm.allSlow.every((t) => t.ok && t.timing === null) && tm.allSlowMs < 1000 + tm.budget + 500;
    check('decoder timing budget: a family over its 500 ms goes untimed, slow decoders cost at most the timing budget, a clip import that never finishes too',
      slowOK && allSlowOK && Object.keys(tm.noClips).length === 0 && tm.noClipsMs >= 290 && tm.noClipsMs < 600,
      `AV1 at 200 ms/frame: ${JSON.stringify(tm.slowAV1)} in ${tm.slowAV1Ms} ms; every family 400 ms/frame (50 ms on the hygiene clip): whole self-test ${tm.allSlowMs} ms, ` +
        `timed ${tm.allSlow.filter((t) => t.timing).length}/3; no clips: ${JSON.stringify(tm.noClips)} after ${tm.noClipsMs} ms`);
    results.push({ selfTestLogic: res });
  } finally {
    await ctx2.close();
  }
}

let testPage = null; // headed browser showing tools/latency-test (wallclock scenario)

// Starts an X server and returns its display (":N").
async function startXvfb(screen = '1280x720x24') {
  const xvfb = spawn('Xvfb', ['-displayfd', '3', '-screen', '0', screen, '-nolisten', 'tcp'], { stdio: ['ignore', 'ignore', 'pipe', 'pipe'] });
  xvfb.log = '';
  procs.push(xvfb);
  return new Promise((res, rej) => {
    let b = '';
    xvfb.stdio[3].on('data', (d) => { b += d; if (b.includes('\n')) res(`:${b.trim()}`); });
    xvfb.on('exit', () => rej(new Error('Xvfb exited')));
    setTimeout(() => rej(new Error('Xvfb did not start')), 10000);
  });
}

// Renderers at unit level (steps 1.7, 4.1, 4.3): the client's own renderers
// (web/static/js/renderers.js: 2D, WebGL2, WebGPU) draw a 64x40 frame whose
// top-left 48x32 holds four colour quadrants, columns 48-63 grey and rows
// 32-39 white, with the area protocol.js visibleArea() computes for a video
// config that crops the bottom 8 rows, and for one that also crops the right
// 16 columns. Without a box the canvas must be exactly the visible size, with
// no white (and, for the second, no grey). Then the second config into a
// 100x40 box (device pixels): the picture scaled to 60x40 and centred, black
// bars left and right; WebGL2 and WebGPU redraw their last picture into a
// new 64x64 box without a new frame. WebGPU runs in a headed browser on Xvfb:
// in headless Chromium here SwiftShader rejects queue.onSubmittedWorkDone()
// after an external texture import ("A valid external Instance reference no
// longer exists."), so the app's WebGPU self-test fails there.
async function checkRendererCrop(haveX) {
  let b = browser;
  if (haveX) {
    const disp = await startXvfb();
    b = await chromium.launch({ headless: false, env: { ...process.env, DISPLAY: disp }, args: ['--enable-unsafe-webgpu', '--ignore-gpu-blocklist'] });
  }
  // Evaluated through the DevTools protocol, which the page's CSP does not
  // restrict; the modules come from the gateway like in the app.
  const expr = `(async () => {
const P = await import('/js/protocol.js');
const R = await import('/js/renderers.js');
const src = new OffscreenCanvas(64, 40);
const g = src.getContext('2d');
g.fillStyle = '#fff'; g.fillRect(0, 0, 64, 40);
g.fillStyle = '#808080'; g.fillRect(48, 0, 16, 32);
[['#f00', '#0f0'], ['#00f', '#ff0']].forEach((row, y) => row.forEach((c, x) => { g.fillStyle = c; g.fillRect(x * 24, y * 16, 24, 16); }));
const pixels = async (r, canvas) => {
  if (r.device) await r.device.queue.onSubmittedWorkDone();
  const bmp = canvas.transferToImageBitmap();
  const c2 = new OffscreenCanvas(bmp.width, bmp.height).getContext('2d');
  c2.drawImage(bmp, 0, 0);
  const d = c2.getImageData(0, 0, bmp.width, bmp.height).data;
  let white = 0, grey = 0;
  for (let i = 0; i < d.length; i += 4) {
    if (d[i] > 200 && d[i + 1] > 200 && d[i + 2] > 200) white++;
    if (Math.abs(d[i] - 128) < 30 && Math.abs(d[i + 1] - 128) < 30 && Math.abs(d[i + 2] - 128) < 30) grey++;
  }
  const at = (x, y) => [...d.subarray((y * bmp.width + x) * 4, (y * bmp.width + x) * 4 + 3)];
  return { w: bmp.width, h: bmp.height, white, grey, at };
};
const near = (px, want) => px.every((v, i) => Math.abs(v - want[i]) < 40);
const quads = (p, x0, y0, cw, ch) => [[0, 0, [255, 0, 0]], [1, 0, [0, 255, 0]], [0, 1, [0, 0, 255]], [1, 1, [255, 255, 0]]]
  .every(([qx, qy, c]) => near(p.at(Math.floor(x0 + (qx + 0.5) * cw), Math.floor(y0 + (qy + 0.5) * ch)), c));
const cropped = { width: 48, height: 32, codedWidth: 64, codedHeight: 40, cropRight: 16, cropBottom: 8 };
const out = [];
for (const kind of R.PATHS) {
  for (const cfg of [{ width: 64, height: 32, codedWidth: 64, codedHeight: 40, cropBottom: 8 }, cropped]) {
    const canvas = new OffscreenCanvas(1, 1);
    let r;
    try { r = await R.createRenderer(kind, canvas); } catch (e) { out.push({ kind, cfg, error: e.message }); continue; }
    const frame = new VideoFrame(src, { timestamp: 0 });
    r.draw(frame, null, P.visibleArea(cfg, frame.visibleRect.width, frame.visibleRect.height, frame.displayWidth, frame.displayHeight));
    const p = await pixels(r, canvas);
    out.push({ kind, cfg, w: p.w, h: p.h, white: p.white, grey: p.grey, desync: r.desynchronized });
    r.destroy();
  }
  // Letterbox into a 100x40 box, then (WebGL2, WebGPU) a redraw into 64x64.
  const canvas = new OffscreenCanvas(1, 1);
  let r;
  try { r = await R.createRenderer(kind, canvas); } catch (e) { out.push({ kind, box: true, error: e.message }); continue; }
  r.resize(100, 40);
  const frame = new VideoFrame(src, { timestamp: 0 });
  r.draw(frame, null, P.visibleArea(cropped, 64, 40, 64, 40));
  const p = await pixels(r, canvas);
  const box = { kind, box: true, w: p.w, h: p.h, white: p.white, grey: p.grey, rect: r.rect,
    bars: near(p.at(10, 20), [0, 0, 0]) && near(p.at(90, 20), [0, 0, 0]), quads: quads(p, 20, 0, 30, 20) };
  if (kind !== 'canvas2d') {
    r.resize(64, 64);
    r.redraw();
    const q = await pixels(r, canvas);
    // 48x32 into 64x64: 64x43 at y 10.
    box.redraw = { w: q.w, h: q.h, quads: quads(q, 0, 10, 32, 21), bars: near(q.at(32, 4), [0, 0, 0]) && near(q.at(32, 60), [0, 0, 0]), white: q.white };
  }
  out.push(box);
  r.destroy();
}
// Frames the renderer leaves open after drawing three in a row (closed: coded width 0).
const keep = [];
for (const kind of R.PATHS) {
  let r;
  try { r = await R.createRenderer(kind, new OffscreenCanvas(1, 1)); } catch (e) { keep.push({ kind, error: e.message }); continue; }
  const fs = [0, 1, 2].map((i) => new VideoFrame(src, { timestamp: i }));
  for (const f of fs) r.draw(f, null, P.visibleArea(null, 64, 40, 64, 40));
  if (r.device) await r.device.queue.onSubmittedWorkDone();
  keep.push({ kind, open: fs.map((f, i) => (f.codedWidth ? i : -1)).filter((i) => i >= 0), prevIsLast: r.prev === fs[2] });
  r.prev?.close();
  r.destroy();
}
return { out, keep };
})()`;
  const ctx2 = await b.newContext({ ignoreHTTPSErrors: true });
  try {
    const p = await ctx2.newPage();
    await p.goto(`${base}/login`);
    const { out: res, keep } = await p.evaluate(expr);
    for (const kind of ['canvas2d', 'webgl2', 'webgpu']) {
      const rows = res.filter((x) => x.kind === kind && !x.box);
      const box = res.find((x) => x.kind === kind && x.box);
      const skipped = kind === 'webgpu' && !haveX && rows.every((x) => x.error);
      if (skipped) {
        console.log(`- renderer unit checks (${kind}): skipped, WebGPU needs a headed browser (Xvfb) here: ${rows[0].error}`);
        continue;
      }
      const ok = rows.length === 2 && rows.every((x) => !x.error && x.w === x.cfg.width && x.h === x.cfg.height && x.white === 0 &&
        (x.cfg.width === 64 ? x.grey === 16 * 32 : x.grey === 0));
      check(`renderer crop (${kind}): draws only the visible area announced by the video config`, ok,
        rows.map((x) => (x.error ? x.error : `${x.cfg.width}x${x.cfg.height} of 64x40: canvas ${x.w}x${x.h}, ${x.white} white, ${x.grey} grey px`)).join('; ') +
          (rows[0]?.desync !== undefined ? `; getContextAttributes().desynchronized ${rows[0].desync}` : ''));
      const rd = box?.redraw;
      check(`renderer letterbox (${kind}): canvas = the device-pixel box, picture scaled to fit and centred${kind === 'canvas2d' ? '' : ', redrawn into a new box without a new frame'}`,
        !!box && !box.error && box.w === 100 && box.h === 40 && box.rect?.x === 20 && box.rect?.w === 60 && box.rect?.h === 40 && box.bars && box.quads && box.white === 0 && box.grey === 0 &&
          (kind === 'canvas2d' || (rd && rd.w === 64 && rd.h === 64 && rd.quads && rd.bars && rd.white === 0)),
        box?.error || `canvas ${box?.w}x${box?.h}, picture ${JSON.stringify(box?.rect)}, bars black ${box?.bars}, quadrants ${box?.quads}, ${box?.white} white, ${box?.grey} grey` +
          (rd ? `; redraw: canvas ${rd.w}x${rd.h}, quadrants ${rd.quads}, bars ${rd.bars}, ${rd.white} white` : ''));
      // Step 4.1: every frame closed once drawn; WebGPU keeps exactly the last (this.prev).
      const k = keep.find((x) => x.kind === kind);
      check(`renderer (${kind}) closes every frame it drew${kind === 'webgpu' ? ' except the last (this.prev)' : ''}`,
        !k.error && (kind === 'webgpu' ? k.open.length === 1 && k.open[0] === 2 && k.prevIsLast : k.open.length === 0),
        k.error || `of 3 frames drawn, open: [${k.open}]${kind === 'webgpu' ? `, prev is the last: ${k.prevIsLast}` : ''}`);
    }
  } finally {
    await ctx2.close();
    if (b !== browser) await b.close();
  }
}

// Client-side upscaling at unit level (Phase 5, web/static/js/fsr1.js): the
// WebGPU renderer's FSR 1 passes (EASU, then RCAS onto the canvas) against
// fsrReference below, a CPU port of the same math written from ffx_fsr1.h
// itself (its gather4 layout, constants and approximations), not from the
// client's WGSL. The picture (fsrPattern, 64x40): an anti-aliased diagonal
// edge between dark blue and orange, a 1-px white line on dark grey, a smooth
// grey ramp. It is drawn at 2x and 1.5x with the external-texture and the
// copy input, sharpness 0 to 1 stops, denoise, and from a frame padded with 8
// white rows that the video config crops; the plain path at 1x must return
// the source exactly (the reference's input). Tolerance: 1 level for EASU
// alone (RCAS at 20 stops is the identity), 5 levels after RCAS, which
// amplifies a 1-level rounding difference of the 8-bit intermediate up to
// 1 / (1 + 4 * lobe), about 2.9x at 0.2 stops. Then against the bilinear
// path: the edge's 10-90 % rise is narrower and its steepest step higher,
// flat areas (3 input pixels from anything else) keep their value within a
// level (no ringing: RCAS's limiter and EASU's min/max clamp), the ramp stays
// a ramp. Then the streaming geometry the sandbox cannot run in real time
// (960x540 into 1920x1080, Auto) at 600 sampled pixels. And the plan: FSR
// never draws a picture shown at its size or smaller, Auto only above 1.05x.
// n output pixels spread over a w x h picture (a fixed sequence).
function fsrSamples(w, h, n) {
  let s = 12345;
  const rnd = () => { s = (s * 1103515245 + 12345) % 2147483648; return s / 2147483648; };
  return Array.from({ length: n }, () => [Math.floor(rnd() * w), Math.floor(rnd() * h)]);
}

function fsrPattern(w, h, pad = 0) {
  const px = new Uint8ClampedArray(w * (h + pad) * 4).fill(255);
  const set = (x, y, c) => { const o = (y * w + x) * 4; px[o] = c[0]; px[o + 1] = c[1]; px[o + 2] = c[2]; };
  const blue = [20, 30, 90];
  const orange = [230, 140, 40];
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      if (x < 32) {
        // Coverage of the pixel by the half-plane right of x = 8 + y / 2.
        const c = Math.min(1, Math.max(0, ((x + 0.5) - (8 + 0.5 * (y + 0.5))) / Math.hypot(1, 0.5) + 0.5));
        set(x, y, blue.map((v, i) => Math.round(v + (orange[i] - v) * c)));
      } else if (y < 20) set(x, y, x === 47 && y >= 2 && y < 18 ? [235, 235, 235] : [30, 30, 30]);
      else set(x, y, Array(3).fill(Math.round(20 + (215 * (x - 32)) / 31)));
    }
  }
  return px;
}

// FSR 1 on the CPU: EASU then RCAS, ported from ffx_fsr1.h (FsrEasuCon,
// FsrEasuF, FsrRcasCon, FsrRcasF, the 32-bit versions) and ffx_a.h's
// approximations (GPUOpen-Effects/FidelityFX-FSR, see third_party/README.md):
//
// Copyright (c) 2021 Advanced Micro Devices, Inc. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.
//
// src: RGB in [0, 1], w*h*3; returns { at(x, y) }:
// the output pixel's RGB in [0, 1] before the canvas's 8-bit store (EASU is
// evaluated where RCAS needs it and stored as 8-bit, like the client's
// rgba8unorm intermediate). Edges: the gathers clamp to the image (a
// clamp-to-edge sampler), RCAS's loads to the EASU output.
function fsrReference(src, w, h, ow, oh, stops, denoise = false) {
  const f = new Float32Array(1);
  const u = new Uint32Array(f.buffer);
  const AU1_AF1 = (a) => { f[0] = a; return u[0]; };
  const AF1_AU1 = (a) => { u[0] = a >>> 0; return f[0]; };
  const APrxLoRcpF1 = (a) => AF1_AU1(0x7ef07ebb - AU1_AF1(a));
  const APrxMedRcpF1 = (a) => { const b = AF1_AU1(0x7ef19fff - AU1_AF1(a)); return b * (-b * a + 2.0); };
  const APrxLoRsqF1 = (a) => AF1_AU1(0x5f347d74 - (AU1_AF1(a) >>> 1));
  const ARcpF1 = (a) => 1.0 / a;
  const ASatF1 = (a) => Math.min(1.0, Math.max(0.0, a));
  // GPU min/max (HLSL on D3D, GLSL on Vulkan): a NaN operand yields the other one.
  const max = (a, b) => (Number.isNaN(a) ? b : Number.isNaN(b) ? a : Math.max(a, b));
  const min = (a, b) => (Number.isNaN(a) ? b : Number.isNaN(b) ? a : Math.min(a, b));
  // FsrEasuCon(viewport w x h, input size w x h, output ow x oh).
  const con0 = [w * ARcpF1(ow), h * ARcpF1(oh), 0.5 * w * ARcpF1(ow) - 0.5, 0.5 * h * ARcpF1(oh) - 0.5];
  const con1 = [ARcpF1(w), ARcpF1(h), 1.0 * ARcpF1(w), -1.0 * ARcpF1(h)];
  const con2 = [-1.0 * ARcpF1(w), 2.0 * ARcpF1(h), 1.0 * ARcpF1(w), 2.0 * ARcpF1(h)];
  const con3 = [0.0 * ARcpF1(w), 4.0 * ARcpF1(h), 0, 0];
  const texel = (x, y, ch) => src[(Math.min(h - 1, Math.max(0, y)) * w + Math.min(w - 1, Math.max(0, x))) * 3 + ch];
  // textureGather of channel ch at normalised p: the bilinear footprint,
  // x = (i0, j1), y = (i1, j1), z = (i1, j0), w = (i0, j0).
  const gather = (p, ch) => {
    const i0 = Math.floor(p[0] * w - 0.5);
    const j0 = Math.floor(p[1] * h - 0.5);
    return [texel(i0, j0 + 1, ch), texel(i0 + 1, j0 + 1, ch), texel(i0 + 1, j0, ch), texel(i0, j0, ch)];
  };
  const L4 = (R, G, B) => [0, 1, 2, 3].map((k) => B[k] * 0.5 + (R[k] * 0.5 + G[k]));
  const FsrEasuTapF = (acc, off, dir, len, lob, clp, c) => {
    const v = [off[0] * dir[0] + off[1] * dir[1], off[0] * -dir[1] + off[1] * dir[0]];
    v[0] *= len[0];
    v[1] *= len[1];
    const d2 = min(v[0] * v[0] + v[1] * v[1], clp);
    let wB = (2.0 / 5.0) * d2 + -1.0;
    let wA = lob * d2 + -1.0;
    wB *= wB;
    wA *= wA;
    wB = (25.0 / 16.0) * wB + -(25.0 / 16.0 - 1.0);
    const wt = wB * wA;
    for (let k = 0; k < 3; k++) acc.c[k] += c[k] * wt;
    acc.w += wt;
  };
  const FsrEasuSetF = (st, pp, biS, biT, biU, biV, lA, lB, lC, lD, lE) => {
    let wt = 0.0;
    if (biS) wt = (1.0 - pp[0]) * (1.0 - pp[1]);
    if (biT) wt = pp[0] * (1.0 - pp[1]);
    if (biU) wt = (1.0 - pp[0]) * pp[1];
    if (biV) wt = pp[0] * pp[1];
    const dc = lD - lC;
    const cb = lC - lB;
    let lenX = APrxLoRcpF1(max(Math.abs(dc), Math.abs(cb)));
    const dirX = lD - lB;
    st.dir[0] += dirX * wt;
    lenX = ASatF1(Math.abs(dirX) * lenX);
    st.len += lenX * lenX * wt;
    const ec = lE - lC;
    const ca = lC - lA;
    let lenY = APrxLoRcpF1(max(Math.abs(ec), Math.abs(ca)));
    const dirY = lE - lA;
    st.dir[1] += dirY * wt;
    lenY = ASatF1(Math.abs(dirY) * lenY);
    st.len += lenY * lenY * wt;
  };
  const FsrEasuF = (ipx, ipy) => {
    const pp = [ipx * con0[0] + con0[2], ipy * con0[1] + con0[3]];
    const fp = [Math.floor(pp[0]), Math.floor(pp[1])];
    pp[0] -= fp[0];
    pp[1] -= fp[1];
    const p0 = [fp[0] * con1[0] + con1[2], fp[1] * con1[1] + con1[3]];
    const p1 = [p0[0] + con2[0], p0[1] + con2[1]];
    const p2 = [p0[0] + con2[2], p0[1] + con2[3]];
    const p3 = [p0[0] + con3[0], p0[1] + con3[1]];
    const [bczzR, bczzG, bczzB] = [0, 1, 2].map((ch) => gather(p0, ch));
    const [ijfeR, ijfeG, ijfeB] = [0, 1, 2].map((ch) => gather(p1, ch));
    const [klhgR, klhgG, klhgB] = [0, 1, 2].map((ch) => gather(p2, ch));
    const [zzonR, zzonG, zzonB] = [0, 1, 2].map((ch) => gather(p3, ch));
    const [bL, cL] = L4(bczzR, bczzG, bczzB);
    const [iL, jL, fL, eL] = L4(ijfeR, ijfeG, ijfeB);
    const [kL, lL, hL, gL] = L4(klhgR, klhgG, klhgB);
    const [, , oL, nL] = L4(zzonR, zzonG, zzonB);
    const st = { dir: [0, 0], len: 0 };
    FsrEasuSetF(st, pp, true, false, false, false, bL, eL, fL, gL, jL);
    FsrEasuSetF(st, pp, false, true, false, false, cL, fL, gL, hL, kL);
    FsrEasuSetF(st, pp, false, false, true, false, fL, iL, jL, kL, nL);
    FsrEasuSetF(st, pp, false, false, false, true, gL, jL, kL, lL, oL);
    const dir = st.dir;
    let len = st.len;
    let dirR = dir[0] * dir[0] + dir[1] * dir[1];
    const zro = dirR < 1.0 / 32768.0;
    dirR = zro ? 1.0 : APrxLoRsqF1(dirR);
    dir[0] = zro ? 1.0 : dir[0];
    dir[0] *= dirR;
    dir[1] *= dirR;
    len = len * 0.5;
    len *= len;
    const stretch = (dir[0] * dir[0] + dir[1] * dir[1]) * APrxLoRcpF1(max(Math.abs(dir[0]), Math.abs(dir[1])));
    const len2 = [1.0 + (stretch - 1.0) * len, 1.0 + -0.5 * len];
    const lob = 0.5 + ((1.0 / 4.0 - 0.04) - 0.5) * len;
    const clp = APrxLoRcpF1(lob);
    const q = (R, G, B, k) => [R[k], G[k], B[k]];
    const fC = q(ijfeR, ijfeG, ijfeB, 2);
    const gC = q(klhgR, klhgG, klhgB, 3);
    const jC = q(ijfeR, ijfeG, ijfeB, 1);
    const kC = q(klhgR, klhgG, klhgB, 0);
    const min4 = [0, 1, 2].map((ch) => min(min(min(fC[ch], gC[ch]), jC[ch]), kC[ch]));
    const max4 = [0, 1, 2].map((ch) => max(max(max(fC[ch], gC[ch]), jC[ch]), kC[ch]));
    const acc = { c: [0, 0, 0], w: 0 };
    const off = (x, y) => [x - pp[0], y - pp[1]];
    FsrEasuTapF(acc, off(0.0, -1.0), dir, len2, lob, clp, q(bczzR, bczzG, bczzB, 0)); // b
    FsrEasuTapF(acc, off(1.0, -1.0), dir, len2, lob, clp, q(bczzR, bczzG, bczzB, 1)); // c
    FsrEasuTapF(acc, off(-1.0, 1.0), dir, len2, lob, clp, q(ijfeR, ijfeG, ijfeB, 0)); // i
    FsrEasuTapF(acc, off(0.0, 1.0), dir, len2, lob, clp, jC); // j
    FsrEasuTapF(acc, off(0.0, 0.0), dir, len2, lob, clp, fC); // f
    FsrEasuTapF(acc, off(-1.0, 0.0), dir, len2, lob, clp, q(ijfeR, ijfeG, ijfeB, 3)); // e
    FsrEasuTapF(acc, off(1.0, 1.0), dir, len2, lob, clp, kC); // k
    FsrEasuTapF(acc, off(2.0, 1.0), dir, len2, lob, clp, q(klhgR, klhgG, klhgB, 1)); // l
    FsrEasuTapF(acc, off(2.0, 0.0), dir, len2, lob, clp, q(klhgR, klhgG, klhgB, 2)); // h
    FsrEasuTapF(acc, off(1.0, 0.0), dir, len2, lob, clp, gC); // g
    FsrEasuTapF(acc, off(1.0, 2.0), dir, len2, lob, clp, q(zzonR, zzonG, zzonB, 2)); // o
    FsrEasuTapF(acc, off(0.0, 2.0), dir, len2, lob, clp, q(zzonR, zzonG, zzonB, 3)); // n
    return [0, 1, 2].map((ch) => min(max4[ch], max(min4[ch], acc.c[ch] * ARcpF1(acc.w))));
  };
  // EASU on demand (memoised), stored as 8-bit like the rgba8unorm intermediate.
  const easu = new Float64Array(ow * oh * 3).fill(NaN);
  const load = (x, y) => {
    const cx = Math.min(ow - 1, Math.max(0, x));
    const cy = Math.min(oh - 1, Math.max(0, y));
    const o = (cy * ow + cx) * 3;
    if (Number.isNaN(easu[o])) {
      const p = FsrEasuF(cx, cy);
      for (let ch = 0; ch < 3; ch++) easu[o + ch] = Math.round(ASatF1(p[ch]) * 255) / 255;
    }
    return [easu[o], easu[o + 1], easu[o + 2]];
  };
  // FsrRcasCon, FsrRcasF.
  const sharp = Math.pow(2, -stops);
  const FSR_RCAS_LIMIT = 0.25 - 1.0 / 16.0;
  const max3 = (a, b, c) => max(max(a, b), c);
  const min3 = (a, b, c) => min(min(a, b), c);
  const L = (c) => c[2] * 0.5 + (c[0] * 0.5 + c[1]);
  const at = (x, y) => {
    const b = load(x, y - 1);
    const d = load(x - 1, y);
    const e = load(x, y);
    const fv = load(x + 1, y);
    const hv = load(x, y + 1);
    const [bL, dL, eL, fL, hL] = [b, d, e, fv, hv].map(L);
    let nz = 0.25 * bL + 0.25 * dL + 0.25 * fL + 0.25 * hL - eL;
    nz = ASatF1(Math.abs(nz) * APrxMedRcpF1(max3(max3(bL, dL, eL), fL, hL) - min3(min3(bL, dL, eL), fL, hL)));
    nz = -0.5 * nz + 1.0;
    const lobes = [0, 1, 2].map((ch) => {
      const mn4 = min(min3(b[ch], d[ch], fv[ch]), hv[ch]);
      const mx4 = max(max3(b[ch], d[ch], fv[ch]), hv[ch]);
      const hitMin = min(mn4, e[ch]) * ARcpF1(4.0 * mx4);
      const hitMax = (1.0 - max(mx4, e[ch])) * ARcpF1(4.0 * mn4 + -4.0);
      return max(-hitMin, hitMax);
    });
    let lobe = max(-FSR_RCAS_LIMIT, min(max3(lobes[0], lobes[1], lobes[2]), 0.0)) * sharp;
    if (denoise) lobe *= nz;
    const rcpL = APrxMedRcpF1(4.0 * lobe + 1.0);
    return [0, 1, 2].map((ch) => (lobe * b[ch] + lobe * d[ch] + lobe * hv[ch] + lobe * fv[ch] + e[ch]) * rcpL);
  };
  return { at };
}

// Sharpness and flatness of an upscaled fsrPattern (RGBA px, w x h, from W x H):
// the diagonal edge's mean 10-90 % rise along rows (input pixels) and steepest
// step (share of the edge's contrast per output pixel), the output pixels in
// flat areas (the same source colour 3 input pixels all round) and the most
// any of them is off, and the ramp's rows (monotonic; compared with bilinear).
function fsrMetrics(px, w, h, src, W, H) {
  const s = w / W;
  const at = (x, y, c) => px[(y * w + x) * 4 + c];
  const widths = [];
  const steps = [];
  for (let yi = 8; yi < 32; yi += 2) {
    const y = Math.floor((yi + 0.5) * s);
    const row = [];
    for (let x = 0; x < Math.floor(30 * s); x++) row.push((at(x, y, 0) - 20) / 210); // R: blue 20 -> orange 230
    const cross = (t) => { for (let x = 1; x < row.length; x++) if (row[x - 1] < t && row[x] >= t) return x - 1 + (t - row[x - 1]) / (row[x] - row[x - 1]); return NaN; };
    widths.push((cross(0.9) - cross(0.1)) / s);
    steps.push(Math.max(...row.slice(1).map((v, x) => v - row[x])));
  }
  const mean = (a) => a.reduce((x, y) => x + y, 0) / a.length;
  let flat = 0;
  let flatMax = 0;
  for (let y = 0; y < h; y++) {
    for (let x = 0; x < w; x++) {
      const xi = (x + 0.5) / s - 0.5;
      const yi = (y + 0.5) / s - 0.5;
      let ref = null;
      let same = true;
      for (let dy = -3; dy <= 3 && same; dy++) {
        for (let dx = -3; dx <= 3 && same; dx++) {
          const o = (Math.min(H - 1, Math.max(0, Math.round(yi + dy))) * W + Math.min(W - 1, Math.max(0, Math.round(xi + dx)))) * 4;
          if (!ref) ref = [src[o], src[o + 1], src[o + 2]];
          else if (src[o] !== ref[0] || src[o + 1] !== ref[1] || src[o + 2] !== ref[2]) same = false;
        }
      }
      if (!same) continue;
      flat++;
      flatMax = Math.max(flatMax, ...[0, 1, 2].map((k) => Math.abs(at(x, y, k) - ref[k])));
    }
  }
  const ramp = [];
  for (let yi = 24; yi < 36; yi += 3) {
    const y = Math.floor((yi + 0.5) * s);
    ramp.push(Array.from({ length: Math.floor(60.5 * s) - Math.ceil(35.5 * s) }, (_, i) => at(Math.ceil(35.5 * s) + i, y, 0)));
  }
  return { width: +mean(widths).toFixed(3), step: +mean(steps).toFixed(3), flat, flatMax, ramp };
}

async function checkUpscaleUnit(haveX) {
  if (!haveX) {
    console.log('- FSR 1 shader (unit): skipped, WebGPU needs a headed browser (Xvfb) here');
    return;
  }
  const W = 64;
  const H = 40;
  const PAD = 8;
  const plain = fsrPattern(W, H);
  const cases = [
    { name: '1x plain', box: [64, 40], up: { mode: 'off' } },
    { name: '2x', box: [128, 80], up: { mode: 'fsr', sharpness: 0.2, input: 'external' } },
    { name: '2x copy', box: [128, 80], up: { mode: 'fsr', sharpness: 0.2, input: 'copy' } },
    { name: '1.5x', box: [96, 60], up: { mode: 'fsr', sharpness: 0.2, input: 'external' } },
    { name: '1.5x copy 1 stop denoise', box: [96, 60], up: { mode: 'fsr', sharpness: 1, denoise: true, input: 'copy' } },
    { name: '1.5x EASU alone', box: [96, 60], up: { mode: 'fsr', input: 'external' }, stops: 20 },
    { name: '1.5x EASU alone copy', box: [96, 60], up: { mode: 'fsr', input: 'copy' }, stops: 20 },
    { name: '2x cropped frame 0 stops', box: [128, 80], up: { mode: 'fsr', sharpness: 0 }, pad: true },
    { name: '2x bilinear', box: [128, 80], up: { mode: 'off' } },
    { name: '1.5x bilinear', box: [96, 60], up: { mode: 'off' } },
    // The plan: [mode, box] -> upscaled?
    { name: 'fsr at 1x', box: [64, 40], up: { mode: 'fsr' }, plan: false },
    { name: 'fsr shown smaller', box: [48, 30], up: { mode: 'fsr' }, plan: false },
    { name: 'auto at 1.03x', box: [66, 41], up: { mode: 'auto' }, plan: false },
    { name: 'auto at 1.09x', box: [70, 44], up: { mode: 'auto' }, plan: true },
    { name: 'fsr at 1.03x', box: [66, 41], up: { mode: 'fsr' }, plan: true },
    // The streaming geometry the sandbox cannot run in real time: a 960x540
    // frame (the pattern tiled) into a 1920x1080 canvas, Auto, at sampled pixels.
    { name: '960x540 -> 1920x1080 auto', box: [1920, 1080], up: { mode: 'auto' }, big: [960, 540], sample: fsrSamples(1920, 1080, 600) },
  ];
  const disp = await startXvfb();
  const b = await chromium.launch({ headless: false, env: { ...process.env, DISPLAY: disp }, args: ['--enable-unsafe-webgpu', '--ignore-gpu-blocklist'] });
  const ctx2 = await b.newContext({ ignoreHTTPSErrors: true });
  let res;
  try {
    const p = await ctx2.newPage();
    await p.goto(`${base}/login`);
    // Evaluated through the DevTools protocol (the page's CSP does not apply);
    // the modules come from the gateway like in the app.
    res = await p.evaluate(async ({ W, H, PAD, px, cases }) => {
      const P = await import('/js/protocol.js');
      const R = await import('/js/renderers.js');
      const frameOf = (pad, big) => {
        const c = new OffscreenCanvas(W, H + pad);
        c.getContext('2d').putImageData(new ImageData(new Uint8ClampedArray(pad ? px.padded : px.plain), W, H + pad), 0, 0);
        if (!big) return new VideoFrame(c, { timestamp: 0 });
        const t = new OffscreenCanvas(big[0], big[1]); // the pattern tiled
        const g = t.getContext('2d');
        for (let y = 0; y < big[1]; y += H) for (let x = 0; x < big[0]; x += W) g.drawImage(c, x, y);
        return new VideoFrame(t, { timestamp: 0 });
      };
      const out = [];
      for (const k of cases) {
        const canvas = new OffscreenCanvas(1, 1);
        try {
          const r = await R.createRenderer('webgpu', canvas, { upscale: k.up });
          const ready = await r.fsrReady;
          if (k.stops !== undefined) r.up = { ...r.up, sharpness: k.stops }; // past the settings' range: RCAS ~ identity
          r.resize(k.box[0], k.box[1]);
          const frame = frameOf(k.pad ? PAD : 0, k.big);
          const cfg = k.pad ? { width: W, height: H, codedWidth: W, codedHeight: H + PAD, cropBottom: PAD } : null;
          r.draw(frame, null, P.visibleArea(cfg, frame.visibleRect.width, frame.visibleRect.height, frame.displayWidth, frame.displayHeight));
          await r.device.queue.onSubmittedWorkDone();
          const bmp = canvas.transferToImageBitmap();
          const g = new OffscreenCanvas(bmp.width, bmp.height).getContext('2d');
          g.drawImage(bmp, 0, 0);
          const data = k.plan === undefined ? g.getImageData(0, 0, bmp.width, bmp.height).data : null;
          out.push({ name: k.name, w: bmp.width, h: bmp.height, px: data && !k.sample ? Array.from(data) : null,
            sampled: data && k.sample ? k.sample.map(([x, y]) => Array.from(data.subarray((y * bmp.width + x) * 4, (y * bmp.width + x) * 4 + 3))) : null,
            ready, error: r.fsrError, upscaled: r.upscaled, info: r.upscaleInfo() });
          r.prev?.close();
          r.prev = null;
          r.destroy();
        } catch (e) {
          out.push({ name: k.name, error: e.message });
        }
      }
      return out;
    }, { W, H, PAD, px: { plain: Array.from(plain), padded: Array.from(fsrPattern(W, H, PAD)) }, cases });
  } finally {
    await ctx2.close();
    await b.close();
  }
  const by = (name) => res.find((x) => x.name === name);
  const src = new Float64Array(W * H * 3);
  for (let i = 0; i < W * H; i++) for (let c = 0; c < 3; c++) src[i * 3 + c] = plain[i * 4 + c] / 255;
  const one = by('1x plain');
  let srcDiff = Infinity;
  if (one?.px) {
    srcDiff = 0;
    for (let i = 0; i < W * H; i++) for (let c = 0; c < 3; c++) srcDiff = Math.max(srcDiff, Math.abs(one.px[i * 4 + c] - plain[i * 4 + c]));
  }
  // Against the reference: max and mean difference in 8-bit levels, and the worst pixel.
  const compare = (k) => {
    const r = by(k.name);
    if (!r?.px) return { name: k.name, error: r?.error || 'no result', max: Infinity };
    const ref = fsrReference(src, W, H, r.w, r.h, k.stops ?? k.up.sharpness ?? 0.2, !!k.up.denoise);
    let max = 0;
    let sum = 0;
    let worst = '';
    for (let i = 0; i < r.w * r.h; i++) {
      const v = ref.at(i % r.w, Math.floor(i / r.w));
      for (let c = 0; c < 3; c++) {
        const want = Math.round(Math.min(1, Math.max(0, v[c])) * 255);
        const d = Math.abs(r.px[i * 4 + c] - want);
        sum += d;
        if (d > max) { max = d; worst = `(${i % r.w},${Math.floor(i / r.w)}) ${'rgb'[c]} ${r.px[i * 4 + c]} vs ${want}`; }
      }
    }
    return { name: k.name, w: r.w, h: r.h, max, mean: sum / (r.w * r.h * 3), worst, upscaled: r.upscaled, ready: r.ready };
  };
  const txt = (x) => (x.error ? `${x.name}: ${x.error}` : `${x.name} ${x.w}x${x.h}: max ${x.max}, mean ${x.mean.toFixed(3)}${x.max ? ` (worst ${x.worst})` : ''}`);
  const easu = cases.filter((k) => k.stops !== undefined).map(compare);
  check('FSR 1 shader (unit): EASU alone matches the CPU reference of ffx_fsr1.h within 1 level (1.5x, external and copy input)',
    srcDiff === 0 && easu.every((x) => x.upscaled && x.max <= 1), `1x plain path vs source: max ${srcDiff}; ${easu.map(txt).join('; ')}`);
  const full = cases.filter((k) => k.up.mode === 'fsr' && k.stops === undefined && k.plan === undefined && !k.pad).map(compare);
  check('FSR 1 shader (unit): EASU + RCAS match the CPU reference at 2x and 1.5x (external and copy input, 0.2 and 1 stop, denoise) within 5 levels',
    srcDiff === 0 && full.every((x) => x.upscaled && x.max <= 5 && x.mean <= 0.25), full.map(txt).join('; '));
  const pad = compare(cases.find((k) => k.pad));
  const padRes = by(pad.name);
  const white = padRes?.px ? Array.from({ length: padRes.w }, (_, x) => (padRes.h - 1) * padRes.w + x).filter((i) => padRes.px[i * 4] > 248 && padRes.px[i * 4 + 1] > 248 && padRes.px[i * 4 + 2] > 248).length : -1;
  check('FSR 1 shader (unit): a frame with 8 white padding rows the video config crops upscales the visible 64x40 only (taps clamped to the crop)',
    pad.upscaled && pad.max <= 5 && white === 0, `${txt(pad)}; white pixels in the bottom row: ${white}`);
  const m = {};
  for (const n of ['2x', '2x bilinear', '1.5x', '1.5x bilinear']) m[n] = by(n)?.px ? fsrMetrics(by(n).px, by(n).w, by(n).h, plain, W, H) : null;
  const sharper = (a, bl) => !!m[a] && !!m[bl] && m[a].width < m[bl].width && m[a].step > m[bl].step;
  check('FSR 1 shader (unit): the diagonal edge comes out sharper than bilinear (narrower 10-90 % rise, steeper step) at 2x and 1.5x',
    sharper('2x', '2x bilinear') && sharper('1.5x', '1.5x bilinear'),
    ['2x', '1.5x'].map((s) => `${s}: rise ${m[s]?.width} vs ${m[`${s} bilinear`]?.width} input px, step ${m[s]?.step} vs ${m[`${s} bilinear`]?.step}`).join('; '));
  // The ramp: EASU's Lanczos-like kernel leans towards the texels and RCAS
  // sharpens that into small steps (the reference does the same), so it is
  // held to a ramp's shape: within half an input step (4 of 7 levels) of
  // bilinear, no reversal over 1 level.
  const ramp = (a, bl) => {
    let off = 0;
    let back = 0;
    m[a].ramp.forEach((row, i) => row.forEach((v, x) => { off = Math.max(off, Math.abs(v - m[bl].ramp[i][x])); if (x) back = Math.max(back, row[x - 1] - v); }));
    return { off, back, ok: off <= 4 && back <= 1 };
  };
  check('FSR 1 shader (unit): flat areas stay flat (within 1 level 3 input px from anything else: no ringing), the ramp stays a ramp (within 4 levels of bilinear, no reversal over 1 level)',
    ['2x', '1.5x'].every((s) => m[s] && m[`${s} bilinear`] && m[s].flat > 1000 && m[s].flatMax <= 1 && ramp(s, `${s} bilinear`).ok),
    ['2x', '1.5x'].map((s) => (m[s] && m[`${s} bilinear`] ? `${s}: ${m[s].flat} flat px, off by at most ${m[s].flatMax}; ramp at most ${ramp(s, `${s} bilinear`).off} ` +
      `from bilinear, reversals at most ${ramp(s, `${s} bilinear`).back}` : `${s}: no result`)).join('; '));
  // The streaming geometry at sampled pixels.
  const bk = cases.find((k) => k.big);
  const big = by(bk.name);
  let bigMax = Infinity;
  if (big?.sampled) {
    const [bw, bh] = bk.big;
    const bsrc = new Float64Array(bw * bh * 3);
    for (let y = 0; y < bh; y++) for (let x = 0; x < bw; x++) for (let c = 0; c < 3; c++) bsrc[(y * bw + x) * 3 + c] = plain[((y % H) * W + (x % W)) * 4 + c] / 255;
    const ref = fsrReference(bsrc, bw, bh, big.w, big.h, 0.2);
    bigMax = Math.max(...bk.sample.map(([x, y], i) => Math.max(...ref.at(x, y).map((v, c) => Math.abs(big.sampled[i][c] - Math.round(Math.min(1, Math.max(0, v)) * 255))))));
  }
  check('FSR 1 shader (unit): Auto upscales a 960x540 frame into a 1920x1080 canvas with FSR 1, matching the CPU reference within 5 levels at 600 sampled pixels',
    !!big && !big.error && big.upscaled && big.info.in?.join('x') === '960x540' && big.info.out?.join('x') === '1920x1080' && bigMax <= 5,
    big?.error || `${big.upscaled ? 'FSR' : `bilinear (${big.info.why})`} ${big.info.in?.join('x')} -> ${big.info.out?.join('x')}, ${big.info.input} input; max difference ${bigMax} levels`);
  const plans = cases.filter((k) => k.plan !== undefined).map((k) => ({ k, r: by(k.name) }));
  check('FSR 1 (unit): never upscales a picture shown at its size or smaller; Auto only above 1.05x, FSR above 1x; the renderer reports why',
    plans.every(({ k, r }) => r && !r.error && r.upscaled === k.plan && r.info.active === k.plan && (k.plan || !!r.info.why)),
    plans.map(({ k, r }) => `${k.name}: ${r?.error || `${r.upscaled ? 'FSR' : `bilinear (${r.info.why})`}, ${r.info.in?.join('x')} -> ${r.info.out?.join('x')}`}`).join('; '));
  results.push({ upscaleUnit: { srcDiff, easu, full, pad, bigMax, metrics: Object.fromEntries(Object.entries(m).map(([k, v]) => [k, v && { width: v.width, step: v.step, flat: v.flat, flatMax: v.flatMax }])) } });
}

// Auto's pick at unit level (step 4.3, renderers.js pickPath) on made-up
// bake-off numbers, among them the review run's (WebGL2 drew 0.2 ms faster
// and its display estimate was 2.6 ms lower by a few outliers: noise, the 2D
// canvas is desynchronized and stays).
async function checkPickRule() {
  const cases = await page.evaluate(async () => {
    const R = await import('/js/renderers.js');
    const mk = (o) => ({ desynchronized: true, draw: { p50: 0.6, n: 70 }, display: { p50: 11.5, n: 60 }, drawRounds: [0.6, 0.6], fps: 30, errors: 0, lost: false, ...o });
    const list = [
      ["the review run: WebGL2 0.2 ms faster but not desynchronized, WebGPU's display p50 more than a refresh behind", 'canvas2d', { webgpu: 'display lags' }, {
        canvas2d: mk({ draw: { p50: 0.66, n: 65 }, drawRounds: [0.64, 0.68], display: { p50: 11.91, n: 65 } }),
        webgl2: mk({ desynchronized: false, draw: { p50: 0.47, n: 73 }, drawRounds: [0.45, 0.49], display: { p50: 11.52, n: 73 } }),
        webgpu: mk({ desynchronized: null, display: { p50: 37, n: 12 } }) }],
      ['both desynchronized, WebGL2 0.19 ms faster: a near tie keeps the 2D default', 'canvas2d', {}, {
        canvas2d: mk({ drawRounds: [0.7, 0.7] }), webgl2: mk({ draw: { p50: 0.51, n: 70 }, drawRounds: [0.51, 0.51] }), webgpu: { error: 'WebGPU unavailable' } }],
      ['both desynchronized, WebGL2 3 ms faster in both rounds', 'webgl2', {}, {
        canvas2d: mk({ draw: { p50: 4.1, n: 70 }, drawRounds: [4.0, 4.2] }), webgl2: mk({ draw: { p50: 0.9, n: 70 }, drawRounds: [0.8, 1.0] }) }],
      ['WebGL2 3 ms faster in one round only', 'canvas2d', {}, {
        canvas2d: mk({ draw: { p50: 2.5, n: 70 }, drawRounds: [4.0, 1.0] }), webgl2: mk({ draw: { p50: 1.1, n: 70 }, drawRounds: [1.0, 1.2] }) }],
      ['the fastest path failed draws', 'canvas2d', { webgl2: 'draw errors' }, {
        canvas2d: mk({ draw: { p50: 4.1, n: 70 }, drawRounds: [4.0, 4.2] }), webgl2: mk({ draw: { p50: 0.1, n: 70 }, drawRounds: [0.1, 0.1], errors: 3 }) }],
      ['the fastest path lost its context', 'canvas2d', { webgl2: 'context lost' }, {
        canvas2d: mk({ draw: { p50: 4.1, n: 70 }, drawRounds: [4.0, 4.2] }), webgl2: mk({ draw: { p50: 0.1, n: 70 }, drawRounds: [0.1, 0.1], lost: true }) }],
      ['the 2D canvas drops frames: WebGL2 although not desynchronized', 'webgl2', { canvas2d: 'drops frames' }, {
        canvas2d: mk({ fps: 12 }), webgl2: mk({ desynchronized: false }) }],
      ['too few samples everywhere', null, { canvas2d: 'too few samples', webgl2: 'too few samples' }, {
        canvas2d: mk({ draw: { p50: 0.6, n: 10 } }), webgl2: mk({ display: { n: 0 } }) }],
    ];
    return list.map(([name, want, out, res]) => ({ name, want, out, got: R.pickPath(res, 1000 / 60) }));
  });
  for (const c of cases) {
    const outOK = Object.entries(c.out).every(([p, why]) => c.got.out[p] === why) && Object.keys(c.got.out).length === Object.keys(c.out).length;
    check(`Auto's pick (unit): ${c.name}`, c.got.winner === c.want && outOK, `${c.got.winner} (${c.got.why}); out ${JSON.stringify(c.got.out)}`);
  }
}

// Frame pacing (step 4.4), live on a scenario's stream. The drawer's "Frame
// pacing" select switches to Smooth without a reconnect (the same worker and
// connection): from then on every frame is drawn from the worker's
// requestAnimationFrame at the first refresh after its decoder output (the
// test runs its own refresh loop in the worker meanwhile), or, where the
// browser ran no refresh for 100 ms (the emulated GPU here while WebGPU
// presents), by the watchdog; at most one per display refresh (the refresh
// starts of consecutive draws at least 0.9 vsync apart); its hold stage
// (decoder output -> draw start) is the wait for the refresh (p50 at most
// one of the refreshes the browser ran, + 2 ms); every frame whose refresh
// started more than 1.25 refresh after its output is counted late; hold and
// draw are the differences of the frame's own marks and the stages still add
// up to end-to-end; the barcode read back is still the frame's seq; none is
// left open; and the overlay names the mode. Back to Lowest latency: drawn on
// decode again (one-task hop, hold p50 under 2 ms). With fallbacks (the 2D
// scenario): without requestAnimationFrame in the worker the page's animation
// frames tick it; with one that never calls back, the watchdog draws from a
// timer (and logs it); with it restored, its ticks are used again.
const streamWorker = () => page.workers().filter((x) => x.url().endsWith('/js/stream-worker.js')).pop();

async function setPacing(mode) {
  await page.evaluate((m) => {
    const sel = [...document.querySelectorAll('#drawer select')].find((x) => [...x.options].some((o) => o.value === 'smooth'));
    sel.value = m;
    sel.dispatchEvent(new Event('change'));
  }, mode);
  await sleep(1000); // applied, and a stats update from after it
}

// Draws in a window of `ms`: the stage records drawn in it (worker clock),
// the pacing counters' change, the probe's change, the stats after it, and
// the worker's own display refresh meanwhile (a requestAnimationFrame loop
// of the test's: refreshes per second, and the vsync interval = the shortest
// interval between two; a loaded machine skips refreshes).
async function pacingWindow(ms) {
  const w = streamWorker();
  const s0 = await page.evaluate(() => ({ pacing: window.__recon.lastStats?.pacing, probe: window.__recon.probe }));
  const t0 = await w.evaluate(() => {
    const raf = self.__raf || self.requestAnimationFrame;
    const ts = [];
    self.__refresh = { on: true, ts };
    const loop = (t) => { ts.push(t); if (self.__refresh.ts === ts && self.__refresh.on) raf.call(self, loop); };
    raf.call(self, loop);
    return performance.now();
  });
  await sleep(ms);
  const [t1, rts] = await w.evaluate(() => { self.__refresh.on = false; return [performance.now(), self.__refresh.ts]; });
  const rgaps = rts.slice(1).map((v, i) => v - rts[i]);
  const tickHz = (1000 * rts.length) / (t1 - t0);
  const vsync = rgaps.length ? Math.min(...rgaps) : null;
  await page.evaluate(() => { window.__recon.stageDump = null; window.__recon.worker.postMessage({ type: 'stageDump' }); });
  const dump = await until(() => page.evaluate(() => window.__recon.stageDump), 3000, 'stage dump').catch(() => []);
  await sleep(600); // a stats update from after the window
  const s1 = await page.evaluate(() => ({ st: window.__recon.lastStats, probe: window.__recon.probe }));
  const recs = dump.filter((r) => r.drawn >= t0 && r.drawn <= t1);
  const d = {};
  for (const k of ['hop', 'raf', 'main', 'timer', 'stale', 'late']) d[k] = (s1.st?.pacing?.counts?.[k] ?? 0) - (s0.pacing?.counts?.[k] ?? 0);
  // Refresh starts of consecutive draws from refresh ticks (a watchdog draw's "tick" is its timer).
  const ticks = recs.filter((r) => r.via === 'raf' || r.via === 'main').map((r) => r.tick).sort((a, b) => a - b);
  const gaps = ticks.slice(1).map((v, i) => v - ticks[i]);
  const H = STAGES.indexOf('hold');
  const D = STAGES.indexOf('draw');
  const q = (v) => { const a = [...v].sort((x, y) => x - y); return (p) => (a.length ? +a[Math.min(a.length - 1, Math.floor(p * a.length))].toFixed(2) : null); };
  const hold = q(recs.map((r) => r.stages[H]));
  const refresh = s1.st?.pacing?.refreshMs;
  const waited = recs.filter((r) => r.tick !== null);
  const wait = waited.map((r) => r.tick - r.output);
  // Over 1.25 of the refresh interval the pacer judged the frame by: must all be counted late.
  const lateRecs = waited.filter((r) => r.tick - r.output > 1.25 * r.refresh + 0.05).length;
  // Drawn at the first refresh after the decoder's output: no refresh of the
  // worker's started between a frame's output and the refresh (or watchdog
  // timer) that drew it (1 ms of slack each side).
  const missed = recs.filter((r) => r.tick !== null && rts.some((f) => f > r.output + 1 && f < r.tick - 1)).length;
  // Bookkeeping per frame: hold and draw from the frame's own marks, the stages up to draw add up to end-to-end.
  const marks = recs.every((r) => Math.abs(r.stages[H] - (r.drawStart - r.output)) < 0.01 && Math.abs(r.stages[D] - (r.drawn - r.drawStart)) < 0.01);
  let sumDiff = 0;
  for (const r of recs) {
    let sum = 0;
    for (let i = r.fromCapture ? 0 : 2; i < DISPLAY; i++) sum += r.stages[i];
    sumDiff += Math.abs(sum - (r.drawn - ((r.fromCapture ? r.captureUs : r.sendUs) / 1000 - r.offset)));
  }
  const probe = { sampled: (s1.probe?.sampled ?? 0) - (s0.probe?.sampled ?? 0), matched: (s1.probe?.valid ?? 0) - (s1.probe?.mismatched ?? 0) - (s0.probe?.valid ?? 0) + (s0.probe?.mismatched ?? 0) };
  return {
    recs, d, st: s1.st, refresh, tickHz, vsync, fps: (1000 * recs.length) / (t1 - t0), minGap: gaps.length ? Math.min(...gaps) : null,
    hold: { p50: hold(0.5), p95: hold(0.95), p99: hold(0.99) }, waitMax: wait.length ? +Math.max(...wait).toFixed(2) : null, lateRecs, missed, marks,
    sumDiff: recs.length ? sumDiff / recs.length : Infinity, probe, via: [...new Set(recs.map((r) => r.via))], modes: [...new Set(recs.map((r) => r.pacing))],
  };
}

async function checkPacing(name, rate, fallbacks) {
  await page.evaluate(() => { window.__recon.worker.__pacingTag = 1; window.__recon.conn.__pacingTag = 1; });
  const sameSession = () => page.evaluate(() => window.__recon.worker?.__pacingTag === 1 && window.__recon.conn?.__pacingTag === 1 && window.__recon.streaming);
  const row = (x) => `${x.recs.length} frames drawn (${x.fps.toFixed(1)} fps of ${rate}; the worker's display refresh meanwhile ${x.tickHz.toFixed(1)} Hz, vsync ` +
    `${x.vsync?.toFixed(2)} ms) via ${x.via.join('/') || '—'}; counters +hop ${x.d.hop} +raf ${x.d.raf} +main ${x.d.main} ` +
    `+timer ${x.d.timer}, stale +${x.d.stale}, late +${x.d.late}; refresh starts at least ${x.minGap?.toFixed(2)} ms apart (pacer's refresh ${x.refresh} ms), ` +
    `${x.missed} drawn after a later refresh than the first after their output; ` +
    `hold p50/p95/p99 ${x.hold.p50}/${x.hold.p95}/${x.hold.p99} ms, refresh start - output at most ${x.waitMax} ms (${x.lateRecs} over 1.25 refresh); ` +
    `marks ${x.marks}, stages vs end-to-end ${x.sumDiff.toFixed(3)} ms; ` +
    `barcode ${x.probe.matched}/${x.probe.sampled} = seq`;
  // Smooth from refresh ticks of `via`, or from the watchdog's timer where the
  // browser ran no refresh (the emulated GPU here stalls the page's refreshes
  // for 100 ms and more while WebGPU presents), one draw per vsync, and with
  // the worker's own ticks each frame drawn at the first refresh after its
  // output (at most 2 % otherwise, a slow tick on a loaded machine); with the
  // page's ticks (refreshes the worker does not see) frames drawn at 60 % of
  // the stream's or the refresh rate, whichever is lower, at least. Every
  // frame drawn over 1.25 refresh after its output counted late, the marks
  // and stage sums right.
  const smooth = (x, via) => x.recs.length >= 15 && x.via.every((v) => v === via || v === 'timer') && x.modes.length === 1 && x.modes[0] === 'smooth' &&
    x.d.hop === 0 && x.d[via] >= x.recs.filter((r) => r.via === via).length && (x.minGap === null || x.minGap >= 0.9 * x.vsync) &&
    (via === 'raf' ? x.missed <= Math.max(1, 0.02 * x.recs.length) : x.fps >= 0.6 * Math.min(rate, x.tickHz)) && x.lateRecs <= x.d.late && x.marks && x.sumDiff <= 2;

  await setPacing('smooth');
  const sm = await pacingWindow(4000);
  const overlay = await page.textContent('#stats').catch(() => '');
  const vf = sm.st?.videoFrames;
  check(`${name}: frame pacing Smooth, applied live: drawn on the worker's display refresh, at most one frame per refresh, hold = the wait for it`,
    (await sameSession()) && sm.st?.pacing?.mode === 'smooth' && sm.st.pacing.ticks === 'raf' && smooth(sm, 'raf') &&
      sm.hold.p50 <= 1000 / sm.tickHz + 2 && overlay.includes('Frame pacing') && overlay.includes('Smooth · each refresh (worker rAF'),
    `${row(sm)}; overlay: ${(overlay.match(/Frame pacing[^\n]*?(?=Audio|$)/) || [''])[0].slice(0, 120)}`);
  check(`${name}: frame pacing Smooth: the drawn picture is the frame described (barcode = seq), no VideoFrame left open`,
    sm.probe.sampled >= 1 && sm.probe.matched >= 0.9 * sm.probe.sampled && !!vf && vf.leaked === 0 && vf.max <= 5,
    `barcode ${sm.probe.matched} of ${sm.probe.sampled} sampled = seq; VideoFrames open ${vf?.open}, max ${vf?.max}, leaked ${vf?.leaked}`);
  results.push({ pacing: name, mode: 'smooth', ...sm, recs: sm.recs.length });

  if (fallbacks) {
    // No requestAnimationFrame in the worker: the page's animation frames.
    await setPacing('latency');
    const w = streamWorker();
    await w.evaluate(() => { self.__raf = self.requestAnimationFrame; self.requestAnimationFrame = undefined; });
    await setPacing('smooth');
    const mt = await pacingWindow(2500);
    check(`${name}: frame pacing Smooth without requestAnimationFrame in the worker: ticked by the page's animation frames`,
      (await sameSession()) && mt.st?.pacing?.ticks === 'main' && smooth(mt, 'main'), row(mt));
    // A requestAnimationFrame that never calls back: the watchdog.
    await setPacing('latency');
    await w.evaluate(() => { self.requestAnimationFrame = () => 0; });
    const log0 = (await page.evaluate(() => window.__recon.logs.length));
    await setPacing('smooth');
    const wd = await pacingWindow(3000);
    const wdLog = (await page.evaluate((n) => window.__recon.logs.slice(n), log0)).filter((l) => l.includes('frame pacing: no display refresh')).map((l) => l.replace(/^\S+ /, ''));
    check(`${name}: frame pacing Smooth with a refresh callback that never runs: the watchdog draws from a timer and says so`,
      (await sameSession()) && wd.recs.length >= 12 && wd.via.length === 1 && wd.via[0] === 'timer' && wd.d.timer >= wd.recs.length && wd.d.hop === 0 && wdLog.length === 1,
      `${row(wd)}; log: ${wdLog.join(' | ') || 'none'}`);
    // Restored: its ticks again.
    await w.evaluate(() => { self.requestAnimationFrame = self.__raf; });
    await sleep(500);
    const back = await pacingWindow(2000);
    check(`${name}: frame pacing Smooth: requestAnimationFrame restored, its ticks are used again`,
      (await sameSession()) && smooth(back, 'raf') && back.d.timer === 0, row(back));
    results.push({ pacing: name, fallbacks: { main: { ...mt, recs: mt.recs.length }, watchdog: { ...wd, recs: wd.recs.length, log: wdLog }, restored: { ...back, recs: back.recs.length } } });
  }

  await setPacing('latency');
  const lt = await pacingWindow(2500);
  check(`${name}: frame pacing back to Lowest latency, applied live: drawn on decode again (one task, hold p50 < 2 ms)`,
    (await sameSession()) && lt.st?.pacing?.mode === 'latency' && lt.recs.length >= rate && lt.via.length === 1 && lt.via[0] === 'hop' && lt.modes[0] === 'latency' &&
      lt.d.raf === 0 && lt.d.main === 0 && lt.d.timer === 0 && lt.d.hop >= lt.recs.length && lt.hold.p50 < 2 && lt.marks && lt.sumDiff <= 2 && lt.fps >= 0.75 * rate, row(lt));
  results.push({ pacing: name, mode: 'latency', ...lt, recs: lt.recs.length });
}

// A host from before step 4.4 (the host's test hook pre-stage-hold: its
// welcome lacks stage-hold, and it logs a stage report only with at most nine
// rows, none of them hold, as those hosts did). The client, in Smooth (a hold
// of up to a refresh before each draw), reports hold and draw to it as one
// draw row: the host logs the report (a separate hold row would make ten rows,
// and it would log nothing), its draw row covers every frame of the window
// (n = e2e's n), and its p50 is the client's hold + draw over the same frames
// (the stage dump right after the report: within max(1.5 ms, 15 %)), not
// draw alone.
async function checkPreStageHoldHost() {
  const host = await restartHost({ RECON_TEST_FAULTS: 'pre-stage-hold' }, 'host-pre-hold');
  await startStream({ path: 'auto', transport: 'auto', pacing: 'smooth' });
  // The first report: 10 s after the worker started.
  const line = await until(() => (host.log.match(/msg="latency stages[^\n]*/) || [])[0], 20000, 'stage line').catch(() => '');
  await page.evaluate(() => { window.__recon.stageDump = null; window.__recon.worker.postMessage({ type: 'stageDump' }); });
  const dump = await until(() => page.evaluate(() => window.__recon.stageDump), 3000, 'stage dump').catch(() => []);
  const features = await page.evaluate(() => window.__recon.welcome?.features || []);
  const row = (k) => { const m = line.match(new RegExp(` ${k}="([\\d.]+)/[\\d.]+/[\\d.]+ n=(\\d+)"`)); return m ? { p50: +m[1], n: +m[2] } : null; };
  const H = STAGES.indexOf('hold');
  const D = STAGES.indexOf('draw');
  const p50 = (v) => { const a = [...v].sort((x, y) => x - y); return a.length ? a[Math.min(a.length - 1, Math.floor(0.5 * a.length))] : NaN; };
  const sum = p50(dump.map((r) => r.stages[H] + r.stages[D]));
  const drawOnly = p50(dump.map((r) => r.stages[D]));
  const draw = row('draw');
  const e2e = row('e2e');
  check('host before step 4.4 (no stage-hold): the client in Smooth reports hold + draw as one draw row, which the host logs',
    !features.includes('stage-hold') && !!draw && !!e2e && !/ hold=/.test(line) && / pacing=smooth /.test(line) && draw.n === e2e.n &&
      dump.length >= 0.8 * draw.n && Math.abs(draw.p50 - sum) <= Math.max(1.5, 0.15 * sum) && sum - drawOnly >= 1,
    `welcome features ${features.join(',')}; host: ${line ? `${(line.match(/ renderer=\S+ pacing=\S+/) || ['no renderer/pacing'])[0].trim()}, ` +
      `draw p50 ${draw?.p50} n ${draw?.n}, e2e n ${e2e?.n}, ${/ hold=/.test(line) ? 'a hold row' : 'no hold row'}` : 'no stage line'}; ` +
      `client's frames (stage dump, ${dump.length}): hold + draw p50 ${sum.toFixed(2)} ms, draw alone ${drawOnly.toFixed(2)} ms`);
  await page.evaluate(() => { window.__recon.userClosed = true; });
}

// The frame pacer at unit level (step 4.4, pacing.js) on a fake clock: what
// it draws and drops, and from which tick, in both modes, at the stale rule's
// edges (older than one refresh with or without a newer frame in the
// decoder, never two drops in a row, the quarter refresh of slack), with the
// main thread's ticks, the watchdog (never a stale drop) and live mode
// switches; the refresh interval taken from the ticks, not a page-load
// measurement that differs from them.
async function checkPacerRule() {
  const cases = await page.evaluate(async () => {
    const T = await import('/js/pacing.js');
    // A pacer on a fake clock (page-load refresh measurement 16 ms unless
    // given): frames offered, one-task hops, requestAnimationFrame callbacks
    // and watchdog timers run when told; what it draws and drops is logged.
    const mk = ({ raf = true, refresh = 16 } = {}) => {
      const ev = [];
      let t = 0;
      let newer = false;
      const hops = [];
      const rafs = [];
      const timers = [];
      const p = new T.Pacer({
        draw: (it) => ev.push(`draw ${it.id} ${it.via}${it.tick === null ? '' : ` @${it.tick}`}`),
        drop: (it, why) => ev.push(`drop ${it.id} ${why}`),
        newerComing: () => newer, refreshMs: () => refresh, raf: () => (raf ? (cb) => rafs.push(cb) : null),
        mainTicks: (on) => ev.push(`main ticks ${on ? 'on' : 'off'}`), now: () => t, log: (x) => ev.push(`log: ${x}`),
        soon: (fn) => hops.push(fn), timer: (fn, ms) => { const x = { fn, at: t + ms, live: true }; timers.push(x); return () => { x.live = false; }; },
      });
      return {
        p, ev,
        at(ms) { t = ms; for (const x of timers) if (x.live && x.at <= t) { x.live = false; x.fn(); } },
        frame(id) { p.offer({ id, decoded: t }); },
        hop() { hops.splice(0).forEach((f) => f()); },
        vsync(ts = t) { rafs.splice(0).forEach((cb) => cb(ts)); },
        newer(v) { newer = v; },
        rafs: () => rafs.length,
      };
    };
    const out = [];
    const run = (name, want, fn, opts) => {
      const x = mk(opts);
      const extra = fn(x) || {};
      out.push({ name, want, got: x.ev, counts: x.p.counts, ...extra });
    };
    run('Lowest latency: drawn one task after the output; a burst keeps the newest', ['drop 1 superseded', 'draw 2 hop'], (x) => {
      x.frame(1); x.frame(2); x.hop();
    });
    run('Smooth: a frame waits for the next refresh, at most one waits, one draw per refresh', ['drop 1 superseded', 'draw 2 raf @16', 'draw 3 raf @48'], (x) => {
      x.p.setMode('smooth');
      x.frame(1); x.at(5); x.frame(2);
      const requests = x.rafs();
      x.at(16); x.vsync();
      x.at(32); x.vsync(); // nothing waits: nothing drawn (the one tick asked for after a draw)
      x.at(40); x.frame(3); x.at(48); x.vsync();
      return { requests };
    });
    run('Smooth: older than one refresh at its refresh, a newer frame in the decoder: dropped (stale), the newer takes the next refresh',
      ['drop 1 stale', 'draw 2 raf @32'], (x) => {
        x.p.setMode('smooth'); x.newer(true);
        x.frame(1); x.at(21); x.vsync(); x.at(22); x.frame(2); x.at(32); x.vsync();
      });
    run('Smooth: older than one refresh, nothing newer on the way: drawn late (the last picture is never lost)', ['draw 1 raf @21'], (x) => {
      x.p.setMode('smooth');
      x.frame(1); x.at(21); x.vsync();
    });
    run('Smooth: never two stale drops in a row', ['drop 1 stale', 'draw 2 raf @45'], (x) => {
      x.p.setMode('smooth'); x.newer(true);
      x.frame(1); x.at(21); x.vsync(); x.at(22); x.frame(2); x.at(45); x.vsync();
    });
    run('Smooth: within a quarter refresh of slack (decoded as a refresh started) it is not stale', ['draw 1 raf @19'], (x) => {
      x.p.setMode('smooth'); x.newer(true);
      x.frame(1); x.at(19); x.vsync();
    });
    run('Smooth without requestAnimationFrame in the worker: the main thread\'s ticks; off again in Lowest latency',
      ['main ticks on', 'draw 1 main @10', 'main ticks off'], (x) => {
        x.p.setMode('smooth');
        x.frame(1); x.at(12); x.p.tick(10, 'main'); x.p.tick(26, 'main');
        x.p.setMode('latency');
      }, { raf: false });
    const WD_LOG = 'log: frame pacing: no display refresh within 100 ms of a decoded frame; drawing from a timer';
    run('Smooth: no refresh tick within max(3 refreshes, 100 ms): the watchdog draws from a timer (logged once per run), later ticks are used again',
      [WD_LOG, 'draw 1 timer @100', 'draw 2 raf @112', WD_LOG, 'draw 3 timer @220', 'draw 4 timer @330', 'draw 5 raf @344', WD_LOG, 'draw 6 timer @445'], (x) => {
        x.p.setMode('smooth');
        x.frame(1); x.at(99); x.at(100); x.at(105); x.frame(2); x.at(112); x.vsync();
        x.at(120); x.frame(3); x.at(220); x.at(230); x.frame(4); x.at(330);
        x.at(340); x.frame(5); x.at(344); x.vsync(); x.at(345); x.frame(6); x.at(445);
      });
    run('Smooth: the watchdog\'s timer never drops a frame as stale, a newer one in the decoder or not (no refresh comes sooner for it): drawn late',
      [WD_LOG, 'draw 1 timer @100', 'stale 0, late 1'], (x) => {
        x.p.setMode('smooth'); x.newer(true);
        x.frame(1); x.at(100);
        x.ev.push(`stale ${x.p.counts.stale}, late ${x.p.counts.late}`);
      });
    run('Smooth: "one refresh" is the interval its ticks show (16 ms) once 8 are seen, not the page-load measurement (7 ms): 12 ms old with a newer frame coming is stale before, drawn after',
      ['main ticks on', 'drop 1 stale', 'draw 2 main @144', 'refresh 7 -> 16, late 0'], (x) => {
        x.p.setMode('smooth'); x.newer(true);
        const before = x.p.info().refreshMs;
        x.at(4); x.frame(1); x.p.tick(16, 'main'); // no interval seen yet: 7 ms
        for (let ts = 32; ts <= 128; ts += 16) x.p.tick(ts, 'main');
        x.at(132); x.frame(2); x.p.tick(144, 'main'); // the 8th interval
        x.ev.push(`refresh ${before} -> ${x.p.info().refreshMs}, late ${x.p.counts.late}`);
      }, { raf: false, refresh: 7 });
    run('Smooth: now and then (at most every 250 ms) the refresh after a frame\'s is ticked too, so a stream at half the refresh rate shows the refresh interval (16 ms, not its 32 ms frame interval)',
      ['20 draws, refresh 16, 3 extra refresh requests in 640 ms'], (x) => {
        x.p.setMode('smooth');
        for (let k = 0; k < 20; k++) { x.at(32 * k + 4); x.frame(k); x.at(32 * k + 16); x.vsync(); x.at(32 * k + 32); x.vsync(); }
        const draws = x.ev.filter((e) => e.startsWith('draw')).length;
        x.ev.length = 0;
        x.ev.push(`${draws} draws, refresh ${x.p.info().refreshMs}, ${x.p.rafSeq - 20} extra refresh requests in 640 ms`);
      });
    run('mode switch with a frame waiting: Smooth -> Lowest latency draws it one task later; Lowest latency -> Smooth waits for the refresh',
      ['draw 1 hop', 'draw 2 raf @32'], (x) => {
        x.p.setMode('smooth');
        x.frame(1); x.p.setMode('latency'); x.hop(); x.at(16); x.vsync();
        x.at(20); x.frame(2); x.p.setMode('smooth'); x.hop(); x.at(32); x.vsync();
      });
    out.push({
      name: 'paceFate at 16 ms refreshes: 20 ms old draws; 21 ms old is stale with a newer frame coming, late without one or after a drop',
      want: ['draw', 'stale', 'late', 'late'],
      got: [T.paceFate(20, 0, 16, true), T.paceFate(21, 0, 16, true), T.paceFate(21, 0, 16, false), T.paceFate(21, 0, 16, true, true)],
    });
    return out;
  });
  for (const c of cases) {
    check(`frame pacing (unit): ${c.name}`, JSON.stringify(c.got) === JSON.stringify(c.want) && (c.requests === undefined || c.requests === 1),
      `${c.got.join('; ')}${c.requests === undefined ? '' : `; ${c.requests} refresh request for two frames`}`);
  }
}

async function checkWallclockProbe() {
  // An X display with the test page full-screen (kiosk, no automation info bar).
  const disp = await startXvfb();
  testPage = await chromium.launchPersistentContext(join(dir, 'testpage-profile'), {
    headless: false, viewport: null, ignoreDefaultArgs: ['--enable-automation'],
    env: { ...process.env, DISPLAY: disp }, args: ['--kiosk', '--window-position=0,0', '--window-size=1280,720'],
  });
  const tp = testPage.pages()[0] || await testPage.newPage();
  await tp.goto(`file://${join(root, 'tools', 'latency-test', 'index.html')}`);
  const lt = await until(() => tp.evaluate(() => (window.__latencyTest.fps > 0 ? window.__latencyTest : null)), 10000, 'latency test page');
  check('latency test page renders full-screen on the host display', lt.fullscreen, `${disp}: ${lt.fps} fps, cell ${lt.cellPx.toFixed(1)} px`);

  // Restart the host agent on x11grab of that display (same pairing).
  const cfgPath = join(dir, 'host.json');
  const hostCfg = JSON.parse(readFileSync(cfgPath, 'utf8'));
  const old = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  old.kill('SIGTERM');
  await new Promise((r) => (old.exitCode !== null ? r() : old.on('exit', r)));
  writeFileSync(cfgPath, JSON.stringify({ ...hostCfg, capture: 'x11grab', x11Display: disp }));
  run('recon-host', ['-config', cfgPath, 'run'], { RECON_INPUT_LOG: inputLog }, 'host-x11');
  await page.goto(`${base}/`);
  await until(async () => (await page.$$('.host.online')).length === 1, 60000, 'host online after restart');

  // 30 fps: software AV1 decode of the 1280x720 screen has to keep up for 10 minutes on CI machines.
  await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify(p)), { stats: true, latencyProbe: true, bitrate: 8, fps: 30, ...PREFS_2D });
  await page.click('.host.online a.btn-primary');
  await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
  await page.click('#btn-start');
  await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
  await sleep(6000); // decoder warm-up
  // E2E_WALLCLOCK_SECONDS=600: a 10-minute run (the wall-clock barcode wraps every 65.5 s).
  const longRun = +(process.env.E2E_WALLCLOCK_SECONDS || 0);
  if (longRun) await sleep(longRun * 1000);
  await until(() => page.evaluate(() => window.__recon.probe?.sampled >= 16), 30000, '16 probe samples').catch(() => {});
  const exp = await page.evaluate(() => window.__recon.exportLatency());
  if (exp) writeFileSync(join(outDir, 'latency-wallclock.json'), JSON.stringify(exp, null, 2));
  const pr = await page.evaluate(() => window.__recon.probe);
  const st = await page.evaluate(() => window.__recon.lastStats);
  const cfg = await page.evaluate(() => window.__recon.videoCfg);
  const l = pr?.latency;
  const p2c = pr?.pageToCapture;
  const share = pr?.sampled ? pr.valid / pr.sampled : 0;
  const e2e = st?.stages?.e2e;
  check('latency probe (wallclock): test page barcode read back, host screen→drawn histogram',
    pr?.mode === 'wallclock' && pr.sampled >= 10 && share >= 0.9 && pr.implausible === 0 && l?.n > 0,
    pr ? `${cfg?.encoder} ${cfg?.capture} ${cfg?.width}x${cfg?.height}, ${pr.durationS} s: ${pr.sampled} sampled (${pr.method}), valid ${(100 * share).toFixed(0)} %, ` +
      `${pr.implausible} implausible; screen→drawn p50/p95/p99 ${l ? `${l.p50}/${l.p95}/${l.p99} ms` : '—'}` +
      `${pr.lastInvalid ? `; last invalid: cells ${pr.lastInvalid.luma}` : ''}` : 'no probe state');
  check('latency probe (wallclock): page→capture from the barcode clock is small and not negative (wall-clock offset + clock sync)',
    !!p2c && p2c.min >= -3 && p2c.p50 <= 100,
    p2c ? `page→capture p50/p95 ${p2c.p50}/${p2c.p95} ms, min ${p2c.min} ms (n ${p2c.n}); capture→draw (stamps) p50 ${e2e?.p50} ms vs screen→drawn ${l?.p50} ms` : 'no page→capture samples');
  results.push({ probe: 'wallclock', summary: pr, stages: st?.stages, cfg });
  await page.evaluate(() => { window.__recon.userClosed = true; });
}

// ---------------------------------------------------------------------------
// Loss handling (guide step 1.4). Frames travel on reliable streams, so a gap
// in the sequence is a late frame unless the host reports the frame dropped
// ({"t":"dropped"}); only a confirmed loss costs a key frame (recovery
// "keyframe": an encoder restart on the FFmpeg path) or nothing at all
// (recovery "skip": the encoder heals the picture with intra refresh). The
// host's test-only hook RECON_TEST_FAULTS (internal/host/faults.go) delays
// every 97th frame by 200 ms and drops every 193rd.

const LOSS_FAULTS = 'delay=every:97:200ms,drop=every:193';

// Encoder restarts by reason in a host log ("restarting video" lines).
function restartsByReason(log) {
  const out = {};
  for (const m of log.matchAll(/msg="restarting video".*? reason=("[^"]*"|\S+) urgent=(\w+)/g)) {
    const k = `${m[1].replaceAll('"', '')}${m[2] === 'true' ? ' (urgent)' : ''}`;
    out[k] = (out[k] || 0) + 1;
  }
  return out;
}

// The client's key-frame requests by reason, from its console log.
function keyRequestsByReason(lines) {
  const out = {};
  for (const l of lines) {
    const m = l.match(/requesting key frame \(([^)]*)\)/) || (l.includes('still waiting for a key frame') ? [l, 'watchdog'] : null);
    if (m) out[m[1]] = (out[m[1]] || 0) + 1;
  }
  return out;
}

const counts = (o) => Object.entries(o).map(([k, v]) => `${k}: ${v}`).join(', ') || 'none';

// Restart the host agent (same pairing and config) with extra environment.
async function restartHost(env, name) {
  const old = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  old.kill('SIGTERM');
  await new Promise((r) => (old.exitCode !== null ? r() : old.on('exit', r)));
  const p = run('recon-host', ['-config', join(dir, 'host.json'), 'run'], { RECON_INPUT_LOG: inputLog, ...env }, name);
  await until(() => /msg="connected to gateway"/.test(p.log) && /direct WebTransport endpoint listening/.test(p.log), 60000, `${name} online`);
  await page.goto(`${base}/`);
  await until(async () => (await page.$$('.host.online')).length === 1, 60000, 'host online after restart');
  return p;
}

async function startStream(prefs) {
  await page.goto(`${base}/`);
  await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify(p)), { stats: true, ...PREFS_2D, ...prefs });
  await page.click('.host.online a.btn-primary');
  await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
  await page.click('#btn-start');
  await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
}

// Stream over direct WebTransport from a host started with faults; measure
// `seconds` after a warm-up.
async function lossRun(name, faults, seconds) {
  const host = await restartHost({ RECON_TEST_FAULTS: faults }, name);
  await startStream({ path: 'auto', transport: 'auto' });
  const calls = await watchDecoder();
  await sleep(4000); // decoder warm-up
  const st0 = await page.evaluate(() => window.__recon.lastStats);
  const log0 = host.log.length;
  const con0 = consoleLines.length;
  const fps = [];
  for (const end = Date.now() + seconds * 1000; Date.now() < end;) {
    await sleep(500);
    fps.push((await page.evaluate(() => window.__recon.lastStats))?.fps ?? 0);
  }
  const st = await page.evaluate(() => window.__recon.lastStats);
  const cfg = await page.evaluate(() => window.__recon.videoCfg);
  const hl = host.log.slice(log0);
  const con = consoleLines.slice(con0);
  const delta = (k) => (st?.[k] ?? 0) - (st0?.[k] ?? 0);
  return {
    name, faults, seconds, cfg, fps: fps.reduce((a, b) => a + b, 0) / Math.max(1, fps.length),
    delayed: (hl.match(/msg="test fault: delaying frame"/g) || []).length,
    dropped: (hl.match(/msg="frames dropped".*? why="test fault"/g) || []).length,
    restarts: restartsByReason(hl),
    keyRequestReasons: keyRequestsByReason(con),
    client: { keyRequests: delta('keyRequests'), hostDropped: delta('hostDropped'), skipped: delta('skipped'), lost: delta('dropped') },
    // The decoder's own error lines, not the key-frame requests they cause.
    decoderErrors: con.filter((l) => l.includes('decoder error:')).length,
    calls, st,
  };
}

async function checkLossHandling() {
  // The scenarios so far ran on a clean loopback link ("lan"): no gap may
  // have been taken for a loss. Restarts for other reasons (settings changes,
  // decoder backlog and congestion on this CPU-only machine) and frames the
  // host dropped on queue overflow are listed for the record.
  const lan = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  const lanKeys = keyRequestsByReason(consoleLines);
  check('clean link (lan): no key frame requested for a gap in the sequence (late frames wait)', !lanKeys['frame lost'],
    `client key-frame requests: ${counts(lanKeys)}; host encoder restarts: ${counts(restartsByReason(lan.log))}; ` +
      `host frame drops: ${(lan.log.match(/msg="frames dropped"/g) || []).length}`);
  results.push({ loss: 'lan', restarts: restartsByReason(lan.log), keyRequests: lanKeys });

  // The debug toggle behind the hardware decoder check in VENDOR_NOTES (1.4):
  // skip one delta frame before the decoder, report whether it errors. Three
  // runs on the clean host: whether a decoder accepts the next P-frame can
  // depend on the frame skipped (AV1 frames inherit entropy-coding state from
  // a reference frame; a non-reference frame can go missing harmlessly).
  await startStream({ path: 'auto', transport: 'auto' });
  await sleep(4000);
  const dts = [];
  for (let i = 0; i < 3; i++) {
    await page.evaluate(() => { window.__recon.dropTest = null; window.__recon.worker.postMessage({ type: 'dropTest' }); });
    const dt = await until(() => page.evaluate(() => window.__recon.dropTest), 8000, 'drop test result').catch(() => null);
    dts.push(dt);
    await sleep(1500); // after a decoder error: the key frame it asked for
  }
  const dtRows = dts.map((d) => (d ? `${d.gen}/${d.seq}: ${d.ok ? 'accepted' : 'error'} (${d.decoded} decoded${d.error ? `, ${d.error}` : ''})` : 'no result'));
  check('drop test (debug toggle) reports whether the decoder accepts the frames after a skipped one',
    dts.every((d) => d && (d.ok || !!d.error)),
    `${dts[0]?.codec} (${dts[0]?.hw ? 'hardware' : 'software'} decoder): ${dtRows.join('; ')}`);
  results.push({ loss: 'dropTest', runs: dts });
  await page.evaluate(() => { window.__recon.userClosed = true; });

  // Recovery "keyframe" (the software encoders have no intra refresh).
  const k = await lossRun('host-faults', LOSS_FAULTS, 20);
  const kfRestarts = k.restarts['keyframe request (urgent)'] || 0;
  // A late frame that outlasted the gap timeout would show as "frame lost";
  // every key-frame restart needs a client request with a logged reason (the
  // drops, a decoder error, the watchdog). Other restarts (congestion) are
  // listed, not checked: this CPU-only machine also falls behind on its own.
  const kRequests = Object.values(k.keyRequestReasons).reduce((a, b) => a + b, 0);
  check('late frames (200 ms) cause no key-frame request ("frame lost"); every key-frame restart answers a logged request',
    k.delayed >= 5 && !k.keyRequestReasons['frame lost'] && kfRestarts <= kRequests,
    `${k.delayed} frames delayed 200 ms, ${k.dropped} dropped; client key-frame requests: ${counts(k.keyRequestReasons)}; ` +
      `host restarts: ${counts(k.restarts)}`);
  // The late frames release bursts of the frames buffered behind them: the
  // decode queue bound must hold there too.
  await checkHygiene('late frames (bursts)', k.calls, k.st);
  check('dropped frames are reported ("dropped") and recovered with a key frame (recovery "keyframe")',
    k.cfg?.recovery === 'keyframe' && k.dropped >= 3 && k.client.hostDropped >= k.dropped - 1 &&
      (k.keyRequestReasons['dropped by host'] || 0) >= 1 && kfRestarts >= 1 && k.fps >= 10,
    `${k.cfg?.encoder} recovery ${k.cfg?.recovery}: host dropped ${k.dropped}, client told ${k.client.hostDropped}, ` +
      `key requests ${k.client.keyRequests} (${counts(k.keyRequestReasons)}), key-frame restarts ${kfRestarts}, ${k.fps.toFixed(1)} fps mean over ${k.seconds} s`);
  await page.evaluate(() => { window.__recon.userClosed = true; });

  // Recovery "skip", forced through the hook: the client skips the dropped
  // frame and decodes on, without a key-frame request. The software AV1
  // encoder has no intra refresh, so the picture stays damaged, and Chrome's
  // AV1 decoder can reject a later frame (entropy-coding state inherited from
  // the missing reference): then, and only then, the decoder-error fallback
  // (reset + key frame) runs. A drop that falls into a generation the client
  // has already given up (decoder backlog or error, waiting for its key
  // frame) needs nothing at all, so not every report is a skip.
  const s = await lossRun('host-faults-skip', `${LOSS_FAULTS},recovery=skip`, 20);
  const skipRestarts = s.restarts['keyframe request (urgent)'] || 0;
  check('dropped frames skipped (recovery "skip"): no key-frame request for the loss itself, playback continues',
    s.cfg?.recovery === 'skip' && s.dropped >= 3 && s.client.hostDropped >= s.dropped - 1 && s.client.skipped >= 1 &&
      !s.keyRequestReasons['dropped by host'] && !s.keyRequestReasons['frame lost'] &&
      skipRestarts <= (s.keyRequestReasons['decoder error'] || 0) + (s.keyRequestReasons.watchdog || 0) && s.fps >= 10,
    `${s.cfg?.encoder} recovery ${s.cfg?.recovery}: host dropped ${s.dropped}, client told ${s.client.hostDropped}, skipped ${s.client.skipped}, ` +
      `decoder errors after a skip ${s.decoderErrors} (fallback: reset + key frame); client key-frame requests: ${counts(s.keyRequestReasons)}; ` +
      `host restarts: ${counts(s.restarts)}; ${s.fps.toFixed(1)} fps mean over ${s.seconds} s`);
  results.push({ loss: 'faults', keyframe: k, skip: s });
  await page.evaluate(() => { window.__recon.userClosed = true; });
}

// ---------------------------------------------------------------------------
// Bitrate recovery (guide step 1.5). A congestion report cuts the bitrate by
// 25 %; while the client's frame acknowledgements show a steady one-way
// delay, the host raises it 15 % at a time back to the setting, each step an
// overlapped (non-urgent) restart, and the overlay shows the target. The
// host's test-only hook shortens the controller's 10 s quiet period and rate
// limit to 2 s. Back-offs of this CPU-only machine's own (decoder backlog)
// may add cuts; the climb back to the setting must still happen, or, after a
// decoder flush, to the cap the host then logs (85 % of the bitrate the
// decoder fell behind at).

async function checkBitrateRecovery() {
  const host = await restartHost({ RECON_TEST_FAULTS: 'rate-period=2s' }, 'host-rate');
  await startStream({ path: 'auto', transport: 'auto', bitrate: 8 });
  await sleep(4000); // decoder warm-up
  const log0 = host.log.length;
  const con0 = consoleLines.length;
  await page.evaluate(() => window.__recon.worker.postMessage({ type: 'ctl', m: { t: 'congestion', delayMs: 100 } }));
  const seen = []; // the configs' bitrates, in order, while the bitrate climbs back
  let cfg = null;
  let overlay = '';
  const changes = (re) => [...host.log.slice(log0).matchAll(re)].map((m) => [+m[1], +m[2]]);
  const cutRe = /msg="congestion: lowering bitrate".*? from=(\d+) to=(\d+)/g;
  const raiseRe = /msg="bitrate recovery: raising bitrate".*? from=(\d+) to=(\d+)/g;
  // The host was restarted for this scenario: its whole log is this session.
  const top = () => Math.min(8000, ...[...host.log.matchAll(/msg="bitrate recovery limited by the client's decoder" max=(\d+)/g)].map((m) => +m[1]));
  for (const end = Date.now() + 45000; Date.now() < end;) {
    await sleep(250);
    const s = await page.evaluate(() => ({ cfg: window.__recon.videoCfg, text: document.getElementById('stats-body')?.innerText || '' }));
    cfg = s.cfg;
    if (cfg?.bitrate && seen[seen.length - 1] !== cfg.bitrate) seen.push(cfg.bitrate);
    if (!overlay && /backed off/.test(s.text)) overlay = s.text; // redrawn with each stats update (500 ms)
    const raises = changes(raiseRe);
    if (changes(cutRe).length && raises.length && raises[raises.length - 1][1] === top() && cfg?.bitrate === top()) break;
  }
  const limit = top();
  const cuts = changes(cutRe);
  const raises = changes(raiseRe);
  const restarts = restartsByReason(host.log.slice(log0));
  const target = (overlay.match(/target\s*([^\n]*)/) || [])[1] || '';
  // Freezes (the client's "freeze: N ms" log): recorded, not checked; on this
  // CPU-only machine every switch also runs a second software encoder.
  const freezes = consoleLines.slice(con0).map((l) => +(l.match(/freeze: (\d+) ms/) || [])[1]).filter((v) => v > 0);
  check('bitrate recovery: a congestion cut, then 15 % raises back to the setting with overlapped restarts; overlay shows the target',
    cuts.length >= 1 && raises.length >= (limit < 8000 ? 1 : 2) && raises.every(([a, b]) => b > a && b <= Math.floor(a * 1.15) + 1) &&
      raises[raises.length - 1][1] === limit && cfg?.bitrate === limit && cfg?.maxBitrate === 8000 &&
      (restarts['bitrate recovery'] || 0) === raises.length && !restarts['bitrate recovery (urgent)'] &&
      /of 8\.0 Mbps \(backed off\)/.test(target),
    `cuts ${cuts.map(([a, b]) => `${a}→${b}`).join(', ')}; raises ${raises.map(([a, b]) => `${a}→${b}`).join(', ')}; ` +
      `configs ${seen.join(' → ')} kbps (max ${cfg?.maxBitrate}${limit < 8000 ? `, decoder limit ${limit}` : ''}); overlay while backed off: "${target}"; restarts: ${counts(restarts)}; ` +
      `freezes > 100 ms: ${freezes.length ? freezes.join(', ') + ' ms' : 'none'}`);
  results.push({ bitrateRecovery: { cuts, raises, configs: seen, decoderLimit: limit < 8000 ? limit : null, restarts, freezes } });
  await page.evaluate(() => { window.__recon.userClosed = true; });
}

// ---------------------------------------------------------------------------

const dir = mkdtempSync(join(tmpdir(), 'recon-e2e-'));
const port = await freePort();
const directPort = await freePort();
const base = `https://127.0.0.1:${port}`;
const inputLog = join(dir, 'input.log');

const gw = run('recon-gateway', ['-listen', `127.0.0.1:${port}`, '-data', join(dir, 'gw')], {}, 'gateway');
await until(() => existsSync(join(dir, 'gw', 'setup-token.txt')), 10000, 'gateway setup token');
const setupToken = readFileSync(join(dir, 'gw', 'setup-token.txt'), 'utf8').trim();

const browser = await chromium.launch({
  headless: true,
  args: ['--autoplay-policy=no-user-gesture-required', '--enable-unsafe-webgpu', '--ignore-gpu-blocklist'],
});
const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 720 } });
// The page the checks drive: the headless one, or the headed one (headedPage)
// for the scenarios that need it.
let page = await ctx.newPage();
// The headed browser's pages: 768x432 CSS px at a device pixel ratio of
// 1.25, so the renderers' canvas (sized to device pixels) is 960x540, the
// test pattern's size: WebGL2 and WebGPU draw through a CPU-emulated GPU here
// (SwiftShader, llvmpipe), and their scenarios stream at 30 fps for the same
// reason.
const HEADED_VIEWPORT = { width: 768, height: 432 };
const HEADED_DPR = 1.25;
// The upscaling scenario's stream: half the headed canvas each way, at a
// frame rate SwiftShader keeps up with while it runs FSR's passes.
const FSR_STREAM = { w: 480, h: 270, fps: 15 };
// Saved settings as the client writes them (rendererV: written since step
// 4.3; without it a saved "canvas2d", the earlier default, reads as "auto").
// The 2D canvas unless a scenario picks another renderer.
const PREFS_2D = { rendererV: 2, renderer: 'canvas2d' };
const consoleLines = [];
const onConsole = (m) => { consoleLines.push(`[${m.type()}] ${m.text()}`); if (process.env.E2E_VERBOSE) console.log('[page]', m.text()); };
const onPageError = (e) => consoleLines.push(`[pageerror] ${e.message}`);
page.on('console', onConsole);
page.on('pageerror', onPageError);

let failed = false;
try {
  // 1. First-run setup through the UI ----------------------------------------
  await page.goto(`${base}/login`);
  await page.waitForSelector('#setup-form:not(.hidden)');
  await page.fill('#setup-token', setupToken);
  await page.fill('#setup-user', 'admin');
  await page.fill('#setup-pass', 'correct-horse-battery');
  await page.fill('#setup-pass2', 'correct-horse-battery');
  await page.click('#setup-form button[type=submit]');
  await page.waitForURL(`${base}/`);
  check('first-run setup creates admin and signs in', true);

  const isolated = await page.evaluate(() => self.crossOriginIsolated);
  check('page is cross-origin isolated (SharedArrayBuffer audio ring)', isolated === true);

  // 2. Add a host and pair the agent ------------------------------------------
  await page.click('#add-host');
  await page.fill('.modal input', 'E2E Test PC');
  await page.click('.modal .btn-primary');
  const cmd = await (await page.waitForSelector('.modal .code-box')).textContent();
  const code = cmd.match(/"(recon1:[^"]+)"/)[1];
  check('pairing code issued', code.startsWith('recon1:'));

  const cfgPath = join(dir, 'host.json');
  const hostCfg = { capture: 'test', testWidth: 960, testHeight: 540, testPad: TEST_PAD, directPort, directAddr: '127.0.0.1', audio: true, logLevel: 'debug' };
  if (process.env.E2E_HOST_FFMPEG) hostCfg.ffmpeg = process.env.E2E_HOST_FFMPEG;
  writeFileSync(cfgPath, JSON.stringify(hostCfg));
  const pair = run('recon-host', ['-config', cfgPath, 'pair', code], {}, 'pair');
  await new Promise((r) => pair.on('exit', r));
  check('host agent paired', pair.exitCode === 0, pair.log.trim().split('\n')[0]);
  const host = run('recon-host', ['-config', cfgPath, 'run'], { RECON_INPUT_LOG: inputLog }, 'host');

  await page.waitForSelector('.modal .status .dot.on', { timeout: 90000 });
  check('pairing dialog detects the agent coming online', true);
  await until(async () => (await page.$$('.host.online')).length === 1, 60000, 'host online on dashboard');
  await page.screenshot({ path: join(outDir, 'dashboard.png') });
  check('host shows online on the dashboard', true);

  // 3. Stream over each path -------------------------------------------------
  const scenarios = [
    { name: 'WebTransport direct', prefs: { path: 'auto', transport: 'auto', renderer: 'canvas2d' }, expect: ['webtransport', 'direct'], pacing: 'fallbacks' },
    { name: 'WebTransport relay', prefs: { path: 'relay', transport: 'auto', renderer: 'canvas2d' }, expect: ['webtransport', 'relay'] },
    { name: 'WebSocket relay', prefs: { path: 'relay', transport: 'websocket', renderer: 'canvas2d' }, expect: ['websocket', 'relay'] },
    // WebGL2 and WebGPU draw through a CPU-emulated GPU here (30 fps streams):
    // in the headed browser on Xvfb (HEADED_VIEWPORT), where WebGL2 runs on
    // llvmpipe (headless: SwiftShader, about 12 of 30 fps on 4 cores) and
    // WebGPU works at all (it fails in headless Chromium, see
    // checkRendererCrop). Without Xvfb WebGL2 runs headless.
    { name: 'WebGL2 renderer', prefs: { path: 'auto', transport: 'auto', renderer: 'webgl2', fps: 30 }, expect: ['webtransport', 'direct'], probe: 'webgl2 readback', headed: 'prefer', pacing: true },
    { name: 'WebGPU renderer', prefs: { path: 'auto', transport: 'auto', renderer: 'webgpu', fps: 30 }, expect: ['webtransport', 'direct'], probe: 'webgpu readback', headed: true, pacing: true },
    // Client-side upscaling (Phase 5, checkUpscaleStream): upscaling Auto,
    // the stream at half the size of the headed canvas (FSR_STREAM, asked
    // for live once connected), so FSR 1 draws it at 2x; then Off, live.
    // SwiftShader cannot run FSR on the 960x540 stream into a 1920x1080
    // canvas in real time (about 0.45 s of emulated GPU per frame here, see
    // docs/VENDOR_NOTES.md, Phase 5): that geometry is checked frame by frame
    // in checkUpscaleUnit, this one at the same factor in real time.
    { name: 'WebGPU upscaling (FSR)', prefs: { path: 'auto', transport: 'auto', renderer: 'webgpu', fps: FSR_STREAM.fps, upscale: 'auto' }, expect: ['webtransport', 'direct'], probe: 'webgpu readback', headed: true, size: [FSR_STREAM.w, FSR_STREAM.h], upscale: true },
  ];
  if (process.env.E2E_ROTATE) scenarios.push(scenarios.shift());

  // Unmeasured warm-up stream. On CPU-only CI machines the browser's first
  // software AV1 decoder instance falls behind for a few seconds (the client
  // detects this, flushes and recovers); hardware decoding is not affected.
  await page.goto(`${base}/`);
  await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify(p)), { path: 'relay', ...PREFS_2D });
  await page.click('.host.online a.btn-primary');
  await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
  await page.click('#btn-start');
  await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
  await sleep(6000);
  const warmStart = Date.now();
  let warm = await page.evaluate(() => window.__recon.lastStats);
  while ((!warm || warm.fps < 30) && Date.now() - warmStart < 10000) {
    await sleep(500);
    warm = await page.evaluate(() => window.__recon.lastStats);
  }
  check('warm-up stream (self-heals if the decoder falls behind)', warm && warm.fps >= 30,
    `${warm?.fps.toFixed(1)} fps, ${warm?.keyRequests} recovery key frames, waited ${((Date.now() - warmStart) / 1000).toFixed(1)} s extra`);
  await page.evaluate(() => { window.__recon.userClosed = true; });
  const mainPage = page;
  for (const sc of scenarios) {
    page = mainPage;
    if (sc.headed) {
      const hp = await headedPage().catch((e) => { console.log(`- headed browser: ${e.message}`); return null; });
      if (hp) page = hp;
      else if (sc.headed === 'prefer') console.log(`- ${sc.name}: headless (no Xvfb for a headed browser)`);
      else {
        console.log(`- ${sc.name}: skipped (needs a headed browser on Xvfb: WebGPU fails in headless Chromium here)`);
        continue;
      }
    }
    writeFileSync(inputLog, '');
    await page.goto(`${base}/`);
    await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify({ stats: true, ...p })), { ...PREFS_2D, ...sc.prefs });
    await page.click('.host.online a.btn-primary');
    await page.waitForURL(/\/stream\?host=/);
    await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
    const t0 = Date.now();
    await page.click('#btn-start');
    await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
    const firstFrameMs = Date.now() - t0;
    const conn = await page.evaluate(() => window.__recon.conn);
    check(`${sc.name}: connected`, conn.transport === sc.expect[0] && conn.path === sc.expect[1] && conn.renderer === sc.prefs.renderer,
      `${conn.transport}/${conn.path}, renderer ${conn.renderer}, first frame after ${firstFrameMs} ms`);
    if (sc.size) {
      // The stream's size, asked for live (Settings has no such resolution): a new encoder generation.
      const prefs = { codec: 'auto', bitrate: 30000, fps: sc.prefs.fps, width: sc.size[0], height: sc.size[1], monitor: 0, audio: true, audioCodec: 'opus', cursor: 'local', quality: 'balanced', adaptive: true };
      await page.evaluate((m) => window.__recon.worker.postMessage({ type: 'ctl', m }), { t: 'settings', prefs });
      const sized = await until(() => page.evaluate((w) => window.__recon.video.w === w, sc.size[0]), 15000, `the stream at ${sc.size.join('x')}`).catch(() => false);
      check(`${sc.name}: the stream restarts at ${sc.size.join('x')} (live settings change)`, sized, JSON.stringify(await page.evaluate(() => window.__recon.video)));
    }
    const calls = await watchDecoder();

    // Wait for steady state (software decoders need a moment to warm up on
    // small CI machines), then measure a fresh stats window.
    const settleStart = Date.now();
    const timeline = [];
    for (let i = 0; i < 16; i++) {
      await sleep(500);
      const x = await page.evaluate(() => window.__recon.lastStats);
      if (x) timeline.push({ t: Date.now() - settleStart, fps: +x.fps.toFixed(1), decode: x.decode && +x.decode.toFixed(1), total: x.total && +x.total.toFixed(1), q: x.queue, mbps: +x.mbps.toFixed(1), keyReq: x.keyRequests });
    }
    results.push({ timeline: sc.name, points: timeline });
    // Steady state = the last 1.5 s of the 8 s window all at real-time rate.
    const tail = timeline.slice(-3);
    const avg = tail.reduce((a, p) => a + p.fps, 0) / Math.max(1, tail.length);
    const rate = sc.prefs.fps || 60; // the stream's frame rate
    const steady = tail.length === 3 && avg >= (rate * 5) / 6; // per-0.5 s samples jitter when frames bunch at a boundary
    const st = await page.evaluate(() => window.__recon.lastStats);
    check(`${sc.name}: steady real-time playback`, steady, `last 1.5 s: ${tail.map((p) => p.fps).join(' / ')} fps; key requests ${st?.keyRequests}`);
    const cfg = await page.evaluate(() => window.__recon.videoCfg);
    check(`${sc.name}: video decoding`, st && st.fps > rate * 0.75, `${st?.fps.toFixed(1)} fps of ${rate}, ${st?.mbps.toFixed(2)} Mbps, codec ${cfg?.codec} via ${cfg?.encoder}`);
    check(`${sc.name}: latency measured`, st && st.synced && st.total !== null,
      `stream ${st?.total?.toFixed(1)} ms (network ${st?.owd?.toFixed(2)} ms, decode ${st?.decode?.toFixed(2)} ms, RTT ${st?.rtt?.toFixed(2)} ms)`);
    await checkStages(sc.name, st);
    await checkCrop(sc.name);
    await checkPresentation(sc.name, sc.prefs.renderer);
    await checkHygiene(sc.name, calls, st);
    if (sc === scenarios[0]) await checkSelfTest(cfg);
    const pr = await checkProbe(sc.name, sc.probe, rate);
    // (Fullscreen is the WebGPU scenario's; the upscaling one has no more to show there.)
    if (sc.name === 'WebTransport direct' || (sc.probe && !sc.upscale)) await checkFullscreen(sc.name);
    if (sc.upscale) await checkUpscaleStream(sc, rate).catch((e) => check(`${sc.name}: upscaling`, false, e.message));
    check(`${sc.name}: audio`, st && st.audioPackets > 50, `${st?.audioPackets} packets/0.5 s window cumulative, buffer ${st?.audioMs?.toFixed(0)} ms, lost ${st?.audioLost}`);
    results.push({ scenario: sc.name, stats: st, firstFrameMs, conn, cfg });

    // Input: keyboard + mouse (desktop mode = absolute) + wheel.
    await page.mouse.move(640, 360);
    await page.mouse.move(700, 400, { steps: 5 });
    await page.mouse.down();
    await page.mouse.up();
    await page.mouse.wheel(0, 300);
    await page.keyboard.press('KeyW');
    await page.keyboard.press('ArrowUp');
    if (!nativeInputLog) {
      await sleep(500);
      const hostProc = procs.find((p) => p.spawnargs.includes('run'));
      const injectErrors = (hostProc.log.match(/msg=inject/g) || []).length;
      const sessionLine = (hostProc.log.match(/session started.*/g) || []).pop();
      check(`${sc.name}: input injected via SendInput without errors`, injectErrors === 0 && !!sessionLine, `${injectErrors} injection errors`);
      if (sc.name !== 'WebTransport direct') { await page.evaluate(() => { window.__recon.userClosed = true; }); continue; }
    }
    const events = nativeInputLog ? await until(() => {
      const lines = readFileSync(inputLog, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l));
      const keys = lines.filter((e) => e.ev === 'key');
      const ok = lines.some((e) => e.ev === 'abs') && lines.some((e) => e.ev === 'button' && e.down) &&
        lines.some((e) => e.ev === 'wheel') && keys.some((e) => e.sc === 0x11 && e.down) && keys.some((e) => e.sc === 0x48 && e.ext);
      return ok ? lines : null;
    }, 5000, 'input events on host').catch((e) => { check(`${sc.name}: input`, false, e.message); return null; }) : null;
    if (events) {
      const abs = events.filter((e) => e.ev === 'abs').pop();
      check(`${sc.name}: keyboard + mouse reach the host`, true, `${events.length} events; last abs (${abs.x}, ${abs.y}); W=0x11, ArrowUp=E0 48`);
    }
    if (sc.name === 'WebTransport direct') {
      // Export latency data from the overlay: a JSON download with the histogram.
      const [dl] = await Promise.all([page.waitForEvent('download', { timeout: 5000 }), page.click('#btn-export-latency')]).catch(() => [null]);
      const exp = dl ? JSON.parse(readFileSync(await dl.path(), 'utf8')) : null;
      const expHist = exp?.probe?.histogram ? Object.values(exp.probe.histogram).reduce((a, b) => a + b, 0) : 0;
      check('overlay "Export latency data" downloads the probe histogram as JSON',
        exp?.kind === 'kloudit-recon-latency' && exp.probe?.mode === 'seq' && expHist >= (pr?.latency?.n || 1) &&
          exp.probe.samples?.length === expHist && !!exp.stages?.e2e && exp.video?.encoder === cfg?.encoder,
        exp ? `${dl.suggestedFilename()}: ${expHist} histogram samples, ${exp.probe.samples?.length} sample rows, stages ${!!exp.stages}` : 'no download');
      await page.mouse.move(640, 5); // reveal toolbar
      await sleep(400);
      await page.screenshot({ path: join(outDir, 'stream.png') });
      // Live settings change: bitrate -> overlapped encoder restart, no reconnect.
      const genBefore = cfg.gen;
      await page.evaluate(() => window.__recon.worker.postMessage({ type: 'ctl', m: { t: 'settings', prefs: { bitrate: 8000, fps: 60 } } }));
      await page.waitForFunction((g) => window.__recon.videoCfg && window.__recon.videoCfg.gen !== g, genBefore, { timeout: 10000 });
      await sleep(1500);
      const st2 = await page.evaluate(() => window.__recon.lastStats);
      check('live settings change (new encoder generation, stream continues)', st2.fps > 40, `${st2.fps.toFixed(1)} fps after switch`);
      // The client reports its stage summary every 10 s; the host logs it per encoder/vendor.
      const hostProc = procs.find((p) => p.spawnargs.includes('run'));
      const line = await until(() => (hostProc.log.match(/msg="latency stages[^\n]*/) || [])[0], 15000, 'stage summary in the host log').catch(() => '');
      check('host logs the client stage summary with encoder and vendor', /encoder=\S+ vendor=\S+/.test(line) && / e2e=/.test(line), line.replace(/^.*?msg=/, '').slice(0, 260));
      // Reference outside the client's clock sync and stage arithmetic: the host's
      // own capture->encoded and queue times of the frames the client acknowledged
      // (the frames it records stages for) in the same 10 s. Equal p50s mean the
      // stamps reach the client intact and its percentiles are right; they do not
      // validate the host stamps themselves (the 0.2 barcode does).
      const p50 = (key) => Number((line.match(new RegExp(` ${key}="([\\d.]+)/`)) || [])[1] ?? NaN);
      const rows = ['capture', 'queue'].map((k) => ({ k, client: p50(k), host: p50(`host_${k}`) }));
      check("client capture/queue p50 match the host's own measurement (±max(2 ms, 10 %))",
        rows.every((r) => Math.abs(r.client - r.host) <= Math.max(2, 0.1 * r.host)),
        rows.map((r) => `${r.k}: client ${r.client} vs host ${r.host} ms`).join('; '));
      // The drawer's adaptive bitrate switch reaches the host: a new encoder
      // generation with fixed-bitrate rate control (AMF: latency-constrained
      // VBR instead of CBR), and back. Waits for the host's own start line with
      // the new value: key-frame and congestion restarts also start
      // generations, with whatever the host's settings are at that moment.
      const toggleAdaptive = async (want) => {
        const from = hostProc.log.length;
        await page.evaluate(() => [...document.querySelectorAll('#drawer label.check')]
          .find((l) => l.textContent.includes('Adaptive bitrate')).querySelector('input').click());
        return until(() => (hostProc.log.slice(from).match(/msg="starting encoder"[^\n]*/g) || [])
          .find((l) => l.includes(` adaptive=${want}`)), 10000, `starting encoder with adaptive=${want}`);
      };
      const off = await toggleAdaptive(false).catch((e) => e.message);
      const on = await toggleAdaptive(true).catch((e) => e.message);
      check('adaptive bitrate switch restarts the encoder with the setting',
        /msg="starting encoder".* adaptive=false/.test(off) && /msg="starting encoder".* adaptive=true/.test(on),
        `off: ${off.replace(/^.*?msg=/, '').slice(0, 160)}; on: ${on.replace(/^.*?msg=/, '').slice(0, 160)}`);
    }
    // Frame pacing (step 4.4): Smooth and back, live, on this renderer.
    if (sc.pacing) await checkPacing(sc.name, sc.prefs.fps || 60, sc.pacing === 'fallbacks').catch((e) => check(`${sc.name}: frame pacing`, false, e.message));
    await page.evaluate(() => { window.__recon.userClosed = true; });
  }
  page = mainPage;

  // 3a. Renderer "auto": the presentation bake-off -----------------------------
  await checkBakeoff().catch((e) => check('renderer auto (bake-off) scenario', false, e.message));

  // 3b. Loss handling with the host's fault-injection hook --------------------
  await checkLossHandling().catch((e) => check('loss handling scenario', false, e.message));
  await checkBitrateRecovery().catch((e) => check('bitrate recovery scenario', false, e.message));
  await checkPreStageHoldHost().catch((e) => check('host before step 4.4 scenario', false, e.message));

  // 3c. Renderers (unit) ---------------------------------------------------------
  const xvfbOk = spawnSync('sh', ['-c', 'command -v Xvfb']).status === 0;
  await checkRendererCrop(xvfbOk).catch((e) => check('renderer crop (unit)', false, e.message));
  await checkUpscaleUnit(xvfbOk).catch((e) => check('FSR 1 shader (unit)', false, e.message));
  await checkPickRule().catch((e) => check("Auto's pick (unit)", false, e.message));
  await checkPacerRule().catch((e) => check('frame pacing (unit)', false, e.message));
  await checkSelfTestLogic().catch((e) => check('decoder self-test logic (unit)', false, e.message));

  // 3d. Latency probe, wallclock mode -----------------------------------------
  // The host captures an X display (x11grab) that shows tools/latency-test in a
  // second, headed Chromium; the client (latency probe enabled) reads the
  // page's wall-clock barcode back and converts it with the host's wall-clock
  // offset and the clock sync. page→capture (barcode time vs the frame's
  // capture stamp) must be small and never negative: a wrong offset or clock
  // conversion shifts it by the error.
  const ffmpegBin = process.env.E2E_HOST_FFMPEG || 'ffmpeg';
  const haveX = spawnSync('sh', ['-c', 'command -v Xvfb']).status === 0 &&
    /x11grab/.test(spawnSync(ffmpegBin, ['-hide_banner', '-devices'], { encoding: 'utf8' }).stdout || '');
  if (process.env.E2E_WALLCLOCK === '0' || hostBin || !haveX) {
    console.log('- latency probe wallclock scenario skipped (needs Xvfb, an FFmpeg with x11grab and the native host)');
  } else {
    await checkWallclockProbe().catch((e) => check('latency probe (wallclock) scenario', false, e.message));
  }

  // 4. Security spot checks from the browser ---------------------------------
  const anon = await browser.newContext({ ignoreHTTPSErrors: true });
  const ap = await anon.newPage();
  const status = await ap.evaluate(async (b) => (await fetch(b + '/api/hosts')).status, base).catch(() => 0);
  await ap.goto(`${base}/api/hosts`).catch(() => {});
  const st = await ap.evaluate(async () => (await fetch('/api/hosts')).status);
  check('anonymous API access rejected', st === 401, `status ${st}`);
  const csp = (await (await ap.goto(`${base}/`)).allHeaders())['content-security-policy'] || '';
  check('strict CSP served', csp.includes("script-src 'self'") && csp.includes("frame-ancestors 'none'"));
  await anon.close();
  void status;
} catch (e) {
  failed = true;
  console.error('E2E failure:', e);
  await page.screenshot({ path: join(outDir, 'failure.png') }).catch(() => {});
} finally {
  writeFileSync(join(outDir, 'results.json'), JSON.stringify(results, null, 2));
  const appLogs = await page.evaluate(() => (window.__recon ? window.__recon.logs : [])).catch(() => []);
  writeFileSync(join(outDir, 'console.log'), consoleLines.concat(appLogs).join('\n'));
  await testPage?.close().catch(() => {});
  await headed?.browser.close().catch(() => {});
  rmSync(join(dir, 'testpage-profile'), { recursive: true, force: true });
  writeFileSync(join(outDir, 'host.log'), procs.filter((p) => p.spawnargs.includes('run')).map((p) => p.log).join('\n--- host restarted ---\n'));
  writeFileSync(join(outDir, 'gateway.log'), gw.log);
  await browser.close();
  cleanup();
}

const bad = results.filter((r) => r.ok === false);
console.log(`\n${results.filter((r) => r.ok === true).length} checks passed, ${bad.length} failed`);
process.exit(failed || bad.length ? 1 : 0);
