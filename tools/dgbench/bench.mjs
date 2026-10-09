// Runs the datagram bench (main.go) in headless Chromium over a rate matrix
// and prints one line per run (GUIDE 2.5: can the browser take the
// "datagram + FEC" video mode's datagrams at 50-150 Mbit/s?).
//
//   node tools/dgbench/bench.mjs                   (after `go build -o <dir>/dgbench ./tools/dgbench`,
//                                                   or it builds into a temp dir)
//   DGBENCH_RATES=50,100 DGBENCH_BUSY=0,8 DGBENCH_STALL=0,100 DGBENCH_HWM=0,1024 DGBENCH_SECONDS=5 node tools/dgbench/bench.mjs
//   DGBENCH_OUT=results.json                       (all reports as JSON)
//
// Playwright comes from test/e2e/node_modules (PLAYWRIGHT_MODULE overrides).
import { spawn, spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import net from 'node:net';

const root = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
const pw = await import(process.env.PLAYWRIGHT_MODULE || pathToFileURL(join(root, 'test', 'e2e', 'node_modules', 'playwright', 'index.mjs')).href);
const { chromium } = pw.default || pw;
const list = (v, d) => (v ? v.split(',').map(Number) : d);
const rates = list(process.env.DGBENCH_RATES, [50, 100, 150, 200]);
const busies = list(process.env.DGBENCH_BUSY, [0, 8]);
const hwms = list(process.env.DGBENCH_HWM, [0]);
const stalls = list(process.env.DGBENCH_STALL, [0]); // ms of a stall once a second
const seconds = Number(process.env.DGBENCH_SECONDS || 5);
const fps = Number(process.env.DGBENCH_FPS || 60);
const shard = Number(process.env.DGBENCH_SHARD || 1218); // the FEC mode's datagram: 18-byte header + 1200

const port = await new Promise((res) => { const s = net.createServer(); s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); }); });
let bin = process.env.DGBENCH_BIN;
let tmp = null;
if (!bin) {
  tmp = mkdtempSync(join(tmpdir(), 'dgbench-'));
  bin = join(tmp, 'dgbench');
  const b = spawnSync('go', ['build', '-o', bin, './tools/dgbench'], { cwd: root, stdio: 'inherit' });
  if (b.status !== 0) process.exit(1);
}
const srv = spawn(bin, ['-listen', `127.0.0.1:${port}`], { stdio: ['ignore', 'inherit', 'inherit'] });
const browser = await chromium.launch({ headless: true });
const out = [];
try {
  await new Promise((r) => setTimeout(r, 500));
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true });
  const page = await ctx.newPage();
  await page.goto(`https://127.0.0.1:${port}/`);
  console.log(`Chromium ${browser.version()}; ${shard}-byte datagrams, ${fps} bursts/s, ${seconds} s per run`);
  const runs = hwms.flatMap((hwm) => stalls.flatMap((stallMs) => busies.flatMap((busyMs) => rates.map((mbps) => ({ hwm, stallMs, busyMs, mbps })))));
  for (const { hwm, stallMs, busyMs, mbps } of runs) {
    const r = await page.evaluate((p) => window.runBench(p), { mbps, fps, shard, seconds, busyMs, stallMs, hwm }).catch((e) => ({ error: e.message }));
    out.push(r);
    const what = `${String(mbps).padStart(4)} Mbit/s busy ${busyMs} ms${stallMs ? ` stall ${stallMs} ms/s` : ''} hwm ${hwm || `default(${r.env?.incomingHighWaterMark})`}`;
    if (r.error) { console.log(`${what}: ${r.error}`); continue; }
    const s = r.server;
    console.log(`${what}: rx ${r.rxMbps} Mbit/s, loss ${r.lossPct} % (${r.lost} of ${s.sent}), reordered ${r.reordered}, ` +
      `one-way p50/p95/p99/max ${r.owdMs.p50}/${r.owdMs.p95}/${r.owdMs.p99}/${r.owdMs.max} ms, longest gap ${r.maxGapMs} ms, ` +
      `frames complete ${r.frames.completePct} % (last datagram p50/p95/p99 ${r.frames.p50}/${r.frames.p95}/${r.frames.p99} ms after the first was sent), ` +
      `sender late ${s.late}, blocked ${Math.round(s.blockedMs)} ms${s.err ? `, error ${s.err}` : ''}`);
  }
  if (out[0]?.env) console.log(`datagrams: ${JSON.stringify(out[0].env)}`);
} finally {
  if (process.env.DGBENCH_OUT) writeFileSync(process.env.DGBENCH_OUT, JSON.stringify(out, null, 2));
  await browser.close();
  srv.kill('SIGTERM');
  if (tmp) rmSync(tmp, { recursive: true, force: true });
}
