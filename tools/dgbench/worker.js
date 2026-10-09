// dgbench worker: one WebTransport session, one run (see main.go). Receives
// the server's datagrams as the stream client's worker does (a reader loop on
// datagrams.readable), optionally busy for busyMs of every frame interval, and
// reports what arrived.
const b64 = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
const wall = () => performance.timeOrigin + performance.now();
const pct = (v, q) => (v.length ? v[Math.min(v.length - 1, Math.floor(q * v.length))] : null);
const round = (x, d = 2) => (x === null ? null : +x.toFixed(d));

self.onmessage = async ({ data: p }) => {
  try {
    self.postMessage(await run(p));
  } catch (e) {
    self.postMessage({ error: e.message || String(e) });
  }
};

async function run(p) {
  const wt = new WebTransport(p.url, {
    serverCertificateHashes: [{ algorithm: 'sha-256', value: b64(p.hash) }],
    requireUnreliable: true,
    congestionControl: 'low-latency',
  });
  await wt.ready;
  const dg = wt.datagrams;
  const env = { incomingHighWaterMark: dg.incomingHighWaterMark, incomingMaxAge: dg.incomingMaxAge, maxDatagramSize: dg.maxDatagramSize };
  if (p.hwm) dg.incomingHighWaterMark = p.hwm;
  env.hwmSet = dg.incomingHighWaterMark;

  const st = await wt.createBidirectionalStream();
  const w = st.writable.getWriter();
  const body = new TextEncoder().encode(JSON.stringify({ mbps: p.mbps, seconds: p.seconds, fps: p.fps, shard: p.shard }));
  const msg = new Uint8Array(4 + body.length);
  new DataView(msg.buffer).setUint32(0, body.length, true);
  msg.set(body, 4);
  await w.write(msg);
  // The server's result, after its last datagram.
  const result = (async () => {
    const r = st.readable.getReader();
    let buf = new Uint8Array(0);
    for (;;) {
      const { value, done } = await r.read();
      if (done) throw new Error('no result');
      const b = new Uint8Array(buf.length + value.length);
      b.set(buf); b.set(value, buf.length);
      buf = b;
      if (buf.length >= 4) {
        const n = new DataView(buf.buffer).getUint32(0, true);
        if (buf.length >= 4 + n) return JSON.parse(new TextDecoder().decode(buf.subarray(4, 4 + n)));
      }
    }
  })();

  // The work a stream client does every frame interval (decode, draw): the
  // worker's event loop does not read datagrams meanwhile.
  let busy = 0;
  if (p.busyMs > 0) {
    busy = setInterval(() => { const t = performance.now(); while (performance.now() - t < p.busyMs); }, 1000 / p.fps);
  }
  // A long stall once a second (a garbage collection, a slow draw): how much
  // the browser's receive queue holds meanwhile.
  let stall = 0;
  if (p.stallMs > 0) {
    stall = setInterval(() => { const t = performance.now(); while (performance.now() - t < p.stallMs); }, 1000);
  }

  const owd = [];
  const bursts = new Map(); // burst -> { n, got, sent, last }
  let received = 0;
  let maxSeq = -1;
  let reordered = 0;
  let duplicates = 0;
  const seen = new Set();
  let first = 0;
  let last = 0;
  let maxGap = 0;
  let reads = 0;
  const reader = dg.readable.getReader();
  let stop = false;
  const loop = (async () => {
    while (!stop) {
      const { value, done } = await reader.read();
      if (done) break;
      reads++;
      const t = wall();
      if (value.length < 24 || value[0] !== 0x7f) continue;
      const v = new DataView(value.buffer, value.byteOffset, value.byteLength);
      const seq = v.getUint32(4, true);
      if (seen.has(seq)) { duplicates++; continue; }
      seen.add(seq);
      received++;
      if (seq < maxSeq) reordered++;
      maxSeq = Math.max(maxSeq, seq);
      const sent = v.getFloat64(16, true);
      owd.push(t - sent);
      if (first) maxGap = Math.max(maxGap, t - last);
      else first = t;
      last = t;
      const id = v.getUint32(8, true);
      let b = bursts.get(id);
      if (!b) bursts.set(id, (b = { n: v.getUint16(12, true), got: 0, sent, last: 0 }));
      b.got++;
      b.sent = Math.min(b.sent, sent);
      b.last = t;
    }
  })();
  const res = await result;
  await new Promise((r) => setTimeout(r, 300)); // datagrams still on their way
  stop = true;
  clearInterval(busy);
  clearInterval(stall);
  try { wt.close(); } catch {}
  await loop.catch(() => {});

  owd.sort((a, b) => a - b);
  const frameMs = [];
  let complete = 0;
  for (const b of bursts.values()) {
    if (b.got === b.n) { complete++; frameMs.push(b.last - b.sent); }
  }
  frameMs.sort((a, b) => a - b);
  const span = (last - first) / 1000;
  return {
    params: { mbps: p.mbps, fps: p.fps, shard: p.shard, seconds: p.seconds, busyMs: p.busyMs || 0, stallMs: p.stallMs || 0, hwm: p.hwm || 0 },
    env,
    server: res,
    received,
    lost: res.sent - received,
    lossPct: round((100 * (res.sent - received)) / Math.max(1, res.sent), 3),
    reordered,
    duplicates,
    reads,
    rxMbps: round((received * p.shard * 8) / 1e6 / Math.max(span, 1e-3), 1),
    owdMs: { p50: round(pct(owd, 0.5)), p95: round(pct(owd, 0.95)), p99: round(pct(owd, 0.99)), max: round(owd[owd.length - 1] ?? null) },
    maxGapMs: round(maxGap),
    frames: { sent: res.frames, complete, completePct: round((100 * complete) / Math.max(1, res.frames), 2), p50: round(pct(frameMs, 0.5)), p95: round(pct(frameMs, 0.95)), p99: round(pct(frameMs, 0.99)) },
  };
}
