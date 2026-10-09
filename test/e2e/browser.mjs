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
// canvas with upscaling Auto (FSR 1 draws), then Off, live. Input and audio
// (step 4.6): on this loopback link the host switches Opus to 5 ms frames
// from the RTT the client's pings report and the jitter buffer adapts within
// 10-60 ms; fullscreen holds Chromium's Keyboard Lock, and Safari's
// fullscreen option (keyboardLock: "browser") is checked on a stubbed
// requestFullscreen; a fake gamepad's triggers come back as force feedback
// (host test hook rumble-echo) and are played with
// vibrationActuator.playEffect; the client reports unadjustedMovement only
// when the browser read the option. Datagram + FEC (step 2.5): with host
// config fec "on" and 3 % of the video shards lost (host test hook
// fec-loss) every frame comes as datagram shards and is rebuilt from parity or
// repaired after a NACK, with steady playback and the stage and barcode
// checks; WebSocket sessions never get shards. E2E_FEC_COMPARE=<seconds>
// adds the comparison with frame streams through a UDP proxy that delays
// each direction 20 ms (40 ms RTT) and loses 1 % / 3 % of the packets:
// stalls over 50 ms (the median of E2E_FEC_COMPARE_RUNS runs per mode,
// default 3) and the overhead of the shards. HDR10 (steps 3.9 / 4.5): the
// host allows HDR ("hdr": "auto"), every scenario before the HDR one streams
// SDR to clients whose display is SDR (unchanged); the WebGPU renderer's HDR
// shader against a CPU reference (unit), and a scenario on a page that plays
// an HDR display: a 10-bit PQ AV1 stream presented with extended range,
// checked to the pixel, then HDR Off live (tone-mapped, then an SDR stream).
// E2E_ONLY=<regex> runs the scenarios and sections whose names match
// (development).

import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, existsSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import net from 'node:net';
import dgram from 'node:dgram';
import vm from 'node:vm';

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

function freeUdpPort() {
  return new Promise((res) => {
    const s = dgram.createSocket('udp4');
    s.bind(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); });
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
// E2E_ONLY=<regex>: only the scenarios and sections whose names match (development runs).
const want = (name) => !process.env.E2E_ONLY || new RegExp(process.env.E2E_ONLY, 'i').test(name);
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
  // Whether the toolbar showed at all, from the page (a starved page shows
  // it late, and may hide it again between two polls of the test).
  await page.evaluate(() => {
    const t = document.getElementById('toolbar');
    window.__e2eToolbar = !t.classList.contains('hide');
    new MutationObserver(() => { if (!t.classList.contains('hide')) window.__e2eToolbar = true; }).observe(t, { attributes: true, attributeFilter: ['class'] });
  });
  await page.mouse.move(100, 60);
  await page.mouse.move(100, 2, { steps: 3 });
  const shown = await until(() => page.evaluate(() => window.__e2eToolbar || null), 8000, 'toolbar').catch(() => false);
  await page.click('#btn-fullscreen', { timeout: 3000 }).catch(() => {});
  let via = 'toolbar button';
  let el = await fsElement(3000);
  if (!el) {
    via = 'hotkey (the button click missed)';
    await page.keyboard.press('Control+Alt+Shift+KeyF');
    el = await fsElement(6000);
  }
  const inFs = await sized();
  // keyboard.lock() resolves asynchronously (slowly on a loaded machine).
  const kbLock = el ? await until(() => page.evaluate(() => window.__recon.keyboardLock), 8000, 'keyboard lock').catch(() => null) : null;
  await page.keyboard.press('Control+Alt+Shift+KeyF');
  const left = await until(() => page.evaluate(() => !document.fullscreenElement), 6000, 'leaving fullscreen').catch(() => false);
  const out = await sized();
  const fits = (x) => x.canvas?.[0] === x.box?.w && x.canvas?.[1] === x.box?.h;
  const refused = (await page.evaluate(() => window.__recon.logs)).filter((l) => l.includes('fullscreen refused')).pop() || '';
  check(`${name}: toolbar at the top edge, element fullscreen of the player and back, the canvas follows its box`, shown && el === 'player' && left && fits(inFs) && fits(out),
    `toolbar shown ${shown}; fullscreen element ${el} via ${via}; in fullscreen box ${inFs.box?.w}x${inFs.box?.h}, canvas ${inFs.canvas?.join('x')}; ` +
      `after: box ${out.box?.w}x${out.box?.h}, canvas ${out.canvas?.join('x')}${refused ? `; ${refused.replace(/^\S+ /, '')}` : ''}`);
  // Keyboard Lock (step 4.6): Chromium has navigator.keyboard.lock(). Judged
  // when fullscreen went in and out as asked (on a loaded machine a late
  // toolbar click and the hotkey can cross; the check above reports that).
  const kbAfter = await until(async () => ((await page.evaluate(() => window.__recon.keyboardLock)) === null ? 'off' : null), 3000, 'keyboard lock released')
    .then(() => null, () => page.evaluate(() => window.__recon.keyboardLock));
  const kbLog = (await page.evaluate(() => window.__recon.logs)).filter((l) => l.includes('keyboard lock')).pop() || '';
  const kbDetail = `in fullscreen: ${kbLock}; after: ${kbAfter}${kbLog ? `; ${kbLog.replace(/^\S+ /, '')}` : ''}`;
  if (el === 'player' && left) {
    check(`${name}: Keyboard Lock held in fullscreen (navigator.keyboard.lock), released with it`, kbLock === 'keyboard.lock' && kbAfter === null, kbDetail);
  } else {
    console.log(`- ${name}: Keyboard Lock not judged (fullscreen ${el ?? 'not entered'}, left ${left}): ${kbDetail}`);
  }
}

// Audio (step 4.6). This loopback link is a LAN (minimum RTT far below
// 10 ms): once the client's pings report the RTT the host switches Opus to
// 5 ms frames (log "audio frame size"), announces them in an audio config,
// and the client receives 5 ms packets (their Opus TOC); the jitter buffer
// (Auto, the default) keeps a target within 10-60 ms; the overlay shows both.
async function checkAudio(name) {
  const hostProc = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  const read = () => page.evaluate(() => ({ st: window.__recon.lastStats, cfg: window.__recon.audioCfg, j: window.__recon.audioJitter }));
  const r = (await until(async () => {
    const x = await read();
    return x.st?.audioFrameMs === 5 && x.cfg?.frameMs === 5 && x.j ? x : null;
  }, 8000, 'audio at 5 ms').catch(() => null)) || (await read());
  const sid = await page.evaluate(() => window.__recon.welcome?.session);
  const line = (hostProc.log.match(new RegExp(`msg="audio frame size" session=${sid} [^\\n]*`)) || [''])[0];
  const overlay = await page.textContent('#stats').catch(() => '');
  const row = (overlay.match(/Audio[^\n]*?lost \d+/) || [''])[0];
  const { st, cfg, j } = r;
  check(`${name}: audio: Opus 5 ms frames on this LAN (picked by the host from the client's minimum RTT), adaptive jitter buffer within 10-60 ms`,
    st?.audioFrameMs === 5 && cfg?.frameMs === 5 && / ms=5 /.test(line) && st.minRtt > 0 && st.minRtt < 10 && j?.auto === true &&
      j.targetMs >= 10 && j.targetMs <= 60 && /opus 5 ms/.test(row) && / auto /.test(row),
    `packets ${st?.audioFrameMs} ms, config ${cfg?.frameMs} ms, client min RTT ${st?.minRtt?.toFixed(2)} ms; host: ${line.replace(/^.*?msg=/, '') || 'no switch logged'}; ` +
      `jitter buffer: target ${j?.targetMs?.toFixed(1)} ms (${j?.auto ? 'auto' : 'fixed'}), level ${j?.levelMs?.toFixed(1)} ms, underruns ${j?.underruns}, ` +
      `trimmed ${j?.skippedMs?.toFixed(1)} ms; overlay "${row}"`);
  results.push({ audio: name, stats: { audioFrameMs: st?.audioFrameMs, minRtt: st?.minRtt, audioMs: st?.audioMs }, cfg, jitter: j });

  // A new audio stream with the same codec: the codec setting changes, the
  // codec does not (here "" — the host's choice, Opus — and back to "opus";
  // a client without an Opus decoder gets PCM for either setting). The host
  // restarts audio, its sequence from 0; the client takes the new stream at
  // once (it used to drop its packets as late until their sequence passed
  // the old stream's: as long as audio had run; the old stream's last
  // datagrams, read after the new config, did that too in about one restart
  // in ten here: AUDIO_MAX_LATE). Audio flows: the ring the
  // worker fills holds more than one render quantum (2.7 ms; a starved
  // jitter buffer keeps less) in at least half the samples over the 3 s
  // after each switch.
  const restarts = () => (hostProc.log.match(new RegExp(`msg="audio capture packet" session=${sid} `, 'g')) || []).length;
  const restartWith = async (v) => {
    const before = restarts();
    await page.evaluate((v) => {
      const sel = [...document.querySelectorAll('#drawer select')].find((x) => [...x.options].some((o) => o.value === 'pcm'));
      if (![...sel.options].some((o) => o.value === v)) sel.append(new Option('host default', v));
      sel.value = v;
      sel.dispatchEvent(new Event('change'));
    }, v);
    await until(async () => restarts() > before, 5000, 'audio restart').catch(() => {});
    const ms = [];
    const pk0 = await page.evaluate(() => window.__recon.lastStats?.audioPackets ?? 0);
    for (let i = 0; i < 12; i++) {
      await sleep(250);
      ms.push(await page.evaluate(() => window.__recon.lastStats?.audioMs ?? 0));
    }
    const packets = (await page.evaluate(() => window.__recon.lastStats?.audioPackets ?? 0)) - pk0;
    return { restarted: restarts() > before, flowing: ms.filter((x) => x > 2.7).length, ms: ms.map((x) => Math.round(x)), packets };
  };
  const toDefault = await restartWith('');
  const toOpus = await restartWith('opus');
  check(`${name}: audio: a restart with the same codec (codec setting "" and back to "opus"): the client plays the new stream at once (sequence from 0)`,
    toDefault.restarted && toOpus.restarted && toDefault.flowing >= 6 && toOpus.flowing >= 6,
    `restarted ${toDefault.restarted}/${toOpus.restarted}; buffered audio in ${toDefault.flowing} and ${toOpus.flowing} of 12 samples ` +
      `(ms: ${toDefault.ms.join(' ')} | ${toOpus.ms.join(' ')}); audio packets received meanwhile ${toDefault.packets} | ${toOpus.packets}`);
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
    const bc0 = cpuTimes();
    // Until the result: the start-up toolbar or game-mode hint over the
    // canvas while the paths are measured? (Other toasts, e.g. a decoder
    // warning on a loaded machine, are listed, not failed.)
    const covered = [];
    const other = new Set();
    let samples = 0;
    const cpuSecs = [[Date.now(), bc0]]; // the CPUs' times about every second of the bake-off
    const result = await until(async () => {
      if (Date.now() - cpuSecs[cpuSecs.length - 1][0] >= 1000) cpuSecs.push([Date.now(), cpuTimes()]);
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
    // Starved: the CPUs had nothing to spare in most of its seconds (the
    // paths take turns, and the emulated GPU's turn starves the others).
    const bIdle = idleShare(bc0, cpuTimes());
    const perSec = cpuSecs.slice(1).map(([, c], i) => idleShare(cpuSecs[i][1], c)).filter((v) => v != null).sort((a, b) => a - b);
    const bMedian = perSec.length ? perSec[perSec.length >> 1] : bIdle;
    const bStarved = bMedian != null && bMedian < STARVED_IDLE;
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
    const bw = await rateWindow(1500); // stats updates from the winner
    const st = await page.evaluate(() => window.__recon.lastStats);
    const br = starvedRate(st?.fps ?? 0, 30, 2 / 3, bw);
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
    // Every path measured in both rounds with 30 draws at least; where the
    // CPUs had nothing to spare during the bake-off, a path that drew fewer
    // must be one the rule left out for it (out: too few samples).
    const enough = (p) => res[p]?.draw?.n >= 30 || (bStarved && res[p]?.out === 'too few samples' && res[p].draw?.n >= 1);
    check(`renderer auto: bake-off on the live stream measures ${all.join(', ')} in two rounds, Auto's pick keeps drawing and is stored`,
      !!result?.winner && result.winner === want && measured.length >= 2 && all.every((p) => enough(p) && res[p].drawRounds?.length === 2 && res[p].display.n >= 1) &&
        !!result.why && st?.renderer?.name === result.winner && st.renderer.bake?.done && stored?.winner === result.winner && stored.why === result.why &&
        canvases.length === 1 && canvases[0] === result.winner && overlay.includes('bake-off') && overlay.includes('★ ') && overlay.includes(result.why) && (st.fps > 20 || br.ok),
      `${page === mainPage ? 'headless' : 'headed'}, ${took.toFixed(1)} s: ${result?.winner} (${result?.why}; rule wants ${want}); ${['canvas2d', 'webgl2', 'webgpu'].map(row).join('; ')}; ` +
        `canvases left: ${canvases.join(', ')}; stored key ${stored?.key}; ${st?.fps?.toFixed(1)} fps after${br.note}; ` +
        `during the bake-off CPUs ${bIdle == null ? '?' : (100 * bIdle).toFixed(0)} % idle (median second ${bMedian == null ? '?' : (100 * bMedian).toFixed(0)} %)`);
    await checkHygiene('renderer auto (bake-off switches)', calls, st);
    const pc = st?.pacing?.counts;
    // Never on decode or the page's ticks; from the worker's refresh callbacks
    // (the watchdog's timer where the emulated GPU starves them while WebGPU
    // presents), 250 draws from the refresh at least, or where the CPUs had
    // nothing to spare during the bake-off (fewer refreshes), 100 and four
    // fifths of the draws.
    check('renderer auto: the bake-off in frame pacing Smooth: every path drew on the display refresh, the result names the mode',
      result?.pacing === 'smooth' && stored?.pacing === 'smooth' && st?.pacing?.mode === 'smooth' && pc?.hop === 0 && pc.main === 0 &&
        (pc.raf >= 250 || (bStarved && pc.raf >= 100 && pc.raf >= 0.8 * (pc.raf + pc.timer))),
      `result pacing ${result?.pacing}, stored ${stored?.pacing}; draws by tick source this session ${JSON.stringify(pc)}; ` +
        `CPUs ${bIdle == null ? '?' : (100 * bIdle).toFixed(0)} % idle during the bake-off (median second ${bMedian == null ? '?' : (100 * bMedian).toFixed(0)} %)`);
    results.push({ bakeoff: result, stored });
    // The client's first stage report (10 s after the worker started) falls
    // in the bake-off: its frames come from several paths.
    const stagesLine = await until(() => (hostProc.log.slice(log0).match(/msg="latency stages[^\n]*/) || [])[0], 12000, 'stage line').catch(() => '');
    check('renderer auto: the host logs a stage window that mixes paths as renderer=bakeoff', / renderer=bakeoff /.test(stagesLine),
      stagesLine.replace(/^.*?msg=/, '').slice(0, 200));
    await endStream();

    await startStream({ path: 'auto', transport: 'auto', renderer: 'auto', fps: 30 });
    await sleep(1500);
    const bw2 = await rateWindow(1500);
    const st2 = await page.evaluate(() => window.__recon.lastStats);
    const br2 = starvedRate(st2?.fps ?? 0, 30, 2 / 3, bw2);
    const canvases2 = await page.evaluate(() => document.querySelectorAll('#stage canvas').length);
    check('renderer auto: the next connection draws with the stored winner at once (no bake-off, one canvas)',
      !!stored && st2?.renderer?.name === stored.winner && st2.renderer.mode === 'auto' && !st2.renderer.bake && canvases2 === 1 && (st2.fps > 20 || br2.ok),
      `${st2?.renderer?.name} (mode ${st2?.renderer?.mode}, bake-off ${JSON.stringify(st2?.renderer?.bake)}), ${canvases2} canvas, ${st2?.fps?.toFixed(1)} fps${br2.note}`);
    await page.evaluate(() => [...document.querySelectorAll('#drawer button')].find((b) => b.textContent.includes('Measure renderers again')).click());
    const cleared = await page.evaluate((k) => localStorage.getItem(k), PRESENT_KEY);
    check('renderer auto: "Measure renderers again" clears the stored result', cleared === null);
    await endStream();

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
    const bw3 = await rateWindow(1500);
    const st3 = await page.evaluate(() => window.__recon.lastStats);
    const br3 = starvedRate(st3?.fps ?? 0, 30, 2 / 3, bw3);
    const after3 = await page.evaluate((k) => ({
      stored: localStorage.getItem(k), canvases: document.querySelectorAll('#stage canvas').length, present: window.__recon.present,
      log: window.__recon.logs.filter((l) => /render error|reconnecting with the 2D canvas/.test(l)).map((l) => l.replace(/^\S+ /, '')),
    }), PRESENT_KEY);
    check('renderer auto: a picked path that stops drawing (WebGL2 context lost) is forgotten and the client reconnects with the 2D canvas',
      !!gl && !!back && after3.stored === null && after3.canvases === 1 && after3.present?.mode === 'auto' && st3?.renderer?.name === 'canvas2d' && (st3.fps > 20 || br3.ok) &&
        after3.log.some((l) => l.includes('reconnecting with the 2D canvas')),
      `before: ${gl ? `webgl2 at ${gl.fps?.toFixed(1)} fps` : 'webgl2 not drawing'}; 2D after ${took2.toFixed(1)} s at ${st3?.fps?.toFixed(1)} fps, stored ${after3.stored}, ` +
        `${after3.canvases} canvas; log: ${after3.log.slice(0, 2).join(' | ')}${br3.note}`);
    await endStream();
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
  await sleep(1000);
  // One window (pacingWindow): the frames drawn and the chunks the decoder got
  // in it. The plain path draws what the decoder delivers: five sixths of it
  // at least (as before of the stream's nominal rate, which a starved 2-vCPU
  // runner's encoder and decoder do not reach), and the decoder delivers
  // two fifths of the stream's rate at least (the stream runs).
  const off = await pacingWindow(3000);
  const fps = [+off.fps.toFixed(1)];
  const decFps = off.decoded === null ? null : off.decoded / off.secs;
  // Where the CPUs had nothing to spare in the window (the emulated GPU's
  // passes and the encoder share two vCPUs on CI), the frames the pacer
  // superseded in bursts count with the drawn ones (none held back).
  const offStarved = off.idle != null && off.idle < STARVED_IDLE;
  const atRate = decFps !== null
    ? decFps >= 0.4 * rate && (off.recs.length >= (5 / 6) * off.decoded || (offStarved && off.superseded !== null && off.recs.length + off.superseded >= (5 / 6) * off.decoded))
    : off.fps >= (rate * 5) / 6;
  const st2 = await page.evaluate(() => window.__recon.lastStats);
  const u2 = st2?.renderer?.upscale;
  const overlay2 = await page.textContent('#stats').catch(() => '');
  const saved = await page.evaluate(() => JSON.parse(localStorage.getItem('recon.prefs.v1') || '{}').upscale);
  const want2 = `Off: bilinear · ${sc.size.join('×')} → ${canvas?.join('×')}`;
  check(`${name}: upscaling Off, applied live: the plain bilinear path draws again at the stream's rate, saved with the settings`,
    switched && u2?.mode === 'off' && !u2.active && u2.why === 'off' && overlay2.includes(want2) && atRate && saved === 'off',
    `${JSON.stringify({ mode: u2?.mode, active: u2?.active, why: u2?.why })}; overlay ${overlay2.includes(want2) ? `shows "${want2}"` : 'lacks it'}; ` +
      `${off.recs.length} frames drawn in ${off.secs.toFixed(1)} s (${off.fps.toFixed(1)} fps of ${rate}) of ${off.decoded ?? '?'} chunks decoded ` +
      `(${decFps?.toFixed(1) ?? '?'} fps; ${off.superseded ?? '?'} superseded); CPUs ${off.idle == null ? '?' : (100 * off.idle).toFixed(0)} % idle` +
      `${offStarved ? ' (no CPU to spare: superseded frames count)' : ''}; saved upscale ${saved}`);
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
  // Test hook for the HDR scenario: with localStorage e2e.hdrDisplay set, the
  // page sees an HDR display (matchMedia "(dynamic-range: high)"; Xvfb is SDR).
  await c.addInitScript(() => {
    if (localStorage.getItem('e2e.hdrDisplay') !== '1') return;
    const mm = window.matchMedia.bind(window);
    window.matchMedia = (q) => (/dynamic-range:\s*high/.test(q) ? { matches: true, media: q, onchange: null, addEventListener() {}, removeEventListener() {} } : mm(q));
  });
  const p = await c.newPage();
  p.on('console', onConsole.bind(p));
  p.on('pageerror', onPageError);
  p.on('response', onResponse);
  p.on('worker', instrumentWorker);
  headed = { browser: b, page: p, disp };
  return p;
}

// Closes the headed browser and its X server once no scenario needs them
// (after the bake-off), not at the very end: its page, GPU process (the
// emulated GPU: llvmpipe, SwiftShader WebGPU) and X server stayed up through
// the loss, bitrate and unit scenarios after it.
async function closeHeaded() {
  const h = headed;
  headed = null;
  if (!h) return;
  await h.browser.close().catch(() => {});
  stopXvfb(h.disp);
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
    xvfb.stdio[3].on('data', (d) => { b += d; if (b.includes('\n')) { xvfb.display = `:${b.trim()}`; res(xvfb.display); } });
    xvfb.on('exit', () => rej(new Error('Xvfb exited')));
    setTimeout(() => rej(new Error('Xvfb did not start')), 10000);
  });
}

// Stops the X server of display disp once its browser is closed.
function stopXvfb(disp) {
  const x = procs.find((p) => p.display === disp && p.exitCode === null);
  if (x) x.kill('SIGTERM');
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
  let disp = null;
  if (haveX) {
    disp = await startXvfb();
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
    if (disp) stopXvfb(disp);
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
// (960x540 into 1920x1080, Auto) at 600 sampled pixels. Then placement and
// sizes, each against the reference of its own rectangle (computed here, not
// taken from the renderer) with the bars around it black: bars left and right
// and top and bottom (RCAS's letterbox offset), an odd input into odd outputs
// (63x37 into 101x59 and 157x99), a frame with 16 white columns the video
// config crops on the right (external and copy input: the clamp of either),
// and a resize followed by a redraw of the same frame (the intermediate
// texture re-created). And the plan: FSR never draws a picture shown at its
// size or smaller, Auto only above 1.05x.
// n output pixels spread over a w x h picture (a fixed sequence).
function fsrSamples(w, h, n) {
  let s = 12345;
  const rnd = () => { s = (s * 1103515245 + 12345) % 2147483648; return s / 2147483648; };
  return Array.from({ length: n }, () => [Math.floor(rnd() * w), Math.floor(rnd() * h)]);
}

// pad white rows below, padRight white columns to the right (encoder padding).
function fsrPattern(w, h, pad = 0, padRight = 0) {
  const stride = w + padRight;
  const px = new Uint8ClampedArray(stride * (h + pad) * 4).fill(255);
  const set = (x, y, c) => { const o = (y * stride + x) * 4; px[o] = c[0]; px[o + 1] = c[1]; px[o + 2] = c[2]; };
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
  const PADR = 16;
  const ODD = [63, 37];
  const plain = fsrPattern(W, H);
  const odd = fsrPattern(ODD[0], ODD[1]);
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
    // Placement and sizes: rect is where the picture must go ([x, y, w, h],
    // the letterbox worked out by hand), frame the source (plain 64x40 if unset).
    { name: '2x pillarboxed', box: [200, 80], up: { mode: 'fsr' }, rect: [36, 0, 128, 80] },
    { name: '2x letterboxed external', box: [128, 120], up: { mode: 'fsr', input: 'external' }, rect: [0, 20, 128, 80] },
    { name: '63x37 -> 101x59', box: [101, 59], up: { mode: 'fsr' }, frame: 'odd', rect: [0, 0, 100, 59] },
    { name: '63x37 -> 157x99 external', box: [157, 99], up: { mode: 'fsr', input: 'external' }, frame: 'odd', rect: [0, 3, 157, 92] },
    { name: '2x right crop external', box: [128, 80], up: { mode: 'fsr', input: 'external' }, frame: 'right', rect: [0, 0, 128, 80],
      cfg: { width: W, height: H, codedWidth: W + PADR, codedHeight: H, cropRight: PADR } },
    { name: '2x right crop copy', box: [128, 80], up: { mode: 'fsr', input: 'copy' }, frame: 'right', rect: [0, 0, 128, 80],
      cfg: { width: W, height: H, codedWidth: W + PADR, codedHeight: H, cropRight: PADR } },
    { name: '2x resized to 200x90 and redrawn', box: [128, 80], up: { mode: 'fsr' }, resize: [200, 90], rect: [28, 0, 144, 90] },
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
    res = await p.evaluate(async ({ W, H, PAD, frames, cases }) => {
      const P = await import('/js/protocol.js');
      const R = await import('/js/renderers.js');
      const frameOf = (k) => {
        const f = frames[k.frame || (k.pad ? 'padded' : 'plain')];
        const c = new OffscreenCanvas(f.w, f.h);
        c.getContext('2d').putImageData(new ImageData(new Uint8ClampedArray(f.px), f.w, f.h), 0, 0);
        if (!k.big) return new VideoFrame(c, { timestamp: 0 });
        const t = new OffscreenCanvas(k.big[0], k.big[1]); // the pattern tiled
        const g = t.getContext('2d');
        for (let y = 0; y < k.big[1]; y += H) for (let x = 0; x < k.big[0]; x += W) g.drawImage(c, x, y);
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
          const frame = frameOf(k);
          const cfg = k.cfg || (k.pad ? { width: W, height: H, codedWidth: W, codedHeight: H + PAD, cropBottom: PAD } : null);
          r.draw(frame, null, P.visibleArea(cfg, frame.visibleRect.width, frame.visibleRect.height, frame.displayWidth, frame.displayHeight));
          await r.device.queue.onSubmittedWorkDone();
          if (k.resize) {
            // A new box (fullscreen, a window resize): the last frame drawn again.
            r.resize(k.resize[0], k.resize[1]);
            r.redraw();
            await r.device.queue.onSubmittedWorkDone();
          }
          const bmp = canvas.transferToImageBitmap();
          const g = new OffscreenCanvas(bmp.width, bmp.height).getContext('2d');
          g.drawImage(bmp, 0, 0);
          const data = k.plan === undefined ? g.getImageData(0, 0, bmp.width, bmp.height).data : null;
          out.push({ name: k.name, w: bmp.width, h: bmp.height, px: data && !k.sample ? Array.from(data) : null,
            sampled: data && k.sample ? k.sample.map(([x, y]) => Array.from(data.subarray((y * bmp.width + x) * 4, (y * bmp.width + x) * 4 + 3))) : null,
            ready, error: r.fsrError, upscaled: r.upscaled, info: r.upscaleInfo(), rect: r.rect && [r.rect.x, r.rect.y, r.rect.w, r.rect.h] });
          r.prev?.close();
          r.prev = null;
          r.destroy();
        } catch (e) {
          out.push({ name: k.name, error: e.message });
        }
      }
      return out;
    }, {
      W, H, PAD, cases, frames: {
        plain: { w: W, h: H, px: Array.from(plain) },
        padded: { w: W, h: H + PAD, px: Array.from(fsrPattern(W, H, PAD)) },
        right: { w: W + PADR, h: H, px: Array.from(fsrPattern(W, H, 0, PADR)) },
        odd: { w: ODD[0], h: ODD[1], px: Array.from(odd) },
      },
    });
  } finally {
    await ctx2.close();
    await b.close();
    stopXvfb(disp);
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
  const full = cases.filter((k) => k.up.mode === 'fsr' && k.stops === undefined && k.plan === undefined && !k.pad && !k.rect).map(compare);
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
  // Placement and sizes: the expected rectangle against the reference of the
  // visible source at that size, every pixel outside it black.
  const srcOf = (px, w, h) => {
    const a = new Float64Array(w * h * 3);
    for (let i = 0; i < w * h; i++) for (let c = 0; c < 3; c++) a[i * 3 + c] = px[i * 4 + c] / 255;
    return a;
  };
  const oddSrc = srcOf(odd, ODD[0], ODD[1]);
  const placed = (k) => {
    const r = by(k.name);
    if (!r?.px) return { name: k.name, error: r?.error || 'no result' };
    const [sw, sh] = k.frame === 'odd' ? ODD : [W, H];
    const [x0, y0, rw, rh] = k.rect;
    const ref = fsrReference(k.frame === 'odd' ? oddSrc : src, sw, sh, rw, rh, k.up.sharpness ?? 0.2, !!k.up.denoise);
    let max = 0;
    let sum = 0;
    let worst = '';
    let bars = 0;
    for (let y = 0; y < r.h; y++) {
      for (let x = 0; x < r.w; x++) {
        const o = (y * r.w + x) * 4;
        if (x < x0 || x >= x0 + rw || y < y0 || y >= y0 + rh) {
          if (r.px[o] || r.px[o + 1] || r.px[o + 2]) bars++;
          continue;
        }
        const v = ref.at(x - x0, y - y0);
        for (let c = 0; c < 3; c++) {
          const want = Math.round(Math.min(1, Math.max(0, v[c])) * 255);
          const d = Math.abs(r.px[o + c] - want);
          sum += d;
          if (d > max) { max = d; worst = `(${x},${y}) ${'rgb'[c]} ${r.px[o + c]} vs ${want}`; }
        }
      }
    }
    const box = k.resize || k.box;
    const ok = r.upscaled && r.w === box[0] && r.h === box[1] && r.rect?.join(',') === k.rect.join(',') && max <= 5 && bars === 0;
    return { name: k.name, ok, w: r.w, h: r.h, rect: r.rect, max, mean: sum / (rw * rh * 3), worst, bars, upscaled: r.upscaled, input: r.info?.input };
  };
  const geo = cases.filter((k) => k.rect).map(placed);
  check('FSR 1 shader (unit): letterboxed (bars left/right, top/bottom), odd sizes (63x37 -> 101x59, 157x99), a right crop (external and copy input) and a resize redrawn: ' +
    'the picture lands in its rectangle, matches the CPU reference there within 5 levels, the bars stay black',
    geo.every((x) => x.ok),
    geo.map((x) => (x.error ? `${x.name}: ${x.error}` : `${x.name}: ${x.upscaled ? 'FSR' : 'bilinear'} (${x.input}) canvas ${x.w}x${x.h}, rect ${x.rect?.join(',')}, ` +
      `max ${x.max}, mean ${x.mean.toFixed(3)}${x.max ? ` (worst ${x.worst})` : ''}, non-black bar pixels ${x.bars}`)).join('; '));
  const plans = cases.filter((k) => k.plan !== undefined).map((k) => ({ k, r: by(k.name) }));
  check('FSR 1 (unit): never upscales a picture shown at its size or smaller; Auto only above 1.05x, FSR above 1x; the renderer reports why',
    plans.every(({ k, r }) => r && !r.error && r.upscaled === k.plan && r.info.active === k.plan && (k.plan || !!r.info.why)),
    plans.map(({ k, r }) => `${k.name}: ${r?.error || `${r.upscaled ? 'FSR' : `bilinear (${r.info.why})`}, ${r.info.in?.join('x')} -> ${r.info.out?.join('x')}`}`).join('; '));
  results.push({ upscaleUnit: { srcDiff, easu, full, pad, geo, bigMax, metrics: Object.fromEntries(Object.entries(m).map(([k, v]) => [k, v && { width: v.width, step: v.step, flat: v.flat, flatMax: v.flatMax }])) } });
}

// HDR10 at unit level (step 4.5, web/static/js/hdr.js): the WebGPU renderer's
// HDR path (the decoded planes copied into textures, BT.2020 Y'CbCr -> PQ
// R'G'B', the PQ EOTF, BT.2020 -> the canvas's primaries, then extended range
// or BT.2390 tone mapping) against hdrReference, a CPU reference written from
// the standards (ITU-R BT.2020 / BT.2100, SMPTE ST 2084, BT.2390-10 5.4.1;
// the primaries matrices derived through XYZ by Gaussian elimination, not
// hdr.js's code). Read back from the canvas itself through the renderer's
// test hook (planes.capture: copyTextureToBuffer of the rgba16float or
// bgra8unorm canvas texture). Code values: black, 1, 100, 203 (reference
// white), 1000, 4000 and 10000 cd/m2 greys, a code above 940, and the BT.2020
// primaries at 1000 cd/m2 and red at 10000.
const HDR_D65 = [0.3127, 0.3290];
const HDR_PRIM = { bt2020: [[0.708, 0.292], [0.170, 0.797], [0.131, 0.046]], srgb: [[0.64, 0.33], [0.30, 0.60], [0.15, 0.06]], 'display-p3': [[0.68, 0.32], [0.265, 0.69], [0.15, 0.06]] };
function hdrSolve3(A, b) {
  const m = A.map((r, i) => [...r, b[i]]);
  for (let c = 0; c < 3; c++) {
    let p = c;
    for (let r = c + 1; r < 3; r++) if (Math.abs(m[r][c]) > Math.abs(m[p][c])) p = r;
    [m[c], m[p]] = [m[p], m[c]];
    for (let r = 0; r < 3; r++) {
      if (r === c) continue;
      const f = m[r][c] / m[c][c];
      for (let k = c; k < 4; k++) m[r][k] -= f * m[c][k];
    }
  }
  return m.map((r, i) => r[3] / r[i]);
}
function hdrToXYZ(p) {
  const xyz = ([x, y]) => [x / y, 1, (1 - x - y) / y];
  const P = [0, 1, 2].map((r) => p.map((c) => xyz(c)[r]));
  const S = hdrSolve3(P, xyz(HDR_D65));
  return P.map((r) => r.map((v, k) => v * S[k]));
}
const HDR_PQ = { m1: 2610 / 16384, m2: (2523 / 4096) * 128, c1: 3424 / 4096, c2: (2413 / 4096) * 32, c3: (2392 / 4096) * 32 };
const hdrEotf = (e) => { const { m1, m2, c1, c2, c3 } = HDR_PQ; const p = Math.max(e, 0) ** (1 / m2); return 10000 * (Math.max(p - c1, 0) / (c2 - c3 * p)) ** (1 / m1); };
const hdrOetf = (l) => { const { m1, m2, c1, c2, c3 } = HDR_PQ; const t = Math.min(Math.max(l / 10000, 0), 1) ** m1; return ((c1 + c2 * t) / (1 + c3 * t)) ** m2; };
// BT.2390-10 5.4.1 EETF with LB = Lmin = 0 (b = 0): source [0, Lw] into [0, Lmax].
function hdrEetf(L, Lw, Lmax) {
  const e1 = Math.min(hdrOetf(L) / hdrOetf(Lw), 1);
  const maxLum = hdrOetf(Lmax) / hdrOetf(Lw);
  const ks = 1.5 * maxLum - 0.5;
  let e2 = e1;
  if (ks < 1 && e1 >= ks) {
    const T = (e1 - ks) / (1 - ks);
    e2 = (2 * T ** 3 - 3 * T ** 2 + 1) * ks + (T ** 3 - 2 * T ** 2 + T) * (1 - ks) + (-2 * T ** 3 + 3 * T ** 2) * maxLum;
  }
  return hdrEotf(e2 * hdrOetf(Lw));
}
const hdrSrgb = (x) => { const a = Math.abs(x); return Math.sign(x) * (a <= 0.0031308 ? 12.92 * a : 1.055 * a ** (1 / 2.4) - 0.055); };
// The canvas value for 10-bit codes [Y, Cb, Cr] (fractional: interpolated chroma).
function hdrReference([Y, Cb, Cr], { mode, white, space, peak }) {
  const Kr = 0.2627;
  const Kb = 0.0593;
  const y = (Y - 64) / 876;
  const cb = (Cb - 512) / 896;
  const cr = (Cr - 512) / 896;
  const R = y + 2 * (1 - Kr) * cr;
  const B = y + 2 * (1 - Kb) * cb;
  const G = (y - Kr * R - Kb * B) / (1 - Kr - Kb);
  let lin = [R, G, B].map((v) => hdrEotf(Math.min(1, Math.max(0, v))));
  if (mode === 'tonemap') {
    const m = Math.max(...lin);
    if (m > 0) { const t = hdrEetf(m, peak, white) / m; lin = lin.map((v) => v * t); }
  }
  const to = hdrToXYZ(HDR_PRIM[mode === 'tonemap' ? 'srgb' : space]);
  const from = hdrToXYZ(HDR_PRIM.bt2020);
  let out = hdrSolve3(to, from.map((r) => r[0] * lin[0] + r[1] * lin[1] + r[2] * lin[2])).map((v) => v / white);
  if (mode === 'tonemap') out = out.map((v) => Math.min(1, Math.max(0, v)));
  return out.map(hdrSrgb);
}
// Within half-float precision (extended) or one 8-bit level (tone mapped).
const hdrClose = (got, want, mode) => got.every((g, k) => Math.abs(g - want[k]) <= (mode === 'tonemap' ? 1.5 / 255 : 0.002 + 0.002 * Math.abs(want[k])));
// 10-bit Y'CbCr codes of BT.2020 R'G'B' (PQ values).
const hdrCodes = (r, g, b) => {
  const y = 0.2627 * r + 0.678 * g + 0.0593 * b;
  return [64 + 876 * y, 512 + (896 * (b - y)) / 1.8814, 512 + (896 * (r - y)) / 1.4746].map(Math.round);
};

async function checkHdrUnit(haveX) {
  if (!haveX) {
    console.log('- HDR shader (unit): skipped, WebGPU needs a headed browser (Xvfb) here');
    return;
  }
  const P1000 = hdrOetf(1000);
  const grey = (n) => [Math.round(64 + 876 * hdrOetf(n)), 512, 512];
  const patches = [
    ['black', [64, 512, 512]], ['1 cd/m2', grey(1)], ['100 cd/m2', grey(100)], ['203 cd/m2 (reference white)', grey(203)],
    ['1000 cd/m2', grey(1000)], ['4000 cd/m2', grey(4000)], ['10000 cd/m2', [940, 512, 512]], ['above 940', [1000, 512, 512]],
    ['BT.2020 red 1000', hdrCodes(P1000, 0, 0)], ['BT.2020 green 1000', hdrCodes(0, P1000, 0)], ['BT.2020 blue 1000', hdrCodes(0, 0, P1000)],
    ['BT.2020 red 10000', hdrCodes(1, 0, 0)],
  ];
  const outputs = [
    { name: 'extended, sRGB canvas, white 203', o: { want: true, white: 203, space: 'srgb', peak: 1000 } },
    { name: 'extended, Display P3 canvas, white 100', o: { want: true, white: 100, space: 'display-p3', peak: 1000 } },
    { name: 'tone mapped, peak 1000', o: { want: false, white: 203, space: 'srgb', peak: 1000 } },
    { name: 'tone mapped, peak 10000', o: { want: false, white: 203, space: 'srgb', peak: 10000 } },
  ];
  const disp = await startXvfb();
  const b = await chromium.launch({ headless: false, env: { ...process.env, DISPLAY: disp }, args: ['--enable-unsafe-webgpu', '--ignore-gpu-blocklist'] });
  const ctx2 = await b.newContext({ ignoreHTTPSErrors: true });
  let res;
  try {
    const p = await ctx2.newPage();
    await p.goto(`${base}/login`);
    res = await p.evaluate(async ({ patches, outputs }) => {
      const R = await import('/js/renderers.js');
      const W = 64;
      const H = 48;
      const CS = { primaries: 'bt2020', transfer: 'pq', matrix: 'bt2020-ncl', fullRange: false };
      // A W x H frame of format fmt: 16x16 patches of codes [Y, Cb, Cr] (10-bit
      // units), 4 per row; or, with chroma, a frame whose Cr changes with every
      // chroma sample (the siting and interpolation check).
      const frameOf = (fmt, cells, chroma) => {
        const sub = fmt.startsWith('I444') ? 1 : 2;
        const cw = W / sub;
        const ch = H / sub;
        const bits = fmt === 'NV12' ? 8 : fmt.endsWith('P12') ? 12 : 10;
        const conv = (v) => (bits === 8 ? Math.round(v / 4) : bits === 12 ? v * 4 : v);
        const Y = new Float64Array(W * H);
        const U = new Float64Array(cw * ch);
        const V = new Float64Array(cw * ch);
        for (let y = 0; y < H; y++) for (let x = 0; x < W; x++) Y[y * W + x] = chroma ? 400 : cells[Math.floor(y / 16) * 4 + Math.floor(x / 16)][0];
        for (let y = 0; y < ch; y++) {
          for (let x = 0; x < cw; x++) {
            const c = chroma ? null : cells[Math.floor((y * sub) / 16) * 4 + Math.floor((x * sub) / 16)];
            U[y * cw + x] = chroma ? 512 : c[1];
            V[y * cw + x] = chroma ? 512 + 40 * ((x * 7 + y * 3) % 9) - 160 : c[2];
          }
        }
        const T = bits === 8 ? Uint8Array : Uint16Array;
        const planes = fmt === 'NV12' ? [Y, (() => { const uv = new Float64Array(cw * ch * 2); for (let i = 0; i < cw * ch; i++) { uv[2 * i] = U[i]; uv[2 * i + 1] = V[i]; } return uv; })()] : [Y, U, V];
        const arrays = planes.map((pl) => T.from(pl, conv));
        const size = arrays.reduce((s, a) => s + a.byteLength, 0);
        const data = new Uint8Array(size);
        let o = 0;
        for (const a of arrays) { data.set(new Uint8Array(a.buffer), o); o += a.byteLength; }
        return { frame: new VideoFrame(data, { format: fmt, codedWidth: W, codedHeight: H, timestamp: 0, colorSpace: CS }), V: Array.from(V), cw, sub };
      };
      const out = { outputs: [], formats: [], siting: null };
      const c = new OffscreenCanvas(W, H);
      const r = await R.WebGPURenderer.create(c, { log: (t) => console.log(t) });
      out.canvas = r.hdrCanvasOk;
      const vis = { w: W, h: H, fx: 1, fy: 1 };
      const cells = patches.map((x) => x[1]);
      const centres = cells.map((_, i) => [(i % 4) * 16 + 8, Math.floor(i / 4) * 16 + 8]);
      const draw = async (f, points, o) => {
        r.setHdr(o);
        const planes = await r.prepare(f);
        return new Promise((resolve) => { planes.capture = { points, resolve }; r.draw(f, null, vis, planes); });
      };
      for (const k of outputs) out.outputs.push({ name: k.name, ...(await draw(frameOf('I420P10', cells).frame, centres, k.o)) });
      for (const fmt of ['I420P12', 'I444P10', 'NV12']) {
        try {
          out.formats.push({ fmt, ...(await draw(frameOf(fmt, cells).frame, centres, outputs[0].o)) });
        } catch (e) {
          out.formats.push({ fmt, error: e.message });
        }
      }
      // Chroma siting: Cr per chroma sample; luma columns 2i (co-sited) and
      // 2i + 1 (between two samples), rows 2j and 2j + 1 (a quarter and three
      // quarters of the way from sample j - 1 / j).
      const s = frameOf('I420P10', null, true);
      const pts = [];
      for (const y of [9, 10, 20, 21]) for (const x of [10, 11, 30, 31]) pts.push([x, y]);
      out.siting = { pts, V: s.V, cw: s.cw, ...(await draw(s.frame, pts, outputs[0].o)) };
      // What importExternalTexture makes of the same PQ frame (Chrome's
      // conversion): the patches read from the external texture into an
      // rgba16float texture.
      try {
        const d = r.device;
        const f = frameOf('I420P10', cells).frame;
        const ext = d.importExternalTexture({ source: f });
        const code = `@group(0) @binding(0) var tex: texture_external;
@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  return vec4f(p[i], 0.0, 1.0);
}
@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  let i = u32(pos.x);
  return textureLoad(tex, vec2u((i % 4u) * 16u + 8u, (i / 4u) * 16u + 8u));
}`;
        const m = d.createShaderModule({ code });
        const pl = d.createRenderPipeline({ layout: 'auto', vertex: { module: m, entryPoint: 'vs' }, fragment: { module: m, entryPoint: 'fs', targets: [{ format: 'rgba16float' }] } });
        const t = d.createTexture({ size: [cells.length, 1], format: 'rgba16float', usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.COPY_SRC });
        const e = d.createCommandEncoder();
        const pass = e.beginRenderPass({ colorAttachments: [{ view: t.createView(), loadOp: 'clear', storeOp: 'store' }] });
        pass.setPipeline(pl);
        pass.setBindGroup(0, d.createBindGroup({ layout: pl.getBindGroupLayout(0), entries: [{ binding: 0, resource: ext }] }));
        pass.draw(3);
        pass.end();
        const buf = d.createBuffer({ size: 256, usage: GPUBufferUsage.COPY_DST | GPUBufferUsage.MAP_READ });
        e.copyTextureToBuffer({ texture: t }, { buffer: buf, bytesPerRow: 256 }, [cells.length, 1]);
        d.queue.submit([e.finish()]);
        await buf.mapAsync(GPUMapMode.READ);
        const h = new Uint16Array(buf.getMappedRange().slice(0, 8 * cells.length));
        const half = (v) => { const sg = v & 0x8000 ? -1 : 1; const ex = (v >> 10) & 31; const mt = v & 1023; return sg * (ex ? 2 ** (ex - 15) * (1 + mt / 1024) : 2 ** -14 * (mt / 1024)); };
        out.external = cells.map((_, i) => [0, 1, 2].map((k) => +half(h[i * 4 + k]).toFixed(4)));
        f.close();
      } catch (e) {
        out.external = e.message;
      }
      out.info = r.hdrInfo();
      r.destroy();
      return out;
    }, { patches, outputs });
  } finally {
    await ctx2.close();
    await b.close();
    stopXvfb(disp);
  }
  const mode = (o) => (o.want ? 'extended' : 'tonemap');
  const cmp = (got, k, codes, label) => {
    const rows = [];
    let ok = !got.error && got.values?.length === codes.length && got.format === (k.want ? 'rgba16float' : got.format) && got.mode === mode(k);
    codes.forEach((code, i) => {
      const want = hdrReference(code, { mode: mode(k), white: k.white, space: k.space, peak: k.peak });
      const v = got.values?.[i] || [NaN, NaN, NaN];
      const good = hdrClose(v, want, mode(k));
      ok &&= good;
      if (!good || i < 1) rows.push(`${label(i)}: ${v.map((x) => x.toFixed(4)).join(',')} vs ${want.map((x) => x.toFixed(4)).join(',')}`);
    });
    const worst = Math.max(...codes.map((code, i) => Math.max(...hdrReference(code, { mode: mode(k), white: k.white, space: k.space, peak: k.peak }).map((w, c) => Math.abs((got.values?.[i]?.[c] ?? NaN) - w)))));
    return { ok, worst, rows };
  };
  const outs = outputs.map((k, i) => ({ k, r: cmp(res.outputs[i], k.o, patches.map((x) => x[1]), (j) => patches[j][0]), got: res.outputs[i] }));
  check('HDR shader (unit): an rgba16float canvas with toneMapping "extended" is confirmed by getConfiguration() here (Chromium on SwiftShader)', !!res.canvas?.ok, JSON.stringify(res.canvas));
  check('HDR shader (unit): I420P10 planes -> BT.2020 PQ -> extended range (sRGB and Display P3 canvases, SDR white 203 / 100 cd/m2) match the CPU reference ' +
    '(black, 1/100/203/1000/4000/10000 cd/m2, a code above 940, BT.2020 primaries), within half-float precision',
  outs.slice(0, 2).every((x) => x.r.ok),
  outs.slice(0, 2).map((x) => `${x.k.name} (${x.got.format}): worst ${x.r.worst.toFixed(4)}${x.r.rows.length ? `; ${x.r.rows.join('; ')}` : ''}`).join(' | '));
  check('HDR shader (unit): tone mapped to SDR (BT.2390 EETF on max(R,G,B), peak 1000 and 10000 cd/m2 to the 203 cd/m2 white, bgra8unorm canvas) matches the CPU reference within 1.5 levels',
    outs.slice(2).every((x) => x.r.ok),
    outs.slice(2).map((x) => `${x.k.name} (${x.got.format}): worst ${(x.r.worst * 255).toFixed(2)} levels${x.r.rows.length ? `; ${x.r.rows.join('; ')}` : ''}`).join(' | '));
  const fmts = res.formats.map((g) => {
    const codes = patches.map((x) => (g.fmt === 'NV12' ? x[1].map((v) => Math.round(v / 4) * 4) : x[1]));
    return { fmt: g.fmt, r: g.error ? { ok: false, rows: [g.error] } : cmp(g, outputs[0].o, codes, (j) => patches[j][0]) };
  });
  check('HDR shader (unit): the other decoder output formats (I420P12, I444P10, NV12 as 8-bit codes x 4) give the reference too',
    fmts.every((x) => x.r.ok), fmts.map((x) => `${x.fmt}: ${x.r.ok ? `worst ${x.r.worst.toFixed(4)}` : x.r.rows.join('; ')}`).join(' | '));
  // Siting: Cr at luma (x, y) interpolates chroma (x / 2, (y - 0.5) / 2).
  const si = res.siting;
  const crAt = (x, y) => {
    const cx = x / 2;
    const cy = (y - 0.5) / 2;
    const x0 = Math.floor(cx);
    const y0 = Math.floor(cy);
    const fx = cx - x0;
    const fy = cy - y0;
    const v = (i, j) => si.V[Math.min(Math.max(j, 0), 23) * si.cw + Math.min(Math.max(i, 0), si.cw - 1)];
    return (v(x0, y0) * (1 - fx) + v(x0 + 1, y0) * fx) * (1 - fy) + (v(x0, y0 + 1) * (1 - fx) + v(x0 + 1, y0 + 1) * fx) * fy;
  };
  const sit = cmp(si, outputs[0].o, si.pts.map(([x, y]) => [400, 512, crAt(x, y)]), (j) => `(${si.pts[j]})`);
  check('HDR shader (unit): 4:2:0 chroma is interpolated bilinearly and sited as chroma_sample_loc_type 0 (co-sited columns, between rows)',
    sit.ok, `16 pixels: worst ${sit.worst.toFixed(4)}${sit.rows.length ? `; ${sit.rows.join('; ')}` : ''}`);
  // A record, not a pass/fail of this code: what Chrome's importExternalTexture
  // makes of a PQ frame (docs/VENDOR_NOTES.md 3.9/4.5).
  const ext = Array.isArray(res.external) ? res.external : null;
  // The greys say what happened to luminance (colours outside sRGB can read
  // below 0 or above 1 at any brightness: extended sRGB).
  const greys = patches.map((x, i) => i).filter((i) => !/BT\.2020/.test(patches[i][0]));
  const sdr = ext ? greys.every((i) => ext[i].every((x) => x <= 1.001)) : null;
  console.log(`  HDR (unit): importExternalTexture of the I420P10 BT.2020 PQ frame ${ext ? `reads ${patches.map((x, i) => `${x[0]} ${ext[i].join('/')}`).join('; ')}` : `failed: ${res.external}`}` +
    `${sdr === null ? '' : sdr ? ' -> every grey up to 10000 cd/m2 within [0, 1]: luminance tone-mapped into SDR by Chrome, HDR lost' : ' -> greys above 1: extended range kept'}`);
  results.push({ hdrUnit: { canvas: res.canvas, outputs: outs.map((x) => ({ name: x.k.name, format: x.got.format, worst: x.r.worst, values: x.got.values })),
    formats: fmts.map((x) => ({ fmt: x.fmt, ok: x.r.ok, worst: x.r.worst })), siting: { worst: sit.worst }, external: res.external, externalSDR: sdr } });
}

// HDR10 end to end in the stream (steps 3.9 / 4.5): the headed page plays an
// HDR display (the test hook e2e.hdrDisplay makes matchMedia("(dynamic-range:
// high)") match; Xvfb is SDR), Renderer WebGPU, codec AV1, HDR Auto; the
// host ("hdr": "auto") streams the test pattern as HDR10 (libsvtav1 10-bit,
// BT.2020 PQ, HDR metadata; media.HDRTestGraph) at HDR_STREAM. Checked: the
// video config and the host's choice, the decoded frames' format and colour
// space, extended-range presentation (or the tone-mapped fallback, recorded),
// the canvas pixels at the test pattern's patches against hdrReference of the
// codes the frame carried (and those codes against the host's), FSR off with
// its reason, the overlay; the scenario's common checks ran with it (real
// time, stage bookkeeping with the copy in the draw stage, crop, the barcode
// read from the copied planes). Then HDR Off from the drawer, live: the
// running HDR stream is tone-mapped at once (pixel check), and the host
// moves to an SDR generation with the reason. Then HDR Auto again with
// frames that cannot be copied (test hook): the client withdraws its offer
// and the host returns to SDR.
const HDR_STREAM = { w: 480, h: 270, fps: 15 };
// media.HDRTestPatches: x (32 px wide, the top 48 rows), name, 10-bit codes.
const HDR_PATCHES = [[160, 'black', [64, 512, 512]], [192, '100 cd/m2', [508, 512, 512]], [224, '200 cd/m2', [572, 512, 512]], [256, '1000 cd/m2', [724, 512, 512]],
  [288, '4000 cd/m2', [856, 512, 512]], [320, '10000 cd/m2', [940, 512, 512]], [352, 'red', [260, 396, 848]], [384, 'green', [452, 288, 228]], [416, 'blue', [140, 848, 456]]];

async function hdrCheckOnce() {
  await page.evaluate((points) => { window.__recon.hdrCheck = null; window.__recon.worker.postMessage({ type: 'hdrCheck', points }); }, HDR_PATCHES.map(([x]) => [x + 16, 24]));
  return until(() => page.evaluate(() => window.__recon.hdrCheck), 8000, 'HDR pixel check');
}

function hdrPixels(res) {
  const rows = [];
  let ok = !!res && !res.error && res.values?.length === HDR_PATCHES.length;
  let worst = 0;
  let codeOff = 0;
  HDR_PATCHES.forEach(([, name, want], i) => {
    const code = res?.codes?.[i];
    if (!code) { ok = false; return; }
    codeOff = Math.max(codeOff, ...code.map((v, k) => Math.abs(v - want[k])));
    const ref = hdrReference(code, { mode: res.mode, white: res.white, space: res.space, peak: res.peak });
    const v = res.values[i];
    worst = Math.max(worst, ...v.map((g, k) => Math.abs(g - ref[k])));
    if (!hdrClose(v, ref, res.mode)) { ok = false; rows.push(`${name} codes ${code.join('/')}: ${v.map((x) => x.toFixed(4)).join(',')} vs ${ref.map((x) => x.toFixed(4)).join(',')}`); }
  });
  return { ok: ok && codeOff <= 8, worst, codeOff, rows };
}

async function checkHdrStream(sc) {
  const name = sc.name;
  const cfg = await page.evaluate(() => window.__recon.videoCfg);
  const st = await page.evaluate(() => window.__recon.lastStats);
  const h = st?.renderer?.hdr;
  const hostProc = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null) || procs.find((p) => p.spawnargs.includes('run'));
  const choice = (hostProc.log.match(/msg="hdr choice"[^\n]* hdr=true [^\n]*/g) || []).pop() || '';
  check(`${name}: the host streams HDR10 to a client that offers it: 10-bit AV1 (libsvtav1), BT.2020 PQ limited range, HDR metadata; its choice logged`,
    !!cfg?.hdr && cfg.bitDepth === 10 && /^av01\.0\.\d\dM\.10$/.test(cfg.codec) && cfg.encoder === 'libsvtav1' && !cfg.hdrNote &&
      JSON.stringify(cfg.colorSpace) === '{"primaries":"bt2020","transfer":"pq","matrix":"bt2020-ncl","fullRange":false}' &&
      cfg.hdrMetadata?.maxCll === 10000 && cfg.hdrMetadata.maxLuminance === 10000 && st?.hdr?.display && st.hdr.canvas && st.hdr.decoders.includes('av1') && !!choice,
    `config hdr ${cfg?.hdr} bitDepth ${cfg?.bitDepth} codec ${cfg?.codec} via ${cfg?.encoder}, ${JSON.stringify(cfg?.colorSpace)}, metadata ${JSON.stringify(cfg?.hdrMetadata)}; ` +
      `client offers ${JSON.stringify({ mode: st?.hdr?.mode, display: st?.hdr?.display, canvas: st?.hdr?.canvas, decoders: st?.hdr?.decoders })}; host: ${choice.replace(/^.*?msg=/, '').slice(0, 200)}`);
  const f = h?.frame;
  check(`${name}: the decoder outputs 10-bit frames with the HDR10 colour space (VideoFrame.format, colorSpace)`,
    f?.format === 'I420P10' && f.bits === 10 && f.colorSpace?.primaries === 'bt2020' && f.colorSpace.transfer === 'pq' && f.colorSpace.matrix === 'bt2020-ncl' && f.colorSpace.fullRange === false,
    `format ${f?.format} (${f?.bits}-bit) ${f?.w}x${f?.h}, colorSpace ${JSON.stringify(f?.colorSpace)}`);
  const overlay = await page.textContent('#stats').catch(() => '');
  const path = h?.path;
  check(`${name}: presented with extended range (rgba16float, toneMapping extended; or the tone-mapped fallback with its reason), shown in the overlay with the colour, metadata and copy cost`,
    (path === 'extended' || (path === 'tonemap' && !!h.why)) && h.canvasConfig?.startsWith(path === 'extended' ? 'extended:srgb' : 'sdr') && overlay.includes('HDR10 · ') &&
      overlay.includes('bt2020/pq/bt2020-ncl/limited · 10-bit') && overlay.includes('MaxCLL 10000') && overlay.includes('copy (copyTo + upload)') && h.copy?.n > 10,
    `path ${path}${path === 'tonemap' ? ` (${h.why})` : ''}, canvas ${h?.canvasConfig}, ${h?.drawn} HDR frames drawn; copy p50 ${h?.copy?.p50} p95 ${h?.copy?.p95} ms (n ${h?.copy?.n}), ` +
      `${(h?.bytes / 1e6).toFixed(2)} MB/frame; overlay ${overlay.includes('HDR10 · ') ? 'shows HDR10' : 'lacks HDR10'}`);
  const u = st?.renderer?.upscale;
  check(`${name}: FSR stays off for HDR frames, with the reason (the picture is shown 2x: Auto would upscale an SDR stream)`,
    !!u && !u.active && /off for HDR streams/.test(u.why || ''), JSON.stringify({ mode: u?.mode, active: u?.active, in: u?.in, out: u?.out, why: u?.why }));
  const px = hdrPixels(await hdrCheckOnce().catch((e) => ({ error: e.message })));
  check(`${name}: canvas pixels at the test pattern's patches (greys 0-10000 cd/m2, colours) = the CPU reference of the codes the frame carried; those codes = the host's (within 8)`,
    px.ok, `worst ${px.worst.toFixed(4)}, codes off by at most ${px.codeOff}${px.rows.length ? `; ${px.rows.join('; ')}` : ''}`);
  results.push({ hdrStream: name, cfg, hdr: h, pixels: { worst: px.worst, codeOff: px.codeOff } });
  console.log(`  ${name}: HDR path ${path}; plane copy (copyTo + writeTexture, CPU) p50 ${h?.copy?.p50} ms, p95 ${h?.copy?.p95} ms, ${(h?.bytes / 1e6).toFixed(2)} MB/frame`);

  // HDR Off from the drawer: tone-mapped at once, then an SDR generation.
  const gen = cfg.gen;
  await page.evaluate(() => {
    const sel = [...document.querySelectorAll('#drawer label')].find((l) => l.textContent === 'HDR')?.parentElement.querySelector('select');
    sel.value = 'off';
    sel.dispatchEvent(new Event('change'));
  });
  const tone = await hdrCheckOnce().catch((e) => ({ error: e.message }));
  const tpx = hdrPixels(tone);
  check(`${name}: HDR Off with the HDR stream running: tone-mapped to SDR at once (BT.2390, bgra8unorm canvas), = the CPU reference`,
    tone?.mode === 'tonemap' && tone.format !== 'rgba16float' && tone.peak === 10000 && tpx.ok,
    `mode ${tone?.mode} on ${tone?.format}, peak ${tone?.peak}, white ${tone?.white}; worst ${(tpx.worst * 255).toFixed(2)} levels${tpx.rows.length ? `; ${tpx.rows.join('; ')}` : ''}${tone?.error ? `; ${tone.error}` : ''}`);
  const sdr = await until(() => page.evaluate((g) => { const c = window.__recon.videoCfg; return c && c.gen !== g && !c.hdr ? c : null; }, gen), 10000, 'an SDR generation').catch(() => null);
  await sleep(1500);
  const st2 = await page.evaluate(() => window.__recon.lastStats);
  const overlay2 = await page.textContent('#stats').catch(() => '');
  check(`${name}: the host then streams SDR (a new generation: 8-bit, no colour fields, hdrNote why), the canvas back to SDR, the overlay says why`,
    !!sdr && /\.08$/.test(sdr.codec) && !sdr.bitDepth && !sdr.colorSpace && sdr.hdrNote === "HDR is off in the client's settings" && st2?.renderer?.hdr?.path === null &&
      st2.renderer.hdr.canvasConfig.startsWith('sdr') && overlay2.includes("off · HDR is off in the client's settings") && st2.fps > HDR_STREAM.fps * 0.6,
    `gen ${gen} -> ${sdr?.gen} codec ${sdr?.codec} hdrNote "${sdr?.hdrNote}"; renderer path ${st2?.renderer?.hdr?.path}, canvas ${st2?.renderer?.hdr?.canvasConfig}; ${st2?.fps?.toFixed(1)} fps`);

  // 10-bit frames without a plane path: Chrome's hardware decoders output
  // P010, whose VideoFrame.format is null (copyTo cannot read it); the
  // worker's test hook hdrOpaque plays such a decoder. HDR Auto again (an
  // HDR-only settings change that changes the decision: a restart) brings an
  // HDR10 generation, its first frame withdraws the AV1 offer, and the host
  // returns to SDR (a second restart) with the reason.
  const log0 = hostProc.log.length;
  const con1 = consoleLines.length;
  const gen2 = sdr?.gen ?? gen;
  await page.evaluate(() => {
    window.__recon.worker.postMessage({ type: 'hdrOpaque' });
    const sel = [...document.querySelectorAll('#drawer label')].find((l) => l.textContent === 'HDR')?.parentElement.querySelector('select');
    sel.value = 'auto';
    sel.dispatchEvent(new Event('change'));
  });
  const back = await until(() => page.evaluate((g) => { const c = window.__recon.videoCfg; return c && c.gen !== g && !c.hdr && /no 10-bit av1 decoder/.test(c.hdrNote || '') ? c : null; }, gen2),
    20000, 'SDR after the withdrawn offer').catch(() => null);
  await sleep(1500);
  const hostLog = hostProc.log.slice(log0);
  const hdrAgain = /msg="hdr choice"[^\n]* hdr=true /.test(hostLog);
  const restarts = (hostLog.match(/msg="restarting video"[^\n]* reason="HDR settings"/g) || []).length;
  const withdrawnLog = consoleLines.slice(con1).find((l) => l.includes('HDR: withdrawn for av1 streams')) || '';
  const st3 = await page.evaluate(() => window.__recon.lastStats);
  const overlay3 = await page.textContent('#stats').catch(() => '');
  check(`${name}: 10-bit frames that cannot be copied (VideoFrame.format null, as from Chrome's hardware decoders; test hook): HDR Auto again streams HDR10, its first frame withdraws the AV1 offer, the host returns to SDR with the reason (two HDR-only restarts), the overlay says why`,
    hdrAgain && !!back && restarts === 2 && /VideoFrame\.format null/.test(withdrawnLog) && !st3?.hdr?.decoders?.includes('av1') &&
      /VideoFrame\.format null/.test(st3?.hdr?.withdrawn?.av1 || '') && overlay3.includes('off · the browser has no 10-bit av1 decoder (av1: VideoFrame.format null') && st3.fps > 0,
    `host: HDR10 again ${hdrAgain}, ${restarts} restarts for HDR settings; gen ${gen2} -> ${back?.gen} hdrNote "${back?.hdrNote}"; client: ${withdrawnLog.replace(/^.*?HDR: /, '').slice(0, 160)}; ` +
      `offers ${JSON.stringify(st3?.hdr?.decoders)}; overlay ${(overlay3.match(/HDR\s*off · [^\n]*/) || [''])[0].slice(0, 160)}; ${st3?.fps?.toFixed(1)} fps`);
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
  const s0 = await page.evaluate(() => ({ pacing: window.__recon.lastStats?.pacing, superseded: window.__recon.lastStats?.superseded, probe: window.__recon.probe }));
  const c0 = cpuTimes();
  const t0 = await w.evaluate(() => {
    const raf = self.__raf || self.requestAnimationFrame;
    const ts = [];
    self.__refresh = { on: true, ts };
    const loop = (t) => { ts.push(t); if (self.__refresh.ts === ts && self.__refresh.on) raf.call(self, loop); };
    raf.call(self, loop);
    self.__refresh.decodes0 = self.__decoderCalls?.decodes ?? null; // watchDecoder's count of decode() calls
    return performance.now();
  });
  await sleep(ms);
  const [t1, rts, decoded] = await w.evaluate(() => {
    self.__refresh.on = false;
    const d0 = self.__refresh.decodes0;
    return [performance.now(), self.__refresh.ts, d0 === null || !self.__decoderCalls ? null : self.__decoderCalls.decodes - d0];
  });
  const idle = idleShare(c0, cpuTimes());
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
  // Watchdog draws (pacing.js: no refresh within max(100 ms, 3 refreshes) of
  // a decoded frame): each is justified only when the worker's refresh (its
  // own requestAnimationFrame, the test's loop above) really stalled from the
  // frame's output to the timer, which then drew it; the longest interval
  // between the worker's refreshes (the window's edges included).
  const timerRecs = recs.filter((r) => r.via === 'timer');
  const timerUnjustified = timerRecs.filter((r) => rts.some((f) => f > r.output + 1 && f < r.tick - 1)).length;
  const edges = [t0, ...rts, t1];
  const longestStall = Math.max(0, ...edges.slice(1).map((v, i) => v - edges[i]));
  // Bookkeeping per frame: hold and draw from the frame's own marks, the stages up to draw add up to end-to-end.
  const marks = recs.every((r) => Math.abs(r.stages[H] - (r.drawStart - r.output)) < 0.01 && Math.abs(r.stages[D] - (r.drawn - r.drawStart)) < 0.01);
  let sumDiff = 0;
  for (const r of recs) {
    let sum = 0;
    for (let i = r.fromCapture ? 0 : 2; i < DISPLAY; i++) sum += r.stages[i];
    sumDiff += Math.abs(sum - (r.drawn - ((r.fromCapture ? r.captureUs : r.sendUs) / 1000 - r.offset)));
  }
  const probe = { sampled: (s1.probe?.sampled ?? 0) - (s0.probe?.sampled ?? 0), matched: (s1.probe?.valid ?? 0) - (s1.probe?.mismatched ?? 0) - (s0.probe?.valid ?? 0) + (s0.probe?.mismatched ?? 0), from: s0.probe };
  return {
    recs, d, st: s1.st, refresh, tickHz, vsync, fps: (1000 * recs.length) / (t1 - t0), decoded, minGap: gaps.length ? Math.min(...gaps) : null,
    superseded: typeof s0.superseded === 'number' && typeof s1.st?.superseded === 'number' ? s1.st.superseded - s0.superseded : null,
    timerDraws: timerRecs.length, timerUnjustified, longestStall, secs: (t1 - t0) / 1000, idle,
    hold: { p50: hold(0.5), p95: hold(0.95), p99: hold(0.99) }, waitMax: wait.length ? +Math.max(...wait).toFixed(2) : null, lateRecs, missed, marks,
    sumDiff: recs.length ? sumDiff / recs.length : Infinity, probe, via: [...new Set(recs.map((r) => r.via))], modes: [...new Set(recs.map((r) => r.pacing))],
  };
}

async function checkPacing(name, rate, fallbacks) {
  await page.evaluate(() => { window.__recon.worker.__pacingTag = 1; window.__recon.conn.__pacingTag = 1; });
  const sameSession = () => page.evaluate(() => window.__recon.worker?.__pacingTag === 1 && window.__recon.conn?.__pacingTag === 1 && window.__recon.streaming);
  const row = (x) => `${x.recs.length} frames drawn (${x.fps.toFixed(1)} fps of ${rate}${x.decoded === null ? '' : `; ${x.decoded} chunks decoded meanwhile`}` +
    `${x.superseded === null ? '' : `; ${x.superseded} superseded from the window's start to the stats after it`}; ` +
    `the worker's display refresh meanwhile ${x.tickHz.toFixed(1)} Hz, vsync ` +
    `${x.vsync?.toFixed(2)} ms) via ${x.via.join('/') || '—'}; counters +hop ${x.d.hop} +raf ${x.d.raf} +main ${x.d.main} ` +
    `+timer ${x.d.timer}, stale +${x.d.stale}, late +${x.d.late}; refresh starts at least ${x.minGap?.toFixed(2) ?? 'n/a (no draw from a refresh tick)'} ms apart (pacer's refresh ${x.refresh} ms), ` +
    `${x.missed} drawn after a later refresh than the first after their output; ` +
    `hold p50/p95/p99 ${x.hold.p50}/${x.hold.p95}/${x.hold.p99} ms, refresh start - output at most ${x.waitMax ?? 'n/a'} ms (${x.lateRecs} over 1.25 refresh); ` +
    `marks ${x.marks}, stages vs end-to-end ${x.sumDiff.toFixed(3)} ms; ` +
    `barcode ${x.probe.matched}/${x.probe.sampled} = seq; CPUs ${x.idle == null ? '?' : (100 * x.idle).toFixed(0)} % idle`;
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
  if (sm.probe.sampled < 1) {
    // The probe reads 1 frame in 30 back: below 8 fps drawn (a starved
    // machine) the window may hold none. Wait for one, still in Smooth.
    const p0 = sm.probe.from;
    const p1 = await until(() => page.evaluate((n) => (window.__recon.probe?.sampled > n ? window.__recon.probe : null), p0?.sampled ?? 0), 15000, 'a probe sample in Smooth').catch(() => null);
    if (p1) sm.probe = { sampled: p1.sampled - (p0?.sampled ?? 0), matched: p1.valid - p1.mismatched - (p0?.valid ?? 0) + (p0?.mismatched ?? 0), from: p0 };
  }
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
    // Its ticks are used again: the draws come from requestAnimationFrame,
    // and a watchdog draw only where the worker's refresh stalled from the
    // frame's output to the timer (a starved worker on a 2-vCPU runner: no
    // refresh for 100 ms and more), never while refreshes came.
    const rafDraws = back.recs.filter((r) => r.via === 'raf').length;
    check(`${name}: frame pacing Smooth: requestAnimationFrame restored, its ticks are used again`,
      (await sameSession()) && smooth(back, 'raf') && rafDraws >= 0.75 * back.recs.length && back.timerUnjustified === 0,
      `${row(back)}; ${rafDraws} drawn from requestAnimationFrame, ${back.timerDraws} from the watchdog's timer ` +
        `(${back.timerUnjustified} while the worker's refresh ran; its longest stall ${back.longestStall.toFixed(0)} ms)`);
    results.push({ pacing: name, fallbacks: { main: { ...mt, recs: mt.recs.length }, watchdog: { ...wd, recs: wd.recs.length, log: wdLog }, restored: { ...back, recs: back.recs.length } } });
  }

  await setPacing('latency');
  const lt = await pacingWindow(2500);
  // Three quarters of the chunks the decoder got in the same window drawn:
  // relative to what reached the decoder, so a machine that streams below
  // the frame rate (a 2-vCPU runner encoding and decoding in software) does
  // not fail it, while a pacer that holds frames back for refresh ticks
  // does. Not all of them: outputs that queue behind the draw's task (a
  // burst after a stall, a decoder releasing frames together) supersede it
  // by design (pacing.js; the row's superseded count), and a loaded machine
  // has such bursts (a local run at load 5-10 on 4 CPUs: 132 of 153 drawn, at
  // 52 fps). Without the decoder count (not instrumented), three quarters of
  // the stream's rate as before.
  // Where the CPUs had nothing to spare in the window, bursts are the rule:
  // then every chunk decoded must be drawn or superseded (none held back),
  // the superseded counted from the window's start to the stats after it.
  const ltStarved = lt.idle != null && lt.idle < STARVED_IDLE;
  const drawnAll = lt.decoded !== null
    ? lt.recs.length >= 0.75 * lt.decoded || (ltStarved && lt.superseded !== null && lt.recs.length + lt.superseded >= 0.9 * lt.decoded - 3)
    : lt.fps >= 0.75 * rate;
  check(`${name}: frame pacing back to Lowest latency, applied live: drawn on decode again (one task, hold p50 < 2 ms)`,
    (await sameSession()) && lt.st?.pacing?.mode === 'latency' && lt.recs.length >= rate && lt.via.length === 1 && lt.via[0] === 'hop' && lt.modes[0] === 'latency' &&
      lt.d.raf === 0 && lt.d.main === 0 && lt.d.timer === 0 && lt.d.hop >= lt.recs.length && lt.hold.p50 < 2 && lt.marks && lt.sumDiff <= 2 && drawnAll, row(lt));
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
  const line = await until(() => (host.log.match(/msg="latency stages[^\n]*/) || [])[0], 32000, 'stage line').catch(() => ''); // (a report every 10 s)
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
  await endStream();
}

// Takeover (final review): a session that another connection replaces tells
// its client with a "bye" on the control stream, which keeps the client from
// reconnecting (two devices would keep taking the session from each other).
// Closing the connection resets the session's streams, so the host waits for
// the client to end the session on the bye (byeGrace at most) before it
// closes; before, the reset overtook the bye, and the replaced client
// reconnected. Page A streams, a second browser context with the same login
// starts a stream (page B): A ends with the host's reason and schedules no
// reconnect (a retry would come after 800 ms), B keeps the stream.
async function checkTakeover() {
  const host = await restartHost({}, 'host-takeover');
  await startStream({ path: 'direct', transport: 'auto' });
  const ctx2 = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 720 }, storageState: await ctx.storageState() });
  try {
    const p = await ctx2.newPage();
    p.on('console', onConsole.bind(p));
    p.on('pageerror', onPageError);
    await p.goto(`${base}/`);
    await p.evaluate((pr) => localStorage.setItem('recon.prefs.v1', JSON.stringify(pr)), { ...PREFS_2D, path: 'direct', transport: 'auto' });
    await p.click('.host.online a.btn-primary');
    await p.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
    await p.click('#btn-start');
    await p.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
    await until(() => page.evaluate(() => !window.__recon.streaming && !window.__recon.worker), 10000, 'the first client closed').catch(() => {});
    await sleep(3000);
    const a = await page.evaluate(() => ({
      streaming: window.__recon.streaming, worker: !!window.__recon.worker, attempts: window.__recon.attempts,
      splash: `${document.getElementById('splash-title')?.textContent} / ${document.getElementById('splash-sub')?.textContent}`,
    }));
    const b = await p.evaluate(() => ({ streaming: window.__recon.streaming }));
    const replaced = (host.log.match(/session replaced by a new connection/g) || []).length;
    check('takeover: the replaced client gets the bye, stops and does not reconnect; the new client keeps the stream',
      !a.streaming && !a.worker && a.attempts === 0 && /Disconnected/.test(a.splash) && /Another device connected/.test(a.splash) &&
        b.streaming && replaced === 1,
      `replaced client: ${JSON.stringify(a)}; new client streaming ${b.streaming}; host: ${replaced} takeover(s)`);
    await endStream(p);
  } finally {
    await ctx2.close();
  }
  await endStream();
}

// Input (step 4.6), on a host with the test hook rumble-echo (it plays a
// gamepad's triggers back as force feedback, as a game's rumble comes back
// through ViGEmBus, which this host lacks).
// Rumble: a fake gamepad (navigator.getGamepads and gamepadconnected
// replaced in the page) with the left trigger full and the right at a
// quarter: the host sends DgRumble 255/64 at once and every 100 ms while it
// runs; the client plays each with vibrationActuator.playEffect("dual-rumble",
// strong 1, weak 64/255, 250 ms) (the median gap between them about 100 ms),
// and the release (0/0, sent three times) resets the actuator, with no effect
// played after it.
// Keyboard Lock on a stubbed requestFullscreen without navigator.keyboard
// (Safari 26.4; the stub enters Chromium's real fullscreen without the
// option): the option keyboardLock "browser" goes with navigationUI "hide"
// and the client reports the lock; a browser that refuses the value
// (TypeError) gets fullscreen without it; one that ignores the option (does
// not read it) has no lock; with navigator.keyboard.lock (Chromium) the
// option is not passed. Pointer Lock on a stubbed requestPointerLock (game
// mode): the client reports unadjustedMovement only when the browser read the
// option and granted the lock; one that ignores it (Firefox, Safari) or
// refuses it (NotSupportedError, then a plain lock) does not.
async function checkInputHost() {
  await restartHost({ RECON_TEST_FAULTS: 'rumble-echo' }, 'host-rumble');
  await startStream({ path: 'auto', transport: 'auto' });
  await page.evaluate(() => {
    const calls = [];
    const resets = [];
    const pad = {
      index: 0, id: 'E2E fake pad', connected: true, mapping: 'standard', timestamp: 0,
      buttons: Array.from({ length: 17 }, () => ({ pressed: false, value: 0 })), axes: [0, 0, 0, 0],
      vibrationActuator: {
        effects: ['dual-rumble', 'trigger-rumble'],
        playEffect(type, p) { calls.push({ type, ...p, t: performance.now() }); return Promise.resolve('complete'); },
        reset() { resets.push(performance.now()); return Promise.resolve('complete'); },
      },
    };
    window.__pad = { pad, calls, resets, present: true };
    navigator.getGamepads = () => (window.__pad.present ? [pad, null, null, null] : [null, null, null, null]);
    const ev = new Event('gamepadconnected');
    Object.defineProperty(ev, 'gamepad', { value: pad });
    window.dispatchEvent(ev);
  });
  const triggers = (lt, rt) => page.evaluate(([l, r]) => {
    const b = window.__pad.pad.buttons;
    b[6] = { pressed: l > 0.5, value: l };
    b[7] = { pressed: r > 0.5, value: r };
  }, [lt, rt]);
  await sleep(300);
  await triggers(1, 0.25);
  // Running: wait for six effects (half a second of repeats; longer on a loaded machine).
  await until(() => page.evaluate(() => window.__pad.calls.length >= 6), 8000, 'rumble effects').catch(() => {});
  const running = await page.evaluate(() => window.__pad.calls.length);
  await triggers(0, 0);
  await until(() => page.evaluate(() => window.__pad.resets.length >= 1), 8000, 'rumble reset').catch(() => {});
  await sleep(600); // the stop's repeats
  const r = await page.evaluate(() => ({ calls: window.__pad.calls, resets: window.__pad.resets, rumbles: window.__recon.rumbles }));
  await page.evaluate(() => { window.__pad.present = false; });
  const want = (c) => c.type === 'dual-rumble' && c.strongMagnitude === 1 && Math.abs(c.weakMagnitude - 64 / 255) < 1e-9 && c.duration === 250;
  const on = r.calls.filter(want);
  const gaps = on.slice(1).map((c, i) => c.t - on[i].t);
  const median = [...gaps].sort((a, b) => a - b)[gaps.length >> 1] ?? NaN;
  const firstReset = r.resets[0] ?? Infinity;
  const late = r.calls.filter((c) => c.t > firstReset).length;
  check('gamepad rumble (fake gamepad, host test hook rumble-echo): DgRumble played with vibrationActuator.playEffect("dual-rumble") and repeated about every 100 ms while it runs; the stop resets the actuator',
    on.length >= 6 && on.length === r.calls.length && median >= 50 && median <= 150 && r.resets.length >= 1 && late === 0,
    `${on.length} effects (strong 1, weak 64/255, 250 ms) of ${r.calls.length} played, ${running} before the release, gaps median ` +
      `${median.toFixed(0)} ms (${gaps.length ? `${Math.min(...gaps).toFixed(0)}-${Math.max(...gaps).toFixed(0)}` : '-'}); ${r.resets.length} resets after the release, ` +
      `${late} effects after the first; ${r.rumbles} rumble datagrams played`);
  results.push({ rumble: { effects: on.length, calls: r.calls.length, resets: r.resets.length, gaps } });

  // Keyboard Lock: Safari's fullscreen option, on a stubbed requestFullscreen.
  await page.evaluate(() => {
    window.__fsOrig = Element.prototype.requestFullscreen;
    Object.defineProperty(navigator, 'keyboard', { value: undefined, configurable: true });
  });
  const fsCase = async (kind) => {
    await page.evaluate((k) => {
      window.__recon.keyboardLock = null;
      window.__fs = [];
      Element.prototype.requestFullscreen = function (o) {
        // An old browser does not read members it does not know.
        const lock = k === 'ignores' || !('keyboardLock' in o) ? null : o.keyboardLock;
        window.__fs.push({ el: this.id, navigationUI: o.navigationUI, lock });
        if (k === 'refuses' && lock) return Promise.reject(new TypeError(`The provided value '${lock}' is not a valid enum value of type FullscreenKeyboardLock.`));
        return window.__fsOrig.call(this, { navigationUI: o.navigationUI }); // real fullscreen
      };
    }, kind);
    await page.keyboard.press('Control+Alt+Shift+KeyF');
    await until(() => page.evaluate(() => !!document.fullscreenElement), 5000, 'fullscreen').catch(() => {});
    await sleep(300);
    const res = await page.evaluate(() => ({ calls: window.__fs, lock: window.__recon.keyboardLock, fs: document.fullscreenElement?.id ?? null,
      log: window.__recon.logs.filter((l) => l.includes('keyboard lock')).pop() || '' }));
    await page.evaluate(() => document.fullscreenElement && document.exitFullscreen().catch(() => {}));
    await until(() => page.evaluate(() => !document.fullscreenElement), 5000, 'leaving fullscreen').catch(() => {});
    return res;
  };
  const safari = await fsCase('safari');
  const refuses = await fsCase('refuses');
  const ignores = await fsCase('ignores');
  await page.evaluate(() => { delete navigator.keyboard; });
  const chromium = await fsCase('chromium');
  await page.evaluate(() => { Element.prototype.requestFullscreen = window.__fsOrig; });
  const one = (x, lock) => x.fs === 'player' && x.calls.length === 1 && x.calls[0].el === 'player' && x.calls[0].navigationUI === 'hide' && x.calls[0].lock === lock;
  check('Keyboard Lock without navigator.keyboard (Safari 26.4): requestFullscreen({keyboardLock: "browser"}); refused value: fullscreen without it; ignored option: no lock; Chromium: no option',
    one(safari, 'browser') && safari.lock === 'fullscreen option' &&
      refuses.fs === 'player' && refuses.calls.length === 2 && refuses.calls[0].lock === 'browser' && refuses.calls[1].lock === null && refuses.calls[1].navigationUI === 'hide' &&
      refuses.lock === null && /keyboard lock refused \(TypeError/.test(refuses.log) &&
      one(ignores, null) && ignores.lock === null && one(chromium, null),
    `safari: ${JSON.stringify(safari.calls)} → ${safari.lock}; refuses: ${JSON.stringify(refuses.calls)} → ${refuses.lock} (${refuses.log.replace(/^\S+ /, '').slice(0, 80)}); ` +
      `ignores: ${JSON.stringify(ignores.calls)} → ${ignores.lock}; chromium: ${JSON.stringify(chromium.calls)}`);

  // Pointer Lock: unadjustedMovement, on a stubbed requestPointerLock.
  await page.evaluate(() => { window.__plOrig = Element.prototype.requestPointerLock; });
  const plCase = async (kind) => {
    await page.evaluate((k) => {
      window.__recon.pointerRaw = null;
      window.__pl = [];
      Element.prototype.requestPointerLock = function (o) {
        // A browser without the option does not read it; one that reads it
        // rejects what it cannot grant.
        const raw = k === 'ignores' || !o ? null : o.unadjustedMovement;
        window.__pl.push({ raw });
        if (k === 'refuses' && raw) return Promise.reject(new DOMException('unadjustedMovement is not supported', 'NotSupportedError'));
        return Promise.resolve();
      };
    }, kind);
    await page.keyboard.press('Control+Alt+Shift+KeyM'); // game mode: locks the pointer
    await until(() => page.evaluate(() => window.__recon.pointerRaw !== null), 3000, 'pointer lock').catch(() => {});
    const res = await page.evaluate(() => ({ calls: window.__pl, raw: window.__recon.pointerRaw }));
    await page.keyboard.press('Control+Alt+Shift+KeyM'); // back to desktop mode
    return res;
  };
  const plReads = await plCase('reads');
  const plIgnores = await plCase('ignores');
  const plRefuses = await plCase('refuses');
  await page.evaluate(() => { Element.prototype.requestPointerLock = window.__plOrig; });
  check('Pointer Lock: unadjustedMovement reported only when the browser read the option and granted the lock (ignored, refused: not)',
    plReads.raw === true && plReads.calls.length === 1 && plReads.calls[0].raw === true &&
      plIgnores.raw === false && plIgnores.calls.length === 1 && plIgnores.calls[0].raw === null &&
      plRefuses.raw === false && plRefuses.calls.length === 2 && plRefuses.calls[0].raw === true && plRefuses.calls[1].raw === null,
    `reads: ${JSON.stringify(plReads)}; ignores: ${JSON.stringify(plIgnores)}; refuses: ${JSON.stringify(plRefuses)}`);
  await endStream();
}

// The audio jitter buffer at unit level (step 4.6, audio-worklet.js), run
// here in Node on a simulated clock: 5 ms packets of a 440 Hz tone into the
// SharedArrayBuffer ring as the worker writes them, an audio device that
// renders 10 ms at a time in 128-sample quanta (Windows shared mode). Auto on
// a clean LAN (0-1 ms jitter): settles at a 10-20 ms target without an
// underrun, and its trims are crossfaded (no sample-to-sample jump above the
// tone's own slope plus the fade's: no click). 40 ms delay spikes every 2 s
// for 30 s, then a clean link: at most two underruns, then none while the
// target holds above the spike, at most 60 ms; back to 10-20 ms within 30 s
// of the spikes ending, with the level following. Fixed 30 ms: the target
// stays 30 ms. Intermittent audio (sound/silence cycles; WASAPI loopback
// sends nothing while nothing plays): the host moves the pts on by each pause
// and the worker reports the first packet after one ({pause: true}, here 5 ms
// after it arrived, as the page passes it on): every pause's underrun is
// taken back and the target stays at 10-20 ms while sound plays. The worklet
// reports its state once a second.
function simulateJitter({ seconds, jitter = 1, spikeEvery = 0, spikeMs = 0, cleanAfter = Infinity, opts = {}, seed = 1, onMs = Infinity, offMs = 0, pauseMarks = true }) {
  let Proc = null;
  const reports = [];
  vm.runInNewContext(readFileSync(join(root, 'web', 'static', 'js', 'audio-worklet.js'), 'utf8'), {
    sampleRate: 48000, Atomics, Int32Array, Float32Array, SharedArrayBuffer, Math,
    AudioWorkletProcessor: class { constructor() { this.port = { postMessage: (m) => reports.push(m) }; } },
    registerProcessor: (_name, c) => { Proc = c; },
  });
  const sab = new SharedArrayBuffer(8 + 48000 * 2 * 4);
  const p = new Proc({ processorOptions: { sab, ...opts } });
  const idx = new Int32Array(sab, 0, 2);
  const data = new Float32Array(sab, 8);
  const cap = data.length / 2;
  const push = (L) => { // stream-worker.js RingWriter
    const w = Atomics.load(idx, 0);
    const n = Math.min(L.length, cap - 1 - ((w - Atomics.load(idx, 1) + cap) % cap));
    let q = w;
    for (let i = 0; i < n; i++) { data[2 * q] = L[i]; data[2 * q + 1] = L[i]; if (++q === cap) q = 0; }
    Atomics.store(idx, 0, q);
  };
  let rng = seed;
  const rand = () => ((rng = (rng * 16807) % 2147483647) / 2147483647);
  let sent = 0;
  let nextSend = 0;
  let arrival = 0;
  let debt = 0;
  const inflight = [];
  const out = [];
  const underruns = [];
  const levels = [];
  const sounding = (ms) => ms % (onMs + offMs) < onMs;
  let paused = false;
  const marks = []; // when the page passes a pause on to the worklet
  for (let step = 0; step < seconds * 4000; step++) { // 0.25 ms steps
    const t = step / 4;
    for (; nextSend <= t; nextSend += 5, sent += 240) {
      if (!sounding(nextSend)) { paused = true; continue; }
      const spike = spikeEvery && nextSend > 0 && nextSend % spikeEvery === 0 && nextSend < cleanAfter ? spikeMs : 0;
      arrival = Math.max(arrival, nextSend + jitter * rand() + spike); // in order: a late packet holds up the next
      inflight.push({ at: arrival, s: sent, pause: paused });
      paused = false;
    }
    while (inflight.length && inflight[0].at <= t) {
      const { s, pause } = inflight.shift();
      push(Float32Array.from({ length: 240 }, (_, i) => 0.5 * Math.sin((2 * Math.PI * 440 * (s + i)) / 48000)));
      if (pause && pauseMarks) marks.push(t + 5);
    }
    while (marks.length && marks[0] <= t) {
      marks.shift();
      p.port.onmessage({ data: { pause: true } });
    }
    if (step % 40 === 0) { // the device's 10 ms period
      for (debt += 480; debt >= 128; debt -= 128) {
        const L = new Float32Array(128);
        const u = p.underruns;
        p.process([], [[L, new Float32Array(128)]]);
        if (p.underruns > u) underruns.push(t / 1000);
        out.push(...L);
      }
      levels.push({ t: t / 1000, level: p.available() / 48, target: p.target / 48, playing: sounding(t) && !p.buffering });
    }
  }
  return { p, reports, underruns, out, levels };
}

async function checkJitterRule() {
  const at = (r, s) => r.levels.find((l) => l.t >= s);
  const mean = (r, a, b) => { const x = r.levels.filter((l) => l.t >= a && l.t < b); return x.reduce((v, l) => v + l.level, 0) / x.length; };
  const slope = (out, from) => { let m = 0; for (let i = from + 1; i < out.length; i++) m = Math.max(m, Math.abs(out[i] - out[i - 1])); return m; };
  const tone = (2 * Math.PI * 440 / 48000) * 0.5; // the tone's steepest step
  const lan = simulateJitter({ seconds: 20 });
  const lanTarget = lan.p.target / 48;
  const lanSlope = slope(lan.out, 0); // the tone starts at phase 0: no step when playback starts
  const last = lan.reports.at(-1) || {};
  check('jitter buffer (unit): Auto on a clean LAN settles at a 10-20 ms target, no underrun, crossfaded trims (no click), reports once a second',
    lanTarget >= 10 && lanTarget <= 20 && lan.underruns.length === 0 && lan.p.skippedMs > 0 && lanSlope < tone + 0.01 &&
      lan.reports.length >= 19 && last.t === 'jitter' && last.auto === true && last.targetMs === lanTarget,
    `target ${lanTarget.toFixed(1)} ms, underruns ${lan.underruns.length}, level (after each render burst) ${mean(lan, 10, 20).toFixed(1)} ms in 10-20 s, ` +
      `trimmed ${lan.p.skippedMs.toFixed(1)} ms; largest step ${lanSlope.toFixed(4)} (tone ${tone.toFixed(4)}); ${lan.reports.length} reports`);
  const spiky = simulateJitter({ seconds: 70, jitter: 3, spikeEvery: 2000, spikeMs: 40, cleanAfter: 30000 });
  const late = spiky.underruns.filter((t) => t > 5);
  const peak = Math.max(...spiky.levels.map((l) => l.target));
  const calm = at(spiky, 60).target;
  check('jitter buffer (unit): 40 ms delay spikes every 2 s: at most two underruns, then none with the target above the spike (at most 60 ms); 10-20 ms again within 30 s of a clean link',
    spiky.underruns.length <= 2 && late.length === 0 && at(spiky, 20).target >= 40 && peak <= 60 && calm >= 10 && calm <= 20 && mean(spiky, 60, 70) < 20,
    `underruns at ${spiky.underruns.map((t) => `${t.toFixed(1)} s`).join(', ') || 'none'}; target at 5/20/29 s ${[5, 20, 29].map((s) => at(spiky, s).target.toFixed(1)).join('/')} ms (peak ` +
      `${peak.toFixed(1)}), at 60 s ${calm.toFixed(1)} ms; level 20-30 s ${mean(spiky, 20, 30).toFixed(1)} ms, 60-70 s ${mean(spiky, 60, 70).toFixed(1)} ms`);
  const fixed = simulateJitter({ seconds: 10, opts: { auto: false, targetMs: 30 } });
  check('jitter buffer (unit): Fixed 30 ms keeps its target', fixed.p.target / 48 === 30 && fixed.underruns.length === 0 && fixed.reports.at(-1)?.auto === false,
    `target ${fixed.p.target / 48} ms, underruns ${fixed.underruns.length}, level ${mean(fixed, 5, 10).toFixed(1)} ms`);
  // Sounds with pauses between them (the end of each runs the buffer dry):
  // 600 ms every 2.1 s, and 3 s on / 3 s off, for a minute (ending in a
  // sound); hosts before the pause marker for comparison (detail only).
  const gaps = [[600, 1500], [3000, 3000]].map(([on, off]) => {
    const r = simulateJitter({ seconds: 61, onMs: on, offMs: off });
    const playing = r.levels.filter((l) => l.playing && l.t >= 5);
    const lo = Math.min(...playing.map((l) => l.target));
    const hi = Math.max(...playing.map((l) => l.target));
    const unmarked = simulateJitter({ seconds: 61, onMs: on, offMs: off, pauseMarks: false });
    return { on, off, lo, hi, underruns: r.p.underruns, pauses: r.p.pauses, drains: r.underruns.length, unmarked: unmarked.p.target / 48, unmarkedUnderruns: unmarked.p.underruns };
  });
  check('jitter buffer (unit): sounds with pauses (600 ms every 2.1 s; 3 s on, 3 s off) and the host\'s pause marker: every pause\'s underrun taken back, the target 10-20 ms while sound plays',
    gaps.every((g) => g.lo >= 10 && g.hi <= 20 && g.underruns === 0 && g.pauses === g.drains && g.pauses >= 9),
    gaps.map((g) => `${g.on}/${g.off} ms: target ${g.lo.toFixed(1)}-${g.hi.toFixed(1)} ms while playing, ${g.pauses} pauses taken back of ${g.drains} drains, ` +
      `${g.underruns} underruns (without the marker: target ${g.unmarked.toFixed(1)} ms, ${g.unmarkedUnderruns} underruns)`).join('; '));
  results.push({ jitterRule: { lan: { target: lanTarget, slope: lanSlope, skippedMs: lan.p.skippedMs }, spiky: { underruns: spiky.underruns, peak, calm }, gaps } });
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

// Send priorities at unit level (GUIDE 2.7): the worker's own
// openSendChannels and telemetrySender (cut out of stream-worker.js) against
// fake WebTransport objects: one with the whole priority API (send groups,
// WebTransportSendStream.sendOrder, datagrams.createWritable), one like this
// Chromium (none of it). With it, every stream and datagram queue is in one
// send group with input 1000 > control 100 > telemetry 10, and telemetry
// has its own queue (never dropped); without, input and telemetry datagrams
// share the one writable, and while it does not move for 50 ms telemetry is
// dropped instead of queued, input never.
async function checkSendPriorities() {
  const workerSrc = readFileSync(join(root, 'web', 'static', 'js', 'stream-worker.js'), 'utf8');
  const start = workerSrc.indexOf('// Send priorities (GUIDE 2.7)');
  const end = workerSrc.indexOf('async function openWebTransport');
  if (start < 0 || end < 0) throw new Error('send priority code not found in stream-worker.js');
  const expr = `(async () => {
${workerSrc.slice(start, end)}
// A writer whose writes stay pending until flush(): a backed-up queue.
function fakeWriter(name, log) {
  const pending = [];
  return { name, write: (b) => { log.push([name, b[0]]); return new Promise((res) => pending.push(res)); }, flush: () => pending.splice(0).forEach((r) => r()) };
}
async function run(full) {
  const log = [];
  const made = [];
  const plain = fakeWriter('datagrams.writable', log);
  const group = { group: 1 };
  const wt = {
    createBidirectionalStream: async (o) => { made.push({ kind: 'stream', ...o }); return { writable: full ? { sendOrder: o.sendOrder } : {} }; },
    datagrams: { writable: { getWriter: () => plain } },
  };
  if (full) {
    wt.createSendGroup = () => group;
    wt.datagrams.createWritable = (o) => { made.push({ kind: 'datagrams', ...o }); return { getWriter: () => fakeWriter('datagrams' + o.sendOrder, log) }; };
  }
  const ch = await openSendChannels(wt);
  const telemetry = telemetrySender(ch.dgTelemetry, ch.prio, () => false);
  telemetry(Uint8Array.of(0x40));
  telemetry(Uint8Array.of(0x40)); // pending writes alone: no backlog yet
  await new Promise((r) => setTimeout(r, 70)); // the queue stood still for 70 ms
  for (let i = 0; i < 3; i++) telemetry(Uint8Array.of(0x40));
  for (let i = 0; i < 3; i++) ch.dgInput.write(Uint8Array.of(0x21));
  const before = { ...ch.prio };
  plain.flush();
  ch.dgTelemetry.flush();
  await new Promise((r) => setTimeout(r, 0));
  telemetry(Uint8Array.of(0x41)); // the queue drained: telemetry goes out again
  return { made: made.map((m) => ({ kind: m.kind, order: m.sendOrder, grouped: m.sendGroup === group })), prio: before, after: { ...ch.prio }, log };
}
return { full: await run(true), none: await run(false) };
})()`;
  const ctx2 = await browser.newContext({ ignoreHTTPSErrors: true });
  try {
    const p = await ctx2.newPage();
    await p.goto(`${base}/login`);
    const r = await p.evaluate(expr);
    const f = r.full;
    const order = (kind, n) => f.made.filter((m) => m.kind === kind).map((m) => m.order).join(',') === n;
    check('send priorities (unit): with the API, input 1000 > control 100 > telemetry 10 in one send group, telemetry on its own datagram queue, never dropped',
      f.prio.sendOrder && f.prio.sendGroup && f.prio.datagramWritables && f.made.every((m) => m.grouped) &&
        order('stream', '100,1000') && order('datagrams', '1000,10') &&
        f.prio.telemetrySent === 5 && f.prio.telemetryDropped === 0 &&
        f.log.filter(([w, t]) => w === 'datagrams10' && t === 0x40).length === 5 && f.log.filter(([w, t]) => w === 'datagrams1000' && t === 0x21).length === 3,
      JSON.stringify({ made: f.made, prio: f.prio }));
    const n = r.none;
    check('send priorities (unit): without the API, one shared datagram queue: telemetry is dropped while it stands still (50 ms), input never',
      !n.prio.sendOrder && !n.prio.sendGroup && !n.prio.datagramWritables && n.made.every((m) => m.order !== undefined && !m.grouped) &&
        n.prio.telemetrySent === 2 && n.prio.telemetryDropped === 3 && n.after.telemetrySent === 3 &&
        n.log.filter(([, t]) => t === 0x21).length === 3 && n.log.every(([w]) => w === 'datagrams.writable'),
      JSON.stringify({ prio: n.prio, after: n.after, writes: n.log.length }));
  } finally {
    await ctx2.close();
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
  await endStream();
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
// Evidence for the clean-link check (checkLossHandling), installed in every
// stream worker as it starts: each "requesting key frame (...)" log line gets
// what the worker was doing when it asked, from the decoder calls (a
// configure() starts a wait for a key frame, a key chunk's decode() ends it;
// the client's own requests start one too) and from a 50 ms timer that
// measures how late the worker's event loop ran in the last 2 s (frames are
// read, gaps checked and outputs handled on that loop: a starved worker
// declares a gap lost although the frame has arrived). The line ends in
// "[e2e: waiting for a key frame for N ms; ...]" or "[e2e: decoding, last
// chunk fed N ms ago; ...]" and "worker timers up to N ms late in the last 2 s]".
async function instrumentWorker(w) {
  if (!w.url().endsWith('/js/stream-worker.js')) return;
  await w.evaluate(() => {
    if (self.__e2eKey) return;
    // blockedMs: how long the worker's event loop held its 50 ms ticker more
    // than 50 ms late, in all (since: when this began), for the send
    // priority check: a datagram write's resolution waits as long.
    const st = { waitSince: null, fed: null, lag: [], due: performance.now() + 50, since: performance.now(), blockedMs: 0 };
    self.__e2eKey = st;
    const tick = () => {
      const t = performance.now();
      if (t - st.due > 50) st.blockedMs += t - st.due;
      st.lag.push([t, t - st.due]);
      while (st.lag.length && st.lag[0][0] < t - 2000) st.lag.shift();
      st.due = t + 50;
      setTimeout(tick, 50);
    };
    setTimeout(tick, 50);
    const { configure, decode } = VideoDecoder.prototype;
    VideoDecoder.prototype.configure = function (c) {
      if (st.waitSince === null) st.waitSince = performance.now();
      return configure.call(this, c);
    };
    VideoDecoder.prototype.decode = function (chunk) {
      st.fed = performance.now();
      if (chunk.type === 'key') st.waitSince = null;
      return decode.call(this, chunk);
    };
    const pm = self.postMessage;
    self.postMessage = function (m, ...rest) {
      if (m?.type === 'log' && typeof m.text === 'string' && m.text.startsWith('requesting key frame (')) {
        const t = performance.now();
        const late = Math.max(0, t - st.due, ...st.lag.map(([, l]) => l));
        const what = st.waitSince !== null ? `waiting for a key frame for ${Math.round(t - st.waitSince)} ms`
          : `decoding, last chunk fed ${st.fed === null ? 'never' : `${Math.round(t - st.fed)} ms ago`}`;
        m = { ...m, text: `${m.text} [e2e: ${what}; worker timers up to ${Math.round(late)} ms late in the last 2 s]` };
        if (st.waitSince === null) st.waitSince = t;
      }
      return pm.call(this, m, ...rest);
    };
  }).catch(() => {});
}

// stream-worker.js: the 1 s watchdog that asks again for a key frame
// (videoWatchdog), and the shortest wait before a gap counts as lost
// (gapTimeout).
const KEY_WATCHDOG_MS = 1000;
const GAP_TIMEOUT_MS = 250;
const WAITING_KEY = /\[e2e: waiting for a key frame/;

// A "frame lost" key-frame request (a gap that outlasted the late-frame wait)
// that is not about a late frame of a running stream, from the evidence
// instrumentWorker and onConsole append. The worker's own timers ran a gap
// timeout late (the worker was starved: it checked the gap before reading a
// frame that had arrived). Or the client was waiting for a key frame (a new
// generation's, or after a reset), so the late frame is that key frame, and
// it had waited the watchdog's second already (the watchdog asks again
// anyway) or the CPUs had nothing to spare in the 2 s before (a starved host
// encodes and sends the large key frame late). null: a gap in a running
// stream, the check's failure.
function starvedKeyRequest(line) {
  const wait = line.match(/\[e2e: waiting for a key frame for (\d+) ms/);
  const logWait = line.match(/\[log: waiting for a key frame, asked again (\d+) times/);
  const late = line.match(/worker timers up to (\d+) ms late/);
  const cpu = line.match(/\[cpu: (\d+) % idle/);
  if (late && +late[1] >= GAP_TIMEOUT_MS) return `worker timers ${late[1]} ms late`;
  if (wait && +wait[1] >= KEY_WATCHDOG_MS) return `waiting for a key frame for ${wait[1]} ms`;
  if (logWait && +logWait[1] >= 1) return `waiting for a key frame, the watchdog asked ${logWait[1]} times`;
  if ((wait || logWait) && cpu && +cpu[1] < 100 * STARVED_IDLE) return `waiting for a key frame, CPUs ${cpu[1]} % idle`;
  return null;
}

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

// CPU starvation evidence for the frame-rate checks: the share of time the
// CPUs this process may run on (its affinity, which the browsers, the host
// and its encoders inherit: taskset, a CI runner's two vCPUs) were idle,
// from /proc/stat. Below STARVED_IDLE in a window the machine had no CPU to
// spare (on two vCPUs under a third of one: the software decoder, the
// emulated GPU and the encoder each need a whole core in bursts): they were
// all short of it, and a frame rate below the stream's says nothing about the
// code. A rate check then judges what the client drew against what reached
// its decoder in the same window (starvedRate). null: not measurable here.
const STARVED_IDLE = 0.15;
const myCpus = (() => {
  try {
    const list = readFileSync('/proc/self/status', 'utf8').match(/^Cpus_allowed_list:\s*(\S+)/m)[1];
    const set = new Set();
    for (const part of list.split(',')) {
      const [a, b] = part.split('-').map(Number);
      for (let i = a; i <= (b ?? a); i++) set.add(i);
    }
    return set;
  } catch {
    return null;
  }
})();
function cpuTimes() {
  if (!myCpus) return null;
  try {
    let idle = 0;
    let total = 0;
    for (const line of readFileSync('/proc/stat', 'utf8').split('\n')) {
      const m = line.match(/^cpu(\d+)\s+(.*)/);
      if (!m || !myCpus.has(+m[1])) continue;
      const v = m[2].trim().split(/\s+/).map(Number);
      idle += v[3] + v[4];
      total += v.slice(0, 8).reduce((a, b) => a + b, 0);
    }
    return { idle, total };
  } catch {
    return null;
  }
}
const idleShare = (a, b) => (a && b && b.total > a.total ? (b.idle - a.idle) / (b.total - a.total) : null);
// For the record in checks that do not judge rates (the loss scenarios).
const idleNote = (idle) => `CPUs ${idle == null ? '?' : (100 * idle).toFixed(0)} % idle${idle != null && idle < STARVED_IDLE ? ' (no CPU to spare)' : ''}`;

// The frame rate over ms: drawn (the client's stats, every 0.5 s), the
// decoder's input (its decode() calls, watchDecoder: what the host
// delivered) and the CPUs' idle share meanwhile.
async function rateWindow(ms) {
  const w = streamWorker();
  if (w && !(await w.evaluate(() => !!self.__decoderCalls).catch(() => true))) await watchDecoder();
  const decodes = async () => (w ? w.evaluate(() => [performance.now(), self.__decoderCalls?.decodes ?? null]).catch(() => null) : null);
  const superseded = () => page.evaluate(() => window.__recon.lastStats?.superseded ?? null).catch(() => null);
  const c0 = cpuTimes();
  const d0 = await decodes();
  const s0 = await superseded();
  const fps = [];
  for (let t = 0; t < ms; t += 500) {
    await sleep(500);
    fps.push(await page.evaluate(() => window.__recon.lastStats?.fps ?? 0).catch(() => 0));
  }
  const d1 = await decodes();
  const s1 = await superseded();
  const secs = d0 && d1 && d1[0] > d0[0] ? (d1[0] - d0[0]) / 1000 : ms / 1000;
  return {
    fps: fps.reduce((a, b) => a + b, 0) / Math.max(1, fps.length), samples: fps,
    decFps: d0?.[1] != null && d1?.[1] != null && d1[0] > d0[0] ? (d1[1] - d0[1]) / secs : null,
    supFps: s0 !== null && s1 !== null ? (s1 - s0) / secs : null,
    idle: idleShare(c0, cpuTimes()),
  };
}

// A frame-rate check's alternative where its window (w: rateWindow, or the
// same fields) had no CPU to spare: the client keeps up with what the host
// delivered, that is `got` fps plus the frames the pacer superseded (outputs
// that came in a burst, closed unseen for the newest: pacing.js) at least
// `share` of what reached the decoder, which itself is a quarter of the
// stream's nominal `rate` at least (the stream runs). The note gives the
// evidence either way.
function starvedRate(got, rate, share, w) {
  const starved = w?.idle != null && w.idle < STARVED_IDLE;
  const sup = w?.supFps ?? 0;
  const ok = starved && w.decFps != null && w.decFps >= rate / 4 && got + sup >= share * w.decFps;
  const note = `; CPUs ${w?.idle == null ? '?' : (100 * w.idle).toFixed(0)} % idle, the decoder got ${w?.decFps == null ? '?' : w.decFps.toFixed(1)} fps` +
    `${w?.supFps == null ? '' : `, ${w.supFps.toFixed(1)} fps superseded`}` +
    (starved ? ` (no CPU to spare: ${ok ? 'judged against' : 'below'} what reached the decoder)` : '');
  return { ok, note };
}

// Ends the page's stream when its scenario ends: the page goes back to the
// dashboard, which tears the client down (worker, decoder, renderer) and
// ends the host's session (and stays on the gateway's origin, where the unit
// checks import the app's modules). __recon.userClosed alone only stops
// reconnecting: the stream ran on until the next connection replaced the
// session (the host streams to one client at a time), and a page whose
// session was replaced kept its worker and renderer, all competing with the
// next scenario's setup for the CPU (two vCPUs on CI).
async function endStream(p = page) {
  await p.evaluate(() => { if (window.__recon) window.__recon.userClosed = true; }).catch(() => {});
  await p.goto(`${base}/`).catch(() => {});
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
async function lossRun(name, faults, seconds, prefs = {}) {
  const host = await restartHost({ RECON_TEST_FAULTS: faults }, name);
  await startStream({ path: 'auto', transport: 'auto', ...prefs });
  const calls = await watchDecoder();
  await sleep(4000); // decoder warm-up
  const st0 = await page.evaluate(() => window.__recon.lastStats);
  const log0 = host.log.length;
  const con0 = consoleLines.length;
  const c0 = cpuTimes();
  const fps = [];
  for (const end = Date.now() + seconds * 1000; Date.now() < end;) {
    await sleep(500);
    fps.push((await page.evaluate(() => window.__recon.lastStats))?.fps ?? 0);
  }
  const idle = idleShare(c0, cpuTimes());
  const st = await page.evaluate(() => window.__recon.lastStats);
  const cfg = await page.evaluate(() => window.__recon.videoCfg);
  const hl = host.log.slice(log0);
  const con = consoleLines.slice(con0);
  const delta = (k) => (st?.[k] ?? 0) - (st0?.[k] ?? 0);
  return {
    name, faults, seconds, cfg, idle, fps: fps.reduce((a, b) => a + b, 0) / Math.max(1, fps.length),
    delayed: (hl.match(/msg="test fault: delaying frame"/g) || []).length,
    dropped: (hl.match(/msg="frames dropped".*? why="test fault"/g) || []).length,
    restarts: restartsByReason(hl),
    keyRequestReasons: keyRequestsByReason(con),
    // The same, split by what the client was doing when it asked
    // (instrumentWorker): waiting for a key frame already (a re-request: a
    // generation it gave up, a key frame not decoded yet) or decoding.
    keyWhileWaiting: keyRequestsByReason(con.filter((l) => WAITING_KEY.test(l))),
    keyWhileDecoding: keyRequestsByReason(con.filter((l) => !WAITING_KEY.test(l))),
    // The loss-recovery ladder (GUIDE 2.3): frame streams cancelled past
    // their deadline (rung 1), frames the host did not send while the client
    // waited for a recovery or key frame (one line per run: its count) and in
    // how many runs, IDRs forced in the encoder.
    cancelled: (hl.match(/msg="frame stream cancelled"/g) || []).length,
    hostDiscarded: [...hl.matchAll(/msg="frames dropped".*? why="awaiting (?:recovery|key) frame".*? count=(\d+)/g)]
      .reduce((a, m) => a + Number(m[1]), 0),
    hostDiscardRuns: (hl.match(/msg="frames dropped".*? why="awaiting (recovery|key) frame"/g) || []).length,
    forcedKeys: (hl.match(/msg="forcing a key frame"/g) || []).length,
    client: {
      keyRequests: delta('keyRequests'), hostDropped: delta('hostDropped'), skipped: delta('skipped'), lost: delta('dropped'),
      recovered: delta('recovered'), recoveredByKey: delta('recoveredByKey'), discarded: delta('recoveryDiscarded'),
      rejected: delta('recoveryRejected'), keyFrames: delta('keyFrames'), freezes: delta('freezes'),
      // Frame streams the host reset whose header arrived (GUIDE 2.4).
      streamResets: delta('streamResets'),
      thinned: delta('thinned'), thinnedTotal: st?.thinned ?? 0,
    },
    // Whether the browser negotiated RESET_STREAM_AT (the host's session line).
    resetStreamAt: ((host.log.match(/msg="session started"[^\n]*/g) || []).pop()?.match(/ reset_stream_at=(\S+)/) || [])[1] ?? null,
    // Reference recovery on the host: losses it asked the encoder to recover,
    // and how the encoder answered (recovery frame, key frame).
    recovering: (hl.match(/msg="recovering from a loss"/g) || []).length,
    recoveredByFrame: (hl.match(/msg="loss recovered".*? by="recovery frame"/g) || []).length,
    recoveredByKey: (hl.match(/msg="loss recovered".*? by="key frame"/g) || []).length + (hl.match(/msg="no recovery frame possible/g) || []).length,
    // Of them, losses of a generation's key frame (seq 0, e.g. the frame
    // queue overflowing as a new generation starts on a starved host):
    // nothing to recover from, a key frame is rung 2's only answer.
    keyFrameLosses: (hl.match(/msg="no recovery frame possible[^\n]*? from_seq=0 /g) || []).length,
    // Losses (the hook's drops, deadline cancels) whose generation a restart
    // replaced (congestion, bitrate recovery: frequent on a starved host)
    // before the encoder answered them: the new generation's key frame ends
    // the client's wait, no recovery frame can come.
    supersededLosses: (() => {
      const open = new Map();
      let n = 0;
      for (const l of hl.split('\n')) {
        let m;
        if ((m = l.match(/msg="recovering from a loss".*? gen=(\d+) from_seq=(\d+) why=(deadline|"test fault")/))) open.set(`${m[1]}/${m[2]}`, +m[1]);
        else if ((m = l.match(/msg="(?:loss recovered|no recovery frame possible[^"]*)".*? gen=(\d+) from_seq=(\d+)/))) open.delete(`${m[1]}/${m[2]}`);
        else if ((m = l.match(/msg="encoder ready".*? gen=(\d+)/))) {
          for (const [k, g] of open) if (g !== +m[1]) { open.delete(k); n++; }
        }
      }
      return n;
    })(),
    hostLog: hl,
    // The decoder's own error lines, not the key-frame requests they cause.
    decoderErrors: con.filter((l) => l.includes('decoder error:')).length,
    // Recovery frames decoded as soon as they were buffered, ahead of late frames before them.
    lateSkips: con.filter((l) => l.includes('ends the recovery wait')).length,
    calls, st,
  };
}

// The host cannot reach the relay port (a firewall in front of the gateway
// that lets only the main port through): the gateway answers the allocation
// with 504 (internal/e2e TestUDPRelayHostCannotBind drops a real host's bind;
// here Playwright answers for the gateway). The client falls back to the
// splice at once and skips the UDP relay on the next connect of the page.
async function checkUdpRelayHostBlocked() {
  let allocations = 0;
  const route = (r) => { allocations++; return r.fulfill({ status: 504, json: { error: 'the host did not reach the relay port' } }); };
  await page.goto(`${base}/`);
  await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify(p)), { path: 'relay', ...PREFS_2D });
  await ctx.route('**/api/relay/udp*', route);
  try {
    const con0 = consoleLines.length;
    await page.click('.host.online a.btn-primary');
    await page.waitForURL(/\/stream\?host=/);
    await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
    await page.click('#btn-start');
    await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
    const first = await page.evaluate(() => window.__recon.conn.path);
    const why = (consoleLines.slice(con0).find((l) => l.includes('relay failed:')) || '').replace(/^.*?relay failed/, 'relay failed');
    check('UDP relay, host cannot bind: falls back to the splice', first === 'relay-splice' && allocations === 1 &&
      /^relay failed: relay allocation: the host did not reach the relay port/.test(why), `${first}; ${why.slice(0, 160)}`);
    // Reconnect in the same page (the drawer's Reconnect button).
    await page.evaluate(() => {
      window.__recon.conn = null;
      [...document.querySelectorAll('#drawer button')].find((b) => b.textContent.includes('Reconnect')).click();
    });
    await page.waitForFunction(() => window.__recon.streaming && window.__recon.conn, null, { timeout: 30000 });
    const second = await page.evaluate(() => window.__recon.conn.path);
    check('UDP relay, host cannot bind: the next connect skips the UDP relay', second === 'relay-splice' && allocations === 1,
      `${second}, ${allocations} allocation request(s)`);
  } finally {
    await endStream();
    await ctx.unroute('**/api/relay/udp*', route);
  }
}

async function checkLossHandling() {
  // The scenarios so far ran on a clean loopback link ("lan"): no gap may
  // have been taken for a loss. Restarts for other reasons (settings changes,
  // decoder backlog and congestion on this CPU-only machine) and frames the
  // host dropped on queue overflow are listed for the record.
  // Decoder backlog and watchdog requests are this CPU-only machine's (not a
  // loss): listed. A gap's request ("frame lost") fails the check, unless
  // the worker's evidence shows it was no late frame of a running stream
  // (starvedKeyRequest): then it is listed with that evidence.
  const lan = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  const lanKeys = keyRequestsByReason(consoleLines);
  const lanLost = consoleLines.filter((l) => /requesting key frame \(frame lost\)/.test(l));
  const gapKeys = lanLost.filter((l) => !starvedKeyRequest(l));
  const evidence = (l) => [...l.matchAll(/\[(?:e2e|log|cpu): ([^\]]*)\]/g)].map((m) => m[1]).join('; ') || 'no evidence';
  check('clean link (lan): no key frame requested for a gap in the sequence (late frames wait)', gapKeys.length === 0,
    `client key-frame requests: ${counts(lanKeys)}; host encoder restarts: ${counts(restartsByReason(lan.log))}; ` +
      `host frame drops: ${(lan.log.match(/msg="frames dropped"/g) || []).length}` +
      `${lanLost.length ? `; "frame lost": ${lanLost.map((l) => `${starvedKeyRequest(l) ? 'excused' : 'A GAP IN A RUNNING STREAM'} (${evidence(l)})`).join(', ')}` : ''}`);
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
  await endStream();

  // Recovery "keyframe" (the software encoders have no intra refresh).
  const k = await lossRun('host-faults', LOSS_FAULTS, 20);
  // Key-frame restarts: for a client's request, or (the ladder's rung 4)
  // for a loss the host knows of, at once (the client's request for the
  // same loss then finds the key frame on its way).
  const kfRestarts = (k.restarts['keyframe request (urgent)'] || 0) + (k.restarts['frame lost (urgent)'] || 0);
  // A late frame that outlasted the gap timeout would show as "frame lost";
  // every key-frame restart needs a logged reason (the drops, a client
  // request: a decoder error, the watchdog). Other restarts (congestion) are
  // listed, not checked: this CPU-only machine also falls behind on its own.
  // Under "keyframe" a late frame is never cancelled (its loss would cost a
  // key frame; GUIDE 2.3 rung 1 only where rung 2 answers the loss).
  const kRequests = Object.values(k.keyRequestReasons).reduce((a, b) => a + b, 0);
  check('late frames (200 ms) cause no key-frame request ("frame lost") and are not cancelled; every key-frame restart answers a loss or a logged request',
    k.delayed >= 5 && !k.keyRequestReasons['frame lost'] && k.cancelled === 0 && kfRestarts <= kRequests + k.dropped,
    `${k.delayed} frames delayed 200 ms (${k.cancelled} cancelled), ${k.dropped} dropped; client key-frame requests: ${counts(k.keyRequestReasons)}; ` +
      `host restarts: ${counts(k.restarts)}`);
  // The late frames release bursts of the frames buffered behind them: the
  // decode queue bound must hold there too.
  await checkHygiene('late frames (bursts)', k.calls, k.st);
  check('dropped frames are reported ("dropped") and recovered with a key frame (recovery "keyframe")',
    k.cfg?.recovery === 'keyframe' && k.dropped >= 3 && k.client.hostDropped >= k.dropped - 1 &&
      (k.keyRequestReasons['dropped by host'] || 0) >= 1 && kfRestarts >= 1 && k.fps >= 10,
    `${k.cfg?.encoder} recovery ${k.cfg?.recovery}: host dropped ${k.dropped}, client told ${k.client.hostDropped}, ` +
      `key requests ${k.client.keyRequests} (${counts(k.keyRequestReasons)}), key-frame restarts ${kfRestarts}, ${k.fps.toFixed(1)} fps mean over ${k.seconds} s`);
  await endStream();

  // Recovery "skip", forced through the hook: the client skips the dropped
  // frame and decodes on, without a key-frame request. The software AV1
  // encoder has no intra refresh, so the picture stays damaged, and Chrome's
  // AV1 decoder can reject a later frame (entropy-coding state inherited from
  // the missing reference): then, and only then, the decoder-error fallback
  // (reset + key frame) runs. A drop that falls into a generation the client
  // has already given up (decoder backlog or error, waiting for its key
  // frame) needs nothing at all, so not every report is a skip.
  const s = await lossRun('host-faults-skip', `${LOSS_FAULTS},recovery=skip`, 20);
  // A request for a drop or a gap made while the client already waited for a
  // key frame (after a decoder error or backlog; the new generation's key
  // frame not decoded yet) asks again for what it needs anyway: not one for
  // the loss itself (the host's urgent restarts may answer it, as they
  // answer the decoder errors and the watchdog).
  const skipRestarts = s.restarts['keyframe request (urgent)'] || 0;
  const skipAgain = (s.keyWhileWaiting['dropped by host'] || 0) + (s.keyWhileWaiting['frame lost'] || 0);
  check('dropped frames skipped (recovery "skip"): no key-frame request for the loss itself, late frames not cancelled, playback continues',
    s.cfg?.recovery === 'skip' && s.dropped >= 3 && s.client.hostDropped >= s.dropped - 1 && s.client.skipped >= 1 && s.cancelled === 0 &&
      !s.keyWhileDecoding['dropped by host'] && !s.keyWhileDecoding['frame lost'] &&
      skipRestarts <= (s.keyRequestReasons['decoder error'] || 0) + (s.keyRequestReasons.watchdog || 0) + (s.keyRequestReasons['decoder backlog'] || 0) + skipAgain &&
      s.fps >= 10,
    `${s.cfg?.encoder} recovery ${s.cfg?.recovery}: host dropped ${s.dropped}, client told ${s.client.hostDropped}, skipped ${s.client.skipped}, ` +
      `decoder errors after a skip ${s.decoderErrors} (fallback: reset + key frame); client key-frame requests: ${counts(s.keyRequestReasons)}` +
      `${Object.keys(s.keyWhileWaiting).length ? ` (while already waiting for a key frame: ${counts(s.keyWhileWaiting)})` : ''}; ` +
      `host restarts: ${counts(s.restarts)}; ${s.fps.toFixed(1)} fps mean over ${s.seconds} s; ${idleNote(s.idle)}`);
  // Reference recovery (GUIDE 3.5) on the software path: the hook makes the
  // software encoder (libsvtav1: Playwright's Chromium decodes no H.264;
  // internal/host/media TestTestRecovery covers libx264 too) stand in for an
  // encoder that recovers by reference invalidation
  // (ref-recovery: a key frame every fps/6 frames, sent as P-frames; after a
  // loss the host flags the next one RECOVERY with refFloor = the frame
  // before the loss; recovery "invalidate" announced). Every dropped frame
  // must be answered by such a frame: the client decodes nothing from the
  // lost frame until it (the last good picture stays), resumes with it, and
  // asks for no key frame; the host starts no new generation for a loss.
  // No late frame outlasts the gap timeout ("frame lost"). The client counts
  // the key frames (IDRs) it decoded and the frames it waited out.
  //
  // The loss-recovery ladder (GUIDE 2.3) on the same run: each frame the
  // hook delays has its stream stand still for 200 ms while the next frames
  // are ready, past its deadline (two frame intervals, 33 ms at 60 fps), so
  // the host cancels it (rung 1: "frame stream cancelled") and treats it as
  // lost; the encoder answers with a recovery frame (rung 2), and the host
  // does not send the frames up to it (the client would discard them; they
  // are reported dropped). Counted: cancelled frames, recoveries, IDRs (the
  // client's key frames, the host's forced ones) and encoder restarts.
  const r = await lossRun('host-faults-ref', `${LOSS_FAULTS},ref-recovery`, 20);
  const refRestarts = (r.restarts['keyframe request (urgent)'] || 0) + (r.restarts['frame lost (urgent)'] || 0);
  // (requests made while the client already waited for a key frame ask again
  // for what it needs anyway, as in the "skip" check above)
  const lossKeys = ['dropped by host', 'frame lost', 'no recovery frame'].reduce((a, k) => a + (r.keyWhileDecoding[k] || 0), 0);
  const refAgain = ['dropped by host', 'frame lost', 'no recovery frame'].reduce((a, k) => a + (r.keyWhileWaiting[k] || 0), 0);
  // Restarts the client's logged requests that are not for a loss ask for:
  // decoder errors, its watchdog, a decoder backlog (a starved decoder),
  // re-requests while it waited for a key frame already.
  const refAllow = (r.keyRequestReasons['decoder error'] || 0) + (r.keyRequestReasons.watchdog || 0) + (r.keyRequestReasons['decoder backlog'] || 0) + refAgain;
  const hostRec = r.recoveredByFrame + r.recoveredByKey - r.keyFrameLosses;
  const losses = r.dropped + r.cancelled;
  check('reference recovery (software stand-in): dropped frames recovered by a recovery frame, frames up to it not decoded, no key-frame request or restart for a loss',
    r.cfg?.recovery === 'invalidate' && r.dropped >= 3 && r.client.hostDropped >= losses - 1 &&
      r.recoveredByFrame >= losses - 2 - r.supersededLosses && r.recoveredByFrame >= 0.9 * hostRec &&
      r.client.recovered >= losses - 2 - r.supersededLosses && r.client.discarded + r.hostDiscarded > 0 && r.client.rejected === 0 && r.decoderErrors === 0 &&
      lossKeys === 0 && refRestarts <= refAllow &&
      !r.keyWhileDecoding['frame lost'] && r.fps >= 10,
    `${r.cfg?.encoder} recovery ${r.cfg?.recovery}: host dropped ${r.dropped} and cancelled ${r.cancelled} (of ${r.delayed} delayed 200 ms), ` +
      `asked the encoder to recover ${r.recovering}, answered by recovery frame ${r.recoveredByFrame} / by key frame ${r.recoveredByKey}` +
      `${r.supersededLosses ? ` (${r.supersededLosses} losses unanswered: their generation replaced by a restart first)` : ''}; ` +
      `client told ${r.client.hostDropped} (${r.hostDiscarded} not sent while it waited, in ${r.hostDiscardRuns} reports), ` +
      `recovered ${r.client.recovered} by recovery frame and ${r.client.recoveredByKey} by key frame, ${r.client.discarded} frames discarded meanwhile ` +
      `(${r.lateSkips} times without waiting for a late frame before the recovery frame), ` +
      `${r.client.keyFrames} IDRs decoded, decoder errors ${r.decoderErrors}; client key-frame requests: ${counts(r.keyRequestReasons)}` +
      `${Object.keys(r.keyWhileWaiting).length ? ` (while already waiting for a key frame: ${counts(r.keyWhileWaiting)})` : ''}; ` +
      `host restarts: ${counts(r.restarts)}; ${r.fps.toFixed(1)} fps mean over ${r.seconds} s; ${idleNote(r.idle)}`);
  const restartsAll = Object.values(r.restarts).reduce((a, b) => a + b, 0);
  check('loss-recovery ladder: frames held past their deadline are cancelled (rung 1) and recovered without a key frame (rung 2): no IDR, no restart for a loss',
    r.delayed >= 5 && r.cancelled >= r.delayed - 2 && r.recoveredByFrame >= r.cancelled - 1 && r.recoveredByKey === r.keyFrameLosses &&
      r.forcedKeys === 0 && refRestarts <= refAllow && r.client.keyFrames <= 1 + restartsAll,
    `${r.delayed} streams held 200 ms, ${r.cancelled} cancelled at their deadline, ${r.dropped} dropped by the hook; ` +
      `recoveries: ${r.recoveredByFrame} by recovery frame, ${r.recoveredByKey} by key frame` +
      `${r.keyFrameLosses ? ` (${r.keyFrameLosses} for a lost generation key frame: nothing to recover from)` : ''}; ` +
      `restarts for a key-frame request ${refRestarts} (the client's requests not for a loss: ${refAllow}); IDRs: ${r.client.keyFrames} decoded by the client ` +
      `(1 = the generation's first), ${r.forcedKeys} forced by the host; encoder restarts: ${restartsAll} (${counts(r.restarts)}); ` +
      `client freezes > 100 ms: ${r.client.freezes}; ${idleNote(r.idle)}`);
  // Partial delivery (GUIDE 2.4) on the same run: where the browser
  // negotiated RESET_STREAM_AT, the host marks each frame stream's header
  // reliable before its payload (the hook's held streams too: quic-go takes
  // the small header write at once), so every stream cancelled at its
  // deadline still delivers its header and the client learns of the loss
  // in-band (streamResets). Where it did not (Chromium 141: docs/VENDOR_NOTES.md
  // 2.4) a cancel is a plain reset of a stream with nothing written, and the
  // client learns of it from the "dropped" report alone. Headers of the
  // hook's half-written (dropped) frames may arrive either way: read before
  // the reset.
  const partialOK = r.resetStreamAt === 'yes' ? r.client.streamResets >= r.cancelled - 1 : r.resetStreamAt === 'no';
  check('partial delivery (RESET_STREAM_AT): detected per session; where negotiated, cancelled frame streams deliver their header',
    partialOK && r.cancelled >= 1,
    `Chromium ${browser.version()}: reset_stream_at=${r.resetStreamAt}; ${r.cancelled} frame streams cancelled at their deadline, ` +
      `reset streams whose header the client read: ${r.client.streamResets} (keyframe run ${k.client.streamResets}, skip run ${s.client.streamResets}, ` +
      `of ${k.dropped} / ${s.dropped} / ${r.dropped} half-written by the hook)`);
  results.push({ partialDelivery: { browser: browser.version(), resetStreamAt: r.resetStreamAt, cancelled: r.cancelled,
    streamResets: { keyframe: k.client.streamResets, skip: s.client.streamResets, ref: r.client.streamResets } } });
  await checkProbe('reference recovery');
  // The drop test under reference recovery: the client drops a frame itself,
  // reports it ({"t":"lost"}), and the host answers with a recovery frame.
  const rts = [];
  for (let i = 0; i < 2; i++) {
    await page.evaluate(() => { window.__recon.dropTest = null; window.__recon.worker.postMessage({ type: 'dropTest' }); });
    rts.push(await until(() => page.evaluate(() => window.__recon.dropTest), 8000, 'drop test result').catch(() => null));
  }
  const host = procs.find((p) => p.spawnargs.includes('run') && p.exitCode === null);
  const clientLost = (host.log.match(/msg="recovering from a loss".*? why=client/g) || []).length;
  check('reference recovery: a loss only the client saw (drop test) is reported ("lost") and answered by a recovery frame the decoder accepts',
    rts.every((d) => d?.ok && d.recovery === 'invalidate' && d.recoveredBy && !d.recoveredBy.key && d.recoveredBy.refFloor < d.seq) && clientLost >= rts.length,
    `${rts.map((d) => (d ? `${d.gen}/${d.seq}: ${d.recoveredBy ? `${d.recoveredBy.key ? 'key frame' : `recovery frame ${d.recoveredBy.seq} (refFloor ${d.recoveredBy.refFloor})`} after ${d.recoveredBy.ms} ms, ${d.recoveredBy.discarded} discarded` : 'not recovered'}, ${d.decoded} decoded${d.error ? `, ${d.error}` : ''}` : 'no result')).join('; ')}; ` +
      `host: ${clientLost} client-reported losses recovered`);
  delete r.hostLog;
  delete k.hostLog;
  delete s.hostLog;
  results.push({ loss: 'faults', keyframe: k, skip: s, ref: r, refDropTests: rts });
  await endStream();
}

// ---------------------------------------------------------------------------
// Temporal SVC thinning (Phase 5 wiring A). Under congestion the host leaves
// out frames no other frame references, before they are sent; the frames
// after them carry the frame extension's "thinned" mask (clients with hello
// v >= 4), and the client skips those seqs: no loss, no "lost" report, no
// recovery, no key-frame request, the frame rate drops for the moment. What
// is real here: the frames left out are the software AV1 encoder's own
// non-reference frames (SVT-AV1's low-delay structure codes every second
// frame with refresh_frame_flags 0; the host reads that from the bitstream,
// internal/codec Discardable, as the native helper reports its SVC
// enhancement layer), the leaving out, the masks and the client's handling.
// What is simulated: the congestion. The host's test-only hook
// thin=every:120:for:40 puts the last 40 frames of every 120 under pressure
// (as the rate controller's delay signal, a building frame queue or a frame
// stream past its deadline would), 0.67 s episodes, short of the second after
// which lasting thinning would cut the bitrate. The recovery mode is
// "keyframe" (FFmpeg path), so any frame taken for lost would show as a key
// request; the frame barcodes check that the frames after a left-out one
// decode to the right pictures. Adaptive bitrate is off and the bitrate 8
// Mbit/s, so this CPU-only machine's own delay does not restart the encoder
// in the window (an overlapped restart can overflow the frame queue: frames
// the host drops and reports, which are no thinning); the real signals still
// thin. Frames the host did drop for a reason of its own (logged "frames
// dropped") are told to the client as before and are the only losses allowed.

async function checkThinning() {
  const t = await lossRun('host-thin', 'thin=every:120:for:40', 15, { adaptive: false, bitrate: 8 });
  const hl = t.hostLog;
  const ended = [...hl.matchAll(/msg="thinning ended".*? frames=(\d+)/g)].map((m) => +m[1]);
  const hostThinned = ended.reduce((a, b) => a + b, 0);
  // Episodes by what started them: one the machine's own delay started before the hook's pressure
  // window goes on through it under that name.
  const why = {};
  for (const m of hl.matchAll(/msg="thinning: leaving out discardable frames under congestion".*? why=("[^"]*"|\S+)/g)) {
    const k = m[1].replace(/"/g, '');
    why[k] = (why[k] || 0) + 1;
  }
  const episodes = Object.values(why).reduce((a, b) => a + b, 0);
  // Where the CPUs had nothing to spare (2-vCPU runners), the stream plays below 30 fps without any thinning.
  const starved = t.idle != null && t.idle < STARVED_IDLE;
  // The host's own drops (queue overflow, awaiting a key frame): logged with their reason, never a thinned frame.
  const hostDrops = [...hl.matchAll(/msg="frames dropped".*? count=(\d+)/g)].reduce((a, m) => a + +m[1], 0);
  // Key requests: none for a loss; a software decoder that falls behind (decoder backlog) is this machine's, and a
  // frame the host dropped of its own is answered by a key frame in recovery "keyframe". Counted while decoding
  // (instrumentWorker): a frame taken for lost asks then; the watchdog's re-asks while a key frame is awaited
  // already (after a decoder backlog flush on a starved machine) are not another loss.
  const lossKeys = Object.entries(t.keyWhileDecoding)
    .filter(([k]) => k !== 'decoder backlog' && !(hostDrops > 0 && k === 'dropped by host')).reduce((a, [, v]) => a + v, 0);
  check('temporal SVC thinning: the host leaves out the encoder\'s non-reference frames under (simulated) congestion; the client skips them: no loss, no recovery, no key-frame request',
    t.cfg?.recovery === 'keyframe' && episodes >= 5 && (why['test fault'] || 0) >= 3 && ended.length >= 4 && hostThinned >= 40 &&
      // The window's first episode may have begun before it, the last may not have ended (20 frames each at most).
      Math.abs(t.client.thinned - hostThinned) <= 20 && t.client.lost <= hostDrops && t.client.hostDropped <= hostDrops &&
      lossKeys === 0 && t.client.recovered === 0 && t.client.recoveredByKey === 0 &&
      t.decoderErrors === 0 && t.dropped === 0 && (t.fps >= 30 || (starved && t.fps >= 10)),
    `${t.cfg?.encoder} (recovery ${t.cfg?.recovery}, ${t.cfg?.bitrate} kbps): ${episodes} thinning episodes in the window, ${ended.length} ended with ${hostThinned} frames left out ` +
      `(${ended.join(', ')}; started by ${counts(why)}); client: ${t.client.thinned} frames skipped as thinned (${t.client.thinnedTotal} this stream), lost ${t.client.lost}, ` +
      `host-dropped ${t.client.hostDropped} (host logged ${hostDrops} drops of its own), key requests ${t.client.keyRequests} (${counts(t.keyRequestReasons)}), ` +
      `recovered ${t.client.recovered}/${t.client.recoveredByKey}, decoder errors ${t.decoderErrors}, freezes ${t.client.freezes}; ${t.fps.toFixed(1)} fps mean over ${t.seconds} s; ` +
      `${idleNote(t.idle)}`);
  await checkProbe('temporal SVC thinning');
  delete t.hostLog;
  results.push({ thinning: t });
  await endStream();
}

// ---------------------------------------------------------------------------
// Bitrate recovery (guide steps 1.5 and 2.2). A congestion report (as older
// clients send it; this one sends rate reports, so the host's rate
// controller judges the delay) cuts the bitrate x0.85, or to 0.85 x what the
// connection delivered; while the client's rate reports show the one-way
// delay back at its base, the rate controller raises it back to the setting,
// each step an overlapped (non-urgent) restart at least a second apart, and
// the overlay shows the target. Back-offs of this CPU-only machine's own
// (decoder backlog, or the delay of a software decoder that falls behind)
// may add cuts; the climb back to the setting must still happen, or, after a
// decoder flush, to the cap the host then logs (85 % of the bitrate the
// decoder fell behind at).

async function checkBitrateRecovery() {
  const host = await restartHost({}, 'host-rate');
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
  const climb0 = cpuTimes();
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
  const climbIdle = idleShare(climb0, cpuTimes());
  const cuts = changes(cutRe);
  const raises = changes(raiseRe);
  // Back at the setting; or where the CPUs had nothing to spare during the
  // climb (the delay the rate controller measures then comes from the starved
  // encoder, decoder and browser, and it cuts again), raised after its cuts
  // at least once, in the same steps.
  const climbStarved = climbIdle != null && climbIdle < STARVED_IDLE;
  const backAtTop = raises.length >= 1 && raises[raises.length - 1][1] === limit && cfg?.bitrate === limit;
  const restarts = restartsByReason(host.log.slice(log0));
  const target = (overlay.match(/target\s*([^\n]*)/) || [])[1] || '';
  // Freezes (the client's "freeze: N ms" log): recorded, not checked; on this
  // CPU-only machine every switch also runs a second software encoder.
  const freezes = consoleLines.slice(con0).map((l) => +(l.match(/freeze: (\d+) ms/) || [])[1]).filter((v) => v > 0);
  const reports = (await page.evaluate(() => window.__recon.lastStats))?.rateReports ?? 0;
  check('bitrate recovery: a congestion cut, then rate-controller raises back to the setting with overlapped restarts; overlay shows the target; rate reports flow',
    cuts.length >= 1 && raises.length >= 1 && raises.every(([a, b]) => b > a && b <= Math.floor(a * 1.25) + 1) &&
      (backAtTop || climbStarved) && cfg?.maxBitrate === 8000 &&
      (restarts['bitrate recovery'] || 0) === raises.length && !restarts['bitrate recovery (urgent)'] &&
      /of 8\.0 Mbps \(backed off\)/.test(target) && reports > 100,
    `${reports} rate reports; cuts ${cuts.map(([a, b]) => `${a}→${b}`).join(', ')}; raises ${raises.map(([a, b]) => `${a}→${b}`).join(', ')}; ` +
      `configs ${seen.join(' → ')} kbps (max ${cfg?.maxBitrate}${limit < 8000 ? `, decoder limit ${limit}` : ''}); overlay while backed off: "${target}"; restarts: ${counts(restarts)}; ` +
      `freezes > 100 ms: ${freezes.length ? freezes.join(', ') + ' ms' : 'none'}; ${idleNote(climbIdle)} during the climb` +
      `${backAtTop ? '' : climbStarved ? ' (not back at the setting: judged on its raises)' : ''}`);
  results.push({ bitrateRecovery: { cuts, raises, configs: seen, decoderLimit: limit < 8000 ? limit : null, restarts, freezes, reports } });
  await endStream();
}

// ---------------------------------------------------------------------------
// Datagram + FEC (guide step 2.5). The host's "fec" config "on" sends every
// frame as datagram shards with Reed-Solomon parity (web/static/js/fec.js
// rebuilds them), whatever the round trip; the test hook fec-loss=0.03 keeps
// 3 % of the shards (and repairs) from leaving the host, as a lossy network
// would. Every frame must arrive as shards and be rebuilt (from parity on
// arrival, or after a NACK from the host's fresh parity rows), with steady
// playback, the stage bookkeeping and the frame barcode intact; the host
// measures the client's shard loss (about 3 %) and logs the overhead. A
// WebSocket session of the same host never gets shards (no datagrams).

async function withHostConfig(extra, fn) {
  const cfgPath = join(dir, 'host.json');
  const orig = readFileSync(cfgPath, 'utf8');
  writeFileSync(cfgPath, JSON.stringify({ ...JSON.parse(orig), ...extra }));
  try {
    return await fn();
  } finally {
    writeFileSync(cfgPath, orig);
  }
}

// The overlay's per-session FEC counters, before -> after.
const fecDelta = (a, b) => Object.fromEntries(Object.keys(b || {}).map((k) => [k, (b?.[k] ?? 0) - (a?.[k] ?? 0)]));

// The last "stream stats" line's fields of a host log (key=value).
function lastStreamStats(log) {
  const line = (log.match(/msg="stream stats"[^\n]*/g) || []).pop() || '';
  return Object.fromEntries([...line.matchAll(/ (\w+)=("[^"]*"|\S+)/g)].map((m) => [m[1], m[2].replaceAll('"', '')]));
}

async function checkFec() {
  await withHostConfig({ fec: 'on' }, async () => {
    const host = await restartHost({ RECON_TEST_FAULTS: 'fec-loss=0.03' }, 'host-fec');
    await startStream({ path: 'auto', transport: 'auto' });
    const conn = await page.evaluate(() => window.__recon.conn);
    const calls = await watchDecoder();
    await sleep(4000); // decoder warm-up
    const st0 = await page.evaluate(() => window.__recon.lastStats);
    const c0 = cpuTimes();
    const dec0 = calls ? await calls() : null;
    const t0 = Date.now();
    const fps = [];
    for (const end = Date.now() + 12000; Date.now() < end;) {
      await sleep(500);
      fps.push(+((await page.evaluate(() => window.__recon.lastStats))?.fps ?? 0).toFixed(1));
    }
    const st = await page.evaluate(() => window.__recon.lastStats);
    // Where the CPUs had nothing to spare (2-vCPU runners), the rates are
    // judged against what reached the decoder in the same window (starvedRate).
    const secs = (Date.now() - t0) / 1000;
    const dec1 = calls ? await calls() : null;
    const tw = {
      decFps: dec0?.decodes != null && dec1?.decodes != null ? (dec1.decodes - dec0.decodes) / secs : null,
      supFps: st0?.superseded != null && st?.superseded != null ? (st.superseded - st0.superseded) / secs : null,
      idle: idleShare(c0, cpuTimes()),
    };
    const mean = fps.reduce((a, b) => a + b, 0) / Math.max(1, fps.length);
    const sr = starvedRate(mean, 60, 0.75, tw);
    const d = fecDelta(st0?.fec, st?.fec);
    const avail = /msg="video transport: datagram \+ FEC available"/.test(host.log);
    const on = /msg="video transport" .*?mode="datagram \+ FEC" why="on in host.json"/.test(host.log);
    const tail = fps.slice(-6);
    const steady = tail.reduce((a, b) => a + b, 0) / tail.length;
    // fecNow: the overlay's Transport row says "datagrams + FEC" while the
    // video comes as shards (a WebSocket session's below never does).
    check('datagram + FEC: every frame arrives as shards and is rebuilt (3 % of the shards lost)',
      conn.transport === 'webtransport' && conn.path === 'direct' && avail && on && d.rebuilt > 0 && st.fecNow === true &&
        d.lost <= 2 && d.bad === 0 && ((d.frames >= 12 * 45 && st.fps >= 45) || (sr.ok && d.frames >= 12 * 15)),
      `${conn.transport}/${conn.path}${st.fecNow ? ' (overlay: datagrams + FEC)' : ' (overlay: no datagrams + FEC)'}; in 12 s: ${d.frames} frames from ${d.shards} shards (${d.parity} parity, ${d.repairs} repairs), ` +
        `${d.rebuilt} rebuilt from parity, ${d.repaired} after a NACK (${d.nacks} NACKs, ${d.wholeNacks} whole), ${d.lost} given up, ` +
        `${d.unused} unused, ${d.bad} bad; shards lost ${d.shardsLost} (${(100 * d.shardsLost / Math.max(1, d.counted + d.shardsLost)).toFixed(2)} %); ` +
        `fps ${fps.join(' / ')}; host: available ${avail}, on ${on}${sr.note}`);
    const ssr = starvedRate(steady, 60, 5 / 6, tw);
    check('datagram + FEC: steady real-time playback', steady >= 50 || ssr.ok,
      `last 3 s: ${tail.join(' / ')} fps; stalls > 50 ms ${(st.stalls ?? 0) - (st0?.stalls ?? 0)}${ssr.note}`);
    await until(() => /fec_frames=[1-9]/.test(host.log), 12000, 'stream stats with fec_frames').catch(() => {});
    const ss = lastStreamStats(host.log);
    check('datagram + FEC: the host measures the client\'s shard loss and logs the overhead (stream stats)',
      +ss.fec_frames > 0 && +ss.fec_loss_pct >= 1.5 && +ss.fec_loss_pct <= 6 && ss.fec_overhead_pct !== undefined,
      `fec_frames=${ss.fec_frames} fec_parity_pct=${ss.fec_parity_pct} fec_overhead_pct=${ss.fec_overhead_pct} fec_loss_pct=${ss.fec_loss_pct} ` +
        `fec_nacks=${ss.fec_nacks} fec_repairs=${ss.fec_repairs} kbps_target=${ss.kbps_target} mbps=${ss.mbps}`);
    await checkStages('datagram + FEC', st);
    await checkHygiene('datagram + FEC', calls, st);
    await checkProbe('datagram + FEC');
    results.push({ fec: { conn, delta: d, fps, streamStats: ss, idle: tw.idle } });
    await endStream();

    // WebSocket: never shards.
    const log0 = host.log.length;
    await startStream({ path: 'relay', transport: 'websocket' });
    const wc0 = cpuTimes();
    await sleep(4000);
    const ws = await page.evaluate(() => window.__recon.lastStats);
    const wsIdle = idleShare(wc0, cpuTimes());
    const why = (host.log.slice(log0).match(/msg="video transport: frame streams only".*? why="[^"]*"/) || [''])[0];
    check('datagram + FEC: a WebSocket session gets frame streams only',
      !ws.fec && !ws.fecNow && (ws.fps > 30 || (wsIdle != null && wsIdle < STARVED_IDLE && ws.fps > 10)) && /does not take shards/.test(why),
      `${ws.fps.toFixed(1)} fps, shards ${ws.fec ? ws.fec.shards : 0}; host: ${why.replace(/^msg=/, '')}; ${idleNote(wsIdle)}`);
    await endStream();
  });
  await restartHost({}, 'host');
}

// A UDP proxy in front of the host's direct port that delays each datagram
// delayMs and loses a share `loss` of them, each direction (netem's wan
// profile in user space: the sandbox kernel has no sch_netem). The browser
// is sent to it by rewriting the direct URL of the connect answer; its port
// is one of the gateway's relay ports, held, so the page's CSP allows it and
// the gateway skips it.
function impairProxy(sock, targetPort) {
  const up = dgram.createSocket('udp4');
  up.bind(0, '127.0.0.1');
  const st = { delayMs: 0, loss: 0, c2h: 0, h2c: 0, lostC2h: 0, lostH2c: 0 };
  let client = null;
  sock.on('message', (m, r) => {
    client = r;
    st.c2h++;
    if (Math.random() < st.loss) { st.lostC2h++; return; }
    setTimeout(() => up.send(m, targetPort, '127.0.0.1'), st.delayMs);
  });
  up.on('message', (m) => {
    st.h2c++;
    if (!client) return;
    if (Math.random() < st.loss) { st.lostH2c++; return; }
    const c = client;
    setTimeout(() => sock.send(m, c.port, c.address), st.delayMs);
  });
  up.unref();
  return st;
}

// Datagram + FEC against frame streams (E2E_FEC_COMPARE=<seconds per run>):
// through impairProxy at 40 ms RTT with 1 % and 3 % loss each way, a direct
// session per mode (host fec "off": frame streams; "auto": datagram + FEC
// above 15 ms RTT), measured after a warm-up: the client's stalls over 50 ms
// (the picture standing still that much longer than the source) and freezes
// over 100 ms, frames per second, key frame requests, the bitrate, and the
// host's FEC stream stats (overhead: parity, headers and repairs over the
// frames' bytes). The checked pairs stream a fixed 20 Mbit/s (adaptive
// bitrate off: both modes carry the same frames, the rate controller's
// reactions to the loss do not differ between runs; a frame-queue overflow
// still cuts it, so each run records its target's range) and run
// E2E_FEC_COMPARE_RUNS times per mode (default 3), the modes interleaved so
// a change of this shared machine's load hits both; the check compares the
// medians (one 30 s run's stall count is within the run-to-run spread). A
// pair at 3 % with the rate controller on runs once. Each run's host log is
// saved (host-fec-<mode>-<loss>-<run>.log in the results), and a FEC run
// records why frames were given up: the client's log of each (shards short,
// NACKs, repairs that came), the host's stopped frames (rung 1), NACKs for
// frames it did not keep and repairs the budget refused.
async function compareFec(seconds) {
  const runs = Math.max(1, Number(process.env.E2E_FEC_COMPARE_RUNS) || 3);
  const rows = [];
  const rewrite = async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    if (body.direct?.url) body.direct.url = body.direct.url.replace(/:\d+\/wt$/, `:${fecProxyPort}/wt`);
    await route.fulfill({ response: resp, json: body });
  };
  await ctx.route('**/api/hosts/*/connect', rewrite);
  try {
    for (const [loss, adaptive] of [[0.01, false], [0.03, false], [0.03, true]]) {
      for (let run = 1; run <= (adaptive ? 1 : runs); run++) {
        for (const mode of ['off', 'auto']) {
          await withHostConfig({ fec: mode }, async () => {
            const name = `host-fec-${mode}-${loss * 100}pct${adaptive ? '-adaptive' : ''}-${run}`;
            const host = await restartHost({}, name);
            Object.assign(fecImpair, { delayMs: 20, loss });
            await startStream({ path: 'direct', transport: 'auto', bitrate: 20, adaptive });
            await sleep(8000); // warm-up, and the client's minimum RTT reaches the host
            const st0 = await page.evaluate(() => window.__recon.lastStats);
            const log0 = host.log.length;
            const con0 = consoleLines.length;
            const fps = [];
            const mbps = [];
            for (const end = Date.now() + seconds * 1000; Date.now() < end;) {
              await sleep(1000);
              const x = await page.evaluate(() => window.__recon.lastStats);
              fps.push(x?.fps ?? 0);
              mbps.push(x?.mbps ?? 0);
            }
            const st = await page.evaluate(() => window.__recon.lastStats);
            const hl = host.log.slice(log0);
            writeFileSync(join(outDir, `${name}.log`), host.log);
            const ss = [...hl.matchAll(/msg="stream stats"[^\n]*/g)].map((m) => lastStreamStats(m[0]));
            const avg = (v) => v.reduce((a, b) => a + b, 0) / Math.max(1, v.length);
            const sum = (v) => v.reduce((a, b) => a + b, 0);
            const num = (k) => ss.map((x) => +x[k]).filter((v) => Number.isFinite(v));
            const targets = num('kbps_target');
            const row = {
              mode: mode === 'off' ? 'frame streams' : 'datagram + FEC', loss, adaptive, seconds, run,
              stalls50: (st.stalls ?? 0) - (st0.stalls ?? 0), freezes100: st.freezes - st0.freezes,
              fps: +avg(fps).toFixed(1), mbps: +avg(mbps).toFixed(1), keyRequests: st.keyRequests - st0.keyRequests,
              fec: fecDelta(st0.fec, st.fec), overheadPct: +avg(num('fec_overhead_pct')).toFixed(1), parityPct: +avg(num('fec_parity_pct')).toFixed(1),
              shardLossPct: +avg(num('fec_loss_pct')).toFixed(2), kbpsTarget: Math.round(avg(targets)),
              kbpsTargetMin: targets.length ? Math.min(...targets) : null, kbpsTargetMax: targets.length ? Math.max(...targets) : null,
              lossPct: +avg(num('loss_pct')).toFixed(2), rttMs: st.minRtt, proxy: { ...fecImpair },
              deadlineDrops: sum(num('deadline_drops')), shardsStopped: (hl.match(/msg="frame shards stopped"/g) || []).length,
              nackMisses: sum(num('fec_nack_misses')), repairRefused: sum(num('fec_repair_refused')),
              givenUp: consoleLines.slice(con0).filter((l) => /lost: its shards could not be rebuilt/.test(l)).map((l) => l.replace(/^.*?frame /, 'frame ')),
            };
            rows.push(row);
            console.log(`  FEC compare: ${row.mode} at ${loss * 100} % loss, 40 ms RTT, ${adaptive ? 'adaptive bitrate' : 'fixed 20 Mbit/s'}, run ${run}, ${seconds} s: ` +
              `stalls > 50 ms ${row.stalls50}, freezes > 100 ms ${row.freezes100}, ${row.fps} fps, ${row.mbps} Mbit/s ` +
              `(target ${row.kbpsTarget}, ${row.kbpsTargetMin}-${row.kbpsTargetMax}), key requests ${row.keyRequests}, deadline drops ${row.deadlineDrops}` +
              (mode === 'off' ? '' : `, overhead ${row.overheadPct} % (parity ${row.parityPct} % of data shards, shard loss ${row.shardLossPct} %), ` +
                `frames rebuilt ${row.fec.rebuilt ?? 0}, repaired ${row.fec.repaired ?? 0} (${row.fec.nacks ?? 0} NACKs, ${row.fec.wholeNacks ?? 0} whole), ` +
                `lost ${row.fec.lost ?? 0}; host: shards stopped ${row.shardsStopped}, NACK misses ${row.nackMisses}, repairs refused ${row.repairRefused}` +
                (row.givenUp.length ? `; given up: ${row.givenUp.join(' | ')}` : '')));
            await page.evaluate(() => { window.__recon.userClosed = true; });
            await sleep(500);
          });
        }
      }
    }
  } finally {
    Object.assign(fecImpair, { delayMs: 0, loss: 0 });
    await ctx.unroute('**/api/hosts/*/connect', rewrite);
    await restartHost({}, 'host');
  }
  results.push({ fecCompare: rows });
  const median = (v) => { const a = [...v].sort((x, y) => x - y); return a.length % 2 ? a[(a.length - 1) / 2] : (a[a.length / 2 - 1] + a[a.length / 2]) / 2; };
  for (const loss of [0.01, 0.03]) {
    const pair = rows.filter((r) => r.loss === loss && !r.adaptive);
    const [str, dg] = [pair.filter((r) => r.mode === 'frame streams'), pair.filter((r) => r.mode !== 'frame streams')];
    const col = (v, k) => v.map((r) => r[k]);
    check(`FEC compare at ${loss * 100} % loss, 40 ms RTT, 20 Mbit/s: fewer stalls > 50 ms with datagram + FEC (median of ${runs} runs each), overhead <= 15 %`,
      median(col(dg, 'stalls50')) < median(col(str, 'stalls50')) && Math.max(...col(dg, 'overheadPct')) <= 15 && dg.every((r) => r.fec.frames > 0),
      `stalls > 50 ms: streams ${col(str, 'stalls50').join('/')} (median ${median(col(str, 'stalls50'))}), FEC ${col(dg, 'stalls50').join('/')} ` +
        `(median ${median(col(dg, 'stalls50'))}); freezes > 100 ms ${col(str, 'freezes100').join('/')} vs ${col(dg, 'freezes100').join('/')}; ` +
        `fps ${col(str, 'fps').join('/')} vs ${col(dg, 'fps').join('/')}; streams' target ${str.map((r) => `${r.kbpsTargetMin}-${r.kbpsTargetMax}`).join(', ')} kbps; ` +
        `FEC overhead ${col(dg, 'overheadPct').join('/')} % (parity ${col(dg, 'parityPct').join('/')} %, shard loss ${col(dg, 'shardLossPct').join('/')} %), ` +
        `given up ${dg.map((r) => r.fec.lost ?? 0).join('/')}`);
  }
}

// ---------------------------------------------------------------------------

const dir = mkdtempSync(join(tmpdir(), 'recon-e2e-'));
const port = await freePort();
const directPort = await freePort();
const base = `https://127.0.0.1:${port}`;
const inputLog = join(dir, 'input.log');

// UDP relay ports (one per relayed session). One of them is held by a socket
// that swallows every datagram, like a firewall that drops the relay ports:
// the gateway skips it (in use), the page's CSP allows it.
const relayPorts = [await freeUdpPort(), await freeUdpPort(), await freeUdpPort()];
const blackhole = dgram.createSocket('udp4');
await new Promise((res) => blackhole.bind(0, '127.0.0.1', res));
const blockedPort = blackhole.address().port;
blackhole.unref();
process.on('exit', () => { try { blackhole.close(); } catch {} });
// The FEC comparison's impairment proxy (impairProxy), on a held relay port.
const fecSock = dgram.createSocket('udp4');
await new Promise((res) => fecSock.bind(0, '127.0.0.1', res));
const fecProxyPort = fecSock.address().port;
fecSock.unref();
const fecImpair = impairProxy(fecSock, directPort);
process.on('exit', () => { try { fecSock.close(); } catch {} });
const gw = run('recon-gateway', ['-listen', `127.0.0.1:${port}`, '-data', join(dir, 'gw'), '-relay-ports', [...relayPorts, blockedPort, fecProxyPort].join(',')], {}, 'gateway');
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
// The CPUs' times every 0.5 s (cpuTimes), for the idle share in the 2 s
// before a "frame lost" key-frame request, which onConsole appends to the
// line (starvedKeyRequest).
const cpuHist = [];
const cpuTimer = setInterval(() => {
  const c = cpuTimes();
  if (c) cpuHist.push([Date.now(), c]);
  if (cpuHist.length > 8) cpuHist.shift();
}, 500);
cpuTimer.unref();
// The page's own log since its last drawn frame says whether it was waiting
// for a key frame (also where instrumentWorker missed the worker): a key-frame
// request or the watchdog's "still waiting" with no frame drawn after it (a
// frame drawn after a wait logs its freeze; a new stream its padded picture).
const pageLogs = new WeakMap();
function waitingInLog(recent) {
  let again = 0;
  for (let i = recent.length - 1; i >= 0; i--) {
    const l = recent[i];
    if (/freeze: \d+ ms longer than the source|padded picture:/.test(l)) return null;
    if (l.includes('still waiting for a key frame')) again++;
    else if (l.includes('requesting key frame (')) return again;
  }
  return again || null;
}
const onConsole = function (m) {
  let text = m.text();
  const recent = pageLogs.get(this) || [];
  if (text.includes('requesting key frame (frame lost)')) {
    const again = waitingInLog(recent);
    if (again !== null) text += ` [log: waiting for a key frame, asked again ${again} times by the watchdog]`;
    if (cpuHist.length) {
      const t = Date.now();
      const from = cpuHist.find(([at]) => at >= t - 2000) || cpuHist[0];
      const idle = idleShare(from[1], cpuTimes());
      if (idle != null) text += ` [cpu: ${(100 * idle).toFixed(0)} % idle in the last ${((t - from[0]) / 1000).toFixed(1)} s]`;
    }
  }
  recent.push(text);
  if (recent.length > 300) recent.splice(0, 100);
  pageLogs.set(this, recent);
  consoleLines.push(`[${m.type()}] ${text}`);
  if (process.env.E2E_VERBOSE) console.log('[page]', text);
};
const onPageError = (e) => consoleLines.push(`[pageerror] ${e.message}`);
// Which request the gateway refused (the console names none): a 401 sends
// the app to the login page.
const onResponse = (r) => { if (r.status() === 401) consoleLines.push(`[401] ${r.request().method()} ${r.url().replace(/([?&]t=)[^&]+/, '$1…')}`); };
page.on('console', onConsole.bind(page));
page.on('pageerror', onPageError);
page.on('response', onResponse);
page.on('worker', instrumentWorker);

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

  // The login page's ?next= (signed in: it redirects at once) stays on the
  // gateway: the URL parser reads a backslash as '/' and drops tabs, so
  // '/\evil.example' and '/<TAB>/evil.example' name another host.
  if (want('login redirect')) {
    const offsite = [];
    const evil = (u) => u.hostname === 'evil.example';
    await page.route(evil, (r) => { offsite.push(r.request().url()); return r.abort(); });
    for (const [q, dest] of [['/%5Cevil.example', '/'], ['/%09/evil.example%2Fx', '/'], ['//evil.example', '/'], ['/%3Fe2e%3D1%23top', '/?e2e=1#top']]) {
      offsite.length = 0;
      // 'commit': the redirect can come before the login page's load event.
      const at = await page.goto(`${base}/login?next=${q}`, { waitUntil: 'commit' })
        .then(() => until(async () => offsite[0] || (page.url().startsWith(`${base}/login`) ? null : page.url()), 15000, `the redirect for next=${q}`))
        .catch((e) => offsite[0] || e.message);
      check(`login ?next=${q} stays on the gateway`, at === `${base}${dest}` && !offsite.length, `went to ${at}`);
    }
    await page.unroute(evil);
    await page.goto(`${base}/`);
  }

  // 2. Add a host and pair the agent ------------------------------------------
  await page.click('#add-host');
  await page.fill('.modal input', 'E2E Test PC');
  await page.click('.modal .btn-primary');
  const cmd = await (await page.waitForSelector('.modal .code-box')).textContent();
  const code = cmd.match(/"(recon1:[^"]+)"/)[1];
  check('pairing code issued', code.startsWith('recon1:'));

  const cfgPath = join(dir, 'host.json');
  // fec "off": the scenarios check frame streams (the loss-recovery ladder's
  // cancels, partial delivery, the video window); "auto" would switch a
  // session to datagram shards once the client's minimum round trip passed
  // 15 ms, which a CPU-starved loopback (2-vCPU runners) can show. The
  // datagram + FEC scenario turns it on (checkFec).
  const hostCfg = { capture: 'test', testWidth: 960, testHeight: 540, testPad: TEST_PAD, directPort, directAddr: '127.0.0.1', audio: true, logLevel: 'debug', hdr: 'auto', fec: 'off' };
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
    // UDP relay (step 2.6): one QUIC connection with the host through a gateway relay port.
    { name: 'WebTransport relay', prefs: { path: 'relay', transport: 'auto', renderer: 'canvas2d' }, expect: ['webtransport', 'relay'], gatewayLog: /msg="udp relay: session started"/ },
    // The relay port is unreachable (firewall): the client falls back to the QUIC splice on the main port.
    { name: 'WebTransport relay fallback (splice)', prefs: { path: 'relay', transport: 'auto', renderer: 'canvas2d' }, expect: ['webtransport', 'relay-splice'], blockUdpRelay: true },
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
    // HDR10 (checkHdrStream): the headed page plays an HDR display, AV1 (the
    // test pattern's HDR encoder is libsvtav1), HDR_STREAM asked for live like
    // the upscaling scenario's size; the barcode is read from the copied planes.
    { name: 'WebGPU HDR10', prefs: { path: 'auto', transport: 'auto', renderer: 'webgpu', fps: HDR_STREAM.fps, codec: 'av1' }, expect: ['webtransport', 'direct'], probe: 'copyTo I420P10 (HDR planes)', headed: true, size: [HDR_STREAM.w, HDR_STREAM.h], hdr: true },
  ].filter((sc) => want(sc.name));
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
  await watchDecoder();
  await sleep(6000);
  const warmStart = Date.now();
  let warm = await page.evaluate(() => window.__recon.lastStats);
  while ((!warm || warm.fps < 30) && Date.now() - warmStart < 10000) {
    await sleep(500);
    warm = await page.evaluate(() => window.__recon.lastStats);
  }
  const ww = warm && warm.fps >= 30 ? null : await rateWindow(2000);
  const wr = ww ? starvedRate(ww.fps, 60, 0.5, ww) : { ok: false, note: '' };
  check('warm-up stream (self-heals if the decoder falls behind)', (warm && warm.fps >= 30) || wr.ok,
    `${warm?.fps.toFixed(1)} fps, ${warm?.keyRequests} recovery key frames, waited ${((Date.now() - warmStart) / 1000).toFixed(1)} s extra` +
      `${ww ? `; then ${ww.fps.toFixed(1)} fps over 2 s${wr.note}` : ''}`);
  await endStream();
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
    const scC0 = cpuTimes(); // the stream's whole life (its cumulative counters)
    await page.goto(`${base}/`);
    await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify({ stats: true, ...p })), { ...PREFS_2D, ...sc.prefs });
    await page.evaluate((on) => (on ? localStorage.setItem('e2e.hdrDisplay', '1') : localStorage.removeItem('e2e.hdrDisplay')), !!sc.hdr);
    // The gateway allocates a relay port and the host binds to it, but the
    // browser is sent to the port whose datagrams are dropped.
    const blockRoute = async (route) => {
      const resp = await route.fetch();
      const body = await resp.json();
      if (body.url) body.url = body.url.replace(/:\d+\/wt$/, `:${blockedPort}/wt`);
      await route.fulfill({ response: resp, json: body });
    };
    if (sc.blockUdpRelay) await ctx.route('**/api/relay/udp*', blockRoute);
    const gwLog0 = gw.log.length;
    const con0 = consoleLines.length;
    const hostRun = procs.find((p) => p.spawnargs.includes('run'));
    const hostLog0 = hostRun.log.length;
    await page.click('.host.online a.btn-primary');
    await page.waitForURL(/\/stream\?host=/);
    await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
    const t0 = Date.now();
    await page.click('#btn-start');
    await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
    const firstFrameMs = Date.now() - t0;
    if (sc.blockUdpRelay) await ctx.unroute('**/api/relay/udp*', blockRoute);
    const conn = await page.evaluate(() => window.__recon.conn);
    check(`${sc.name}: connected`, conn.transport === sc.expect[0] && conn.path === sc.expect[1] && conn.renderer === sc.prefs.renderer,
      `${conn.transport}/${conn.path}, renderer ${conn.renderer}, first frame after ${firstFrameMs} ms`);
    if (conn.transport === 'webtransport') {
      // GUIDE 2.4: the host logs per session whether the browser's QUIC
      // endpoint negotiated RESET_STREAM_AT (partial delivery: a cancelled
      // frame stream still delivers its header); on the splice its peer is
      // the gateway ("n/a"). Recorded for this browser.
      const line = (hostRun.log.slice(hostLog0).match(/msg="session started"[^\n]*/g) || []).pop() || '';
      const rsa = (line.match(/ reset_stream_at=(\S+)/) || [])[1];
      const expected = conn.path === 'relay-splice' ? ['n/a'] : ['yes', 'no'];
      check(`${sc.name}: the host records whether the browser negotiated RESET_STREAM_AT (partial delivery)`, expected.includes(rsa),
        `Chromium ${browser.version()}: reset_stream_at=${rsa}`);
      results.push({ resetStreamAt: { scenario: sc.name, path: conn.path, value: rsa ?? null, browser: browser.version() } });
    }
    if (sc.blockUdpRelay) {
      // The allocation worked; the WebTransport connection to the port did not.
      const why = consoleLines.slice(con0).find((l) => l.includes('relay failed:')) || '';
      check(`${sc.name}: the UDP relay was tried first and its port did not answer`,
        /\brelay failed: WebTransport relay timed out/.test(why), why.replace(/^.*?relay failed/, 'relay failed').slice(0, 200));
    }
    if (sc.gatewayLog) {
      const line = (gw.log.slice(gwLog0).match(new RegExp(`${sc.gatewayLog.source}[^\n]*`)) || [''])[0];
      check(`${sc.name}: the gateway forwards the session's datagrams`, !!line, line.replace(/^.*?msg=/, '').slice(0, 200));
    }
    if (sc.size) {
      // The stream's size, asked for live (Settings has no such resolution): a new encoder generation.
      const prefs = { codec: sc.prefs.codec || 'auto', bitrate: 30000, fps: sc.prefs.fps, width: sc.size[0], height: sc.size[1], monitor: 0, audio: true, audioCodec: 'opus', cursor: 'local', quality: 'balanced', adaptive: true };
      await page.evaluate((m) => window.__recon.worker.postMessage({ type: 'ctl', m }), { t: 'settings', prefs });
      const sized = await until(() => page.evaluate((w) => window.__recon.video.w === w, sc.size[0]), 15000, `the stream at ${sc.size.join('x')}`).catch(() => false);
      check(`${sc.name}: the stream restarts at ${sc.size.join('x')} (live settings change)`, sized, JSON.stringify(await page.evaluate(() => window.__recon.video)));
    }
    const calls = await watchDecoder();

    // Wait for steady state (software decoders need a moment to warm up on
    // small CI machines), then measure a fresh stats window.
    const settleStart = Date.now();
    const timeline = [];
    const w0 = streamWorker();
    const decAt = async () => (w0 ? w0.evaluate(() => [performance.now(), self.__decoderCalls?.decodes ?? null]).catch(() => null) : null);
    let tail0 = null;
    for (let i = 0; i < 16; i++) {
      await sleep(500);
      const x = await page.evaluate(() => window.__recon.lastStats);
      if (x) timeline.push({ t: Date.now() - settleStart, fps: +x.fps.toFixed(1), decode: x.decode && +x.decode.toFixed(1), total: x.total && +x.total.toFixed(1), q: x.queue, mbps: +x.mbps.toFixed(1), keyReq: x.keyRequests, thinned: x.thinned ?? 0 });
      if (i === 12) tail0 = { d: await decAt(), c: cpuTimes(), s: x?.superseded ?? null }; // the last 1.5 s start here
    }
    const sup1 = await page.evaluate(() => window.__recon.lastStats?.superseded ?? null).catch(() => null);
    const tail1 = { d: await decAt(), c: cpuTimes() };
    const tailS = tail0?.d && tail1.d && tail1.d[0] > tail0.d[0] ? (tail1.d[0] - tail0.d[0]) / 1000 : null;
    const tw = {
      decFps: tail0?.d?.[1] != null && tail1.d?.[1] != null && tailS ? (tail1.d[1] - tail0.d[1]) / tailS : null,
      supFps: tail0?.s != null && sup1 !== null && tailS ? (sup1 - tail0.s) / tailS : null,
      idle: idleShare(tail0?.c, tail1.c),
    };
    results.push({ timeline: sc.name, points: timeline });
    // Steady state = the last 1.5 s of the 8 s window all at real-time rate.
    const tail = timeline.slice(-3);
    // Frames the host left out on purpose (temporal SVC thinning: its answer to
    // a delay, a frame queue or a slow stream, which this CPU-only machine's own
    // load produces too) are no failure to play in real time: their rate over
    // the window counts with the frames drawn.
    const before = timeline[timeline.length - 4]; // the sample before the window
    const thinRate = before && tail.length === 3 ? ((tail[2].thinned - before.thinned) * 1000) / Math.max(1, tail[2].t - before.t) : 0;
    const thinNote = thinRate > 0 ? ` + ${thinRate.toFixed(1)} thinned/s` : '';
    const drawn = tail.reduce((a, p) => a + p.fps, 0) / Math.max(1, tail.length);
    const avg = drawn + thinRate;
    const rate = sc.prefs.fps || 60; // the stream's frame rate
    // (per-0.5 s samples jitter when frames bunch at a boundary). Where the
    // CPUs had nothing to spare, three quarters of what reached the decoder
    // (starvedRate: frames drawn or superseded; outputs come in bursts;
    // thinned frames never reach it).
    const sr = starvedRate(drawn, rate, 0.75, tw);
    const steady = tail.length === 3 && (avg >= (rate * 5) / 6 || sr.ok);
    const st = await page.evaluate(() => window.__recon.lastStats);
    check(`${sc.name}: steady real-time playback`, steady, `last 1.5 s: ${tail.map((p) => p.fps).join(' / ')} fps${thinNote}; key requests ${st?.keyRequests}${sr.note}`);
    const cfg = await page.evaluate(() => window.__recon.videoCfg);
    const dr = starvedRate(drawn, rate, 0.75, tw); // the window's drawn rate (st.fps is its last 0.5 s)
    check(`${sc.name}: video decoding`, st && (st.fps + thinRate > rate * 0.75 || dr.ok),
      `${st?.fps.toFixed(1)} fps${thinNote} of ${rate}, ${st?.mbps.toFixed(2)} Mbps, codec ${cfg?.codec} via ${cfg?.encoder}${dr.note}`);
    check(`${sc.name}: latency measured`, st && st.synced && st.total !== null,
      `stream ${st?.total?.toFixed(1)} ms (network ${st?.owd?.toFixed(2)} ms, decode ${st?.decode?.toFixed(2)} ms, RTT ${st?.rtt?.toFixed(2)} ms)`);
    // GUIDE 2.2: the welcome asks for rate reports; the worker sends one
    // every 25 ms on every path (datagrams; WebSocket: channel messages).
    check(`${sc.name}: rate reports to the host's rate controller`, st && st.rateReports > 100, `${st?.rateReports} sent`);
    // GUIDE 2.7: the send priorities this browser schedules by, detected as
    // its API has them, and telemetry that gives way to input on a shared
    // datagram queue only while that queue stands still (Chromium's datagram
    // writes stall for 50 ms and more now and then on this CPU-bound machine:
    // at most 15 %, and drops only with such a stall; a first version that
    // capped pending writes at two dropped 28 %).
    if (conn.transport === 'webtransport') {
      const api = await page.evaluate(() => ({
        sendGroup: 'createSendGroup' in WebTransport.prototype,
        datagramWritables: typeof WebTransportDatagramDuplexStream !== 'undefined' && 'createWritable' in WebTransportDatagramDuplexStream.prototype,
        sendOrder: typeof WebTransportSendStream !== 'undefined' && 'sendOrder' in WebTransportSendStream.prototype,
      }));
      const p = st?.prio;
      const total = p ? p.telemetrySent + p.telemetryDropped : 0;
      // Where the CPUs had nothing to spare during the stream (2-vCPU
      // runners), the worker runs the writes' resolutions late, so the queue
      // seems to stand still far more often: the share dropped is no measure
      // then. Still required: drops only with a stall, and telemetry flowing.
      const scIdle = idleShare(scC0, cpuTimes());
      const prioStarved = scIdle != null && scIdle < STARVED_IDLE;
      // A write's resolution waits while the worker's event loop is busy:
      // the queue then seems to stand still and telemetry due meanwhile is
      // dropped (instrumentWorker: the share of the stream's time the
      // worker held its ticker more than 50 ms late). Allowed on top of the
      // 15 %: that share of the telemetry.
      const wb = await streamWorker()?.evaluate(() => { const k = self.__e2eKey; return k ? { blocked: k.blockedMs, secs: (performance.now() - k.since) / 1000 } : null; }).catch(() => null);
      const blockedShare = wb && wb.secs > 0 ? Math.min(1, wb.blocked / 1000 / wb.secs) : 0;
      check(`${sc.name}: send priorities feature-detected; telemetry gives way to input only while the datagram queue stalls`,
        !!p && p.sendOrder === api.sendOrder && p.sendGroup === api.sendGroup && p.datagramWritables === api.datagramWritables &&
          total > 100 && (p.telemetryDropped <= total * (0.15 + blockedShare) || (prioStarved && p.telemetrySent > 100)) &&
          (p.telemetryDropped === 0 || p.telemetryStallMs > 50),
        p ? `sendOrder ${p.sendOrder}, send groups ${p.sendGroup}, datagram queues ${p.datagramWritables} (browser API: ${JSON.stringify(api)}); ` +
          `telemetry ${p.telemetrySent} sent, ${p.telemetryDropped} dropped, longest stall ${p.telemetryStallMs} ms; ` +
          `worker event loop more than 50 ms late for ${wb ? `${(wb.blocked / 1000).toFixed(1)} s of ${wb.secs.toFixed(0)} s (${(100 * blockedShare).toFixed(0)} %)` : '? (not instrumented)'}; ` +
          `${idleNote(scIdle)} during the stream` +
          `${prioStarved && p.telemetryDropped > total * (0.15 + blockedShare) ? ' (judged on the stalls)' : ''}` : 'no prio in the stats');
    } else {
      check(`${sc.name}: no send priorities over WebSocket`, st && st.prio === null);
    }
    await checkStages(sc.name, st);
    await checkCrop(sc.name);
    await checkPresentation(sc.name, sc.prefs.renderer);
    await checkHygiene(sc.name, calls, st);
    // (The codec choice's reason is the automatic one's: E2E_ONLY can make a scenario with a codec setting the first.)
    if (sc === scenarios[0] && !sc.prefs.codec) await checkSelfTest(cfg);
    const pr = await checkProbe(sc.name, sc.probe, rate);
    // (Fullscreen is the WebGPU scenario's; the upscaling one has no more to show there.)
    if (sc.name === 'WebTransport direct' || (sc.probe && !sc.upscale && !sc.hdr)) await checkFullscreen(sc.name);
    if (sc.upscale) await checkUpscaleStream(sc, rate).catch((e) => check(`${sc.name}: upscaling`, false, e.message));
    if (sc.hdr) await checkHdrStream(sc).catch((e) => check(`${sc.name}: HDR10`, false, e.message));
    check(`${sc.name}: audio`, st && st.audioPackets > 50, `${st?.audioPackets} packets/0.5 s window cumulative, buffer ${st?.audioMs?.toFixed(0)} ms, lost ${st?.audioLost}`);
    if (sc === scenarios[0]) await checkAudio(sc.name);
    results.push({ scenario: sc.name, stats: st, firstFrameMs, conn, cfg });

    // Input: keyboard + mouse (desktop mode = absolute) + wheel, in the middle
    // of the stage. Toasts (e.g. the rate controller's congestion notice) sit
    // at the bottom right with pointer events, and in the headed browser's
    // 768x432 viewport (HEADED_VIEWPORT) the old fixed points (640, 360) ->
    // (700, 400) fell on them: the host then got no mouse events.
    const vp = page.viewportSize() || { width: 1280, height: 720 };
    const [ix0, iy0, ix1, iy1] = [vp.width * 0.5, vp.height * 0.5, vp.width * 0.55, vp.height * 0.55].map(Math.round);
    const under = await page.evaluate(([x, y]) => {
      const e = document.elementFromPoint(x, y);
      return e ? `${e.tagName.toLowerCase()}${e.id ? `#${e.id}` : ''}${e.parentElement?.id ? ` in #${e.parentElement.id}` : ''}` : 'nothing';
    }, [ix1, iy1]);
    await page.mouse.move(ix0, iy0);
    await page.mouse.move(ix1, iy1, { steps: 5 });
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
      if (sc.name !== 'WebTransport direct') { await endStream(); continue; }
    }
    const events = nativeInputLog ? await until(() => {
      const lines = readFileSync(inputLog, 'utf8').trim().split('\n').filter(Boolean).map((l) => JSON.parse(l));
      const keys = lines.filter((e) => e.ev === 'key');
      const ok = lines.some((e) => e.ev === 'abs') && lines.some((e) => e.ev === 'button' && e.down) &&
        lines.some((e) => e.ev === 'wheel') && keys.some((e) => e.sc === 0x11 && e.down) && keys.some((e) => e.sc === 0x48 && e.ext);
      return ok ? lines : null;
    }, 5000, 'input events on host').catch((e) => { check(`${sc.name}: input`, false, `${e.message} (pointer over ${under})`); return null; }) : null;
    if (events) {
      const abs = events.filter((e) => e.ev === 'abs').pop();
      check(`${sc.name}: keyboard + mouse reach the host`, true, `${events.length} events; last abs (${abs.x}, ${abs.y}); W=0x11, ArrowUp=E0 48; pointer over ${under}`);
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
      const lw = await rateWindow(1500);
      const st2 = await page.evaluate(() => window.__recon.lastStats);
      const lr = starvedRate(st2.fps, 60, 2 / 3, lw);
      check('live settings change (new encoder generation, stream continues)', st2.fps > 40 || lr.ok, `${st2.fps.toFixed(1)} fps after switch${lr.note}`);
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
    await page.evaluate(() => localStorage.removeItem('e2e.hdrDisplay')).catch(() => {});
    await endStream();
  }
  page = mainPage;

  // 3a. Renderer "auto": the presentation bake-off -----------------------------
  if (want('bake-off')) await checkBakeoff().catch((e) => check('renderer auto (bake-off) scenario', false, e.message));
  await closeHeaded();

  if (want('udp relay host blocked')) await checkUdpRelayHostBlocked().catch((e) => check('UDP relay, host cannot bind', false, e.message));

  // 3b. Loss handling with the host's fault-injection hook --------------------
  if (want('loss handling')) await checkLossHandling().catch((e) => check('loss handling scenario', false, e.message));
  if (want('thinning')) await checkThinning().catch((e) => check('temporal SVC thinning scenario', false, e.message));
  if (want('bitrate recovery')) await checkBitrateRecovery().catch((e) => check('bitrate recovery scenario', false, e.message));
  // 3b'. Datagram + FEC (step 2.5) ------------------------------------------------
  if (want('fec')) await checkFec().catch((e) => check('datagram + FEC scenario', false, e.message));
  if (Number(process.env.E2E_FEC_COMPARE) > 0 && want('fec comparison')) {
    await compareFec(Number(process.env.E2E_FEC_COMPARE)).catch((e) => check('FEC comparison', false, e.message));
  }
  if (want('pre stage hold host')) await checkPreStageHoldHost().catch((e) => check('host before step 4.4 scenario', false, e.message));
  if (want('input host')) await checkInputHost().catch((e) => check('input (rumble, keyboard lock) scenario', false, e.message));
  if (want('takeover')) await checkTakeover().catch((e) => check('takeover scenario', false, e.message));

  // 3c. Renderers (unit) ---------------------------------------------------------
  const xvfbOk = spawnSync('sh', ['-c', 'command -v Xvfb']).status === 0;
  if (want('renderer crop unit')) await checkRendererCrop(xvfbOk).catch((e) => check('renderer crop (unit)', false, e.message));
  if (want('fsr unit')) await checkUpscaleUnit(xvfbOk).catch((e) => check('FSR 1 shader (unit)', false, e.message));
  if (want('hdr unit')) await checkHdrUnit(xvfbOk).catch((e) => check('HDR shader (unit)', false, e.message));
  if (want('pick rule unit')) await checkPickRule().catch((e) => check("Auto's pick (unit)", false, e.message));
  if (want('pacer unit')) await checkPacerRule().catch((e) => check('frame pacing (unit)', false, e.message));
  if (want('jitter unit')) await checkJitterRule().catch((e) => check('jitter buffer (unit)', false, e.message));
  if (want('self-test logic unit')) await checkSelfTestLogic().catch((e) => check('decoder self-test logic (unit)', false, e.message));
  if (want('send priorities unit')) await checkSendPriorities().catch((e) => check('send priorities (unit)', false, e.message));

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
  if (process.env.E2E_WALLCLOCK === '0' || hostBin || !haveX || !want('wallclock')) {
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
