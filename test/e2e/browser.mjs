// End-to-end test: real gateway + real host agent + headless Chromium.
//
//   node test/e2e/browser.mjs            (after `make build`)
//
// Drives the actual UI: first-run setup, pairing a host, connecting, and
// streaming over direct WebTransport, relayed WebTransport and WebSocket.
// Verifies decoded video, audio, keyboard/mouse delivery to the host, and
// reports the measured latencies.

import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, existsSync, writeFileSync, mkdirSync } from 'node:fs';
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
// is reported and non-negative, (b) per frame the stages add up to end-to-end
// (mean within ±2 ms), recomputed here from the raw timestamps.
const STAGES = ['capture', 'queue', 'network', 'transfer', 'wait', 'decode', 'draw', 'display'];
async function checkStages(name, st) {
  const lat = st?.stages;
  const missing = STAGES.filter((k) => !lat?.stages?.[k]);
  const negative = STAGES.filter((k) => lat?.stages?.[k] && ['p50', 'p95', 'p99'].some((q) => lat.stages[k][q] < 0));
  const p = (r) => (r ? `${r.p50}/${r.p95}/${r.p99}` : '—');
  check(`${name}: per-stage latency (all stages, non-negative)`, !!lat && lat.from === 'capture' && !missing.length && !negative.length && lat.e2e.p50 >= 0,
    lat ? `${lat.from}→draw ${p(lat.e2e)} ms; ${STAGES.map((k) => `${k} ${p(lat.stages[k])}`).join(', ')}${missing.length ? '; missing ' + missing : ''}${negative.length ? '; negative ' + negative : ''}` : 'no stage stats');
  const overlay = await page.textContent('#stats').catch(() => '');
  check(`${name}: overlay shows the stage table`, overlay.includes('End-to-end (capture→draw)') && overlay.includes('host queue') && overlay.includes('display (est.)'));

  await page.evaluate(() => { window.__recon.stageDump = null; window.__recon.worker.postMessage({ type: 'stageDump' }); });
  const dump = await until(() => page.evaluate(() => window.__recon.stageDump), 3000, 'stage dump').catch(() => []);
  let n = 0; let sum = 0; let e2e = 0; let dn = 0; let dsum = 0; let de2e = 0;
  for (const r of dump) {
    const start = (r.fromCapture ? r.captureUs : r.sendUs) / 1000 - r.offset;
    let s = 0;
    for (let i = r.fromCapture ? 0 : 2; i < 7; i++) s += r.stages[i];
    n++; sum += s; e2e += r.drawn - start;
    if (r.displayed !== undefined) { dn++; dsum += s + r.stages[7]; de2e += r.displayed - start; }
  }
  const diff = n ? Math.abs(sum - e2e) / n : Infinity;
  const ddiff = dn ? Math.abs(dsum - de2e) / dn : Infinity;
  check(`${name}: stages add up to end-to-end (±2 ms)`, n >= 20 && diff <= 2 && ddiff <= 2,
    `${n} frames: mean sum ${(sum / n).toFixed(2)} vs end-to-end ${(e2e / n).toFixed(2)} ms; ${dn} with display est: ${(dsum / dn).toFixed(2)} vs ${(de2e / dn).toFixed(2)} ms`);
  results.push({ stages: name, summary: lat, frames: dump.length });
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
const page = await ctx.newPage();
const consoleLines = [];
page.on('console', (m) => { consoleLines.push(`[${m.type()}] ${m.text()}`); if (process.env.E2E_VERBOSE) console.log('[page]', m.text()); });
page.on('pageerror', (e) => consoleLines.push(`[pageerror] ${e.message}`));

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
  const hostCfg = { capture: 'test', testWidth: 960, testHeight: 540, directPort, directAddr: '127.0.0.1', audio: true, logLevel: 'debug' };
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
    { name: 'WebTransport direct', prefs: { path: 'auto', transport: 'auto' }, expect: ['webtransport', 'direct'] },
    { name: 'WebTransport relay', prefs: { path: 'relay', transport: 'auto' }, expect: ['webtransport', 'relay'] },
    { name: 'WebSocket relay', prefs: { path: 'relay', transport: 'websocket' }, expect: ['websocket', 'relay'] },
    { name: 'WebGPU renderer', prefs: { path: 'auto', transport: 'auto', renderer: 'webgpu' }, expect: ['webtransport', 'direct'] },
  ];
  if (process.env.E2E_ROTATE) scenarios.push(scenarios.shift());

  // Unmeasured warm-up stream. On CPU-only CI machines the browser's first
  // software AV1 decoder instance falls behind for a few seconds (the client
  // detects this, flushes and recovers); hardware decoding is not affected.
  await page.goto(`${base}/`);
  await page.evaluate(() => localStorage.setItem('recon.prefs.v1', JSON.stringify({ path: 'relay' })));
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
  for (const sc of scenarios) {
    writeFileSync(inputLog, '');
    await page.goto(`${base}/`);
    await page.evaluate((p) => localStorage.setItem('recon.prefs.v1', JSON.stringify({ stats: true, ...p })), sc.prefs);
    await page.click('.host.online a.btn-primary');
    await page.waitForURL(/\/stream\?host=/);
    await page.waitForSelector('#btn-start:not(.hidden)', { timeout: 15000 });
    const t0 = Date.now();
    await page.click('#btn-start');
    await page.waitForFunction(() => window.__recon && window.__recon.streaming, null, { timeout: 30000 });
    const firstFrameMs = Date.now() - t0;
    const conn = await page.evaluate(() => window.__recon.conn);
    check(`${sc.name}: connected`, conn.transport === sc.expect[0] && conn.path === sc.expect[1], `${conn.transport}/${conn.path}, renderer ${conn.renderer}, first frame after ${firstFrameMs} ms`);

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
    const steady = tail.length === 3 && avg >= 50; // per-0.5 s samples jitter when frames bunch at a boundary
    const st = await page.evaluate(() => window.__recon.lastStats);
    check(`${sc.name}: steady real-time playback`, steady, `last 1.5 s: ${tail.map((p) => p.fps).join(' / ')} fps; key requests ${st?.keyRequests}`);
    const cfg = await page.evaluate(() => window.__recon.videoCfg);
    check(`${sc.name}: video decoding`, st && st.fps > 45, `${st?.fps.toFixed(1)} fps, ${st?.mbps.toFixed(2)} Mbps, codec ${cfg?.codec} via ${cfg?.encoder}`);
    check(`${sc.name}: latency measured`, st && st.synced && st.total !== null,
      `stream ${st?.total?.toFixed(1)} ms (network ${st?.owd?.toFixed(2)} ms, decode ${st?.decode?.toFixed(2)} ms, RTT ${st?.rtt?.toFixed(2)} ms)`);
    await checkStages(sc.name, st);
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
    }
    await page.evaluate(() => { window.__recon.userClosed = true; });
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
  const hostProc = procs.find((p) => p.spawnargs.includes('run'));
  writeFileSync(join(outDir, 'host.log'), hostProc ? hostProc.log : '');
  writeFileSync(join(outDir, 'gateway.log'), gw.log);
  await browser.close();
  cleanup();
}

const bad = results.filter((r) => r.ok === false);
console.log(`\n${results.filter((r) => r.ok === true).length} checks passed, ${bad.length} failed`);
process.exit(failed || bad.length ? 1 : 0);
