// Stream worker: owns the network transport, the WebCodecs decoders and the
// OffscreenCanvas renderer so that nothing on the main thread (layout, GC,
// input handling) can delay a frame.
//
//   WebTransport (per-frame QUIC streams + datagrams) or WebSocket fallback
//     -> reorder by sequence -> VideoDecoder (optimizeForLatency)
//     -> immediate draw on a desynchronized 2D canvas or WebGPU external texture
//   Opus datagrams -> AudioDecoder -> lock-free SharedArrayBuffer ring -> AudioWorklet

import * as P from './protocol.js';

const td = new TextDecoder();
const post = (type, data = {}) => self.postMessage({ type, ...data });
const now = () => performance.now();

// ---------------------------------------------------------------------------
// State

let transport = null;
let prefs = {};
let byeReason = '';
let renderer = null;
let canvas = null;

const video = {
  cfg: null,
  decoder: null,
  ready: false,
  waitingKey: true,
  expectSeq: 0,
  reorder: new Map(),
  gapSince: 0,
  early: [],
  inflight: new Map(),
  hw: null,
  lastSize: '',
  lostGen: -1,
  keyRequested: 0,
  waitSince: 0,
};

const clock = { offset: null, samples: [], pingId: 0, pings: new Map(), rtt: 0 };

const stats = {
  frames: 0, bytes: 0, decodeSum: 0, decodeN: 0, owdSum: 0, owdN: 0, totalSum: 0, totalN: 0,
  dropped: 0, keyRequests: 0, lastPost: now(), totalMin: Infinity, totalMax: 0,
  audioPackets: 0, audioLost: 0,
};

const congestion = { owdHist: [], over: 0, lastSent: 0 };

const audio = { cfg: null, decoder: null, ring: null, port: null, lastSeq: -1, L: null, R: null };

// ---------------------------------------------------------------------------
// Clock synchronisation (NTP-style, keep the minimum-RTT sample)

function hostToLocal(us) {
  return us / 1000 - (clock.offset ?? 0);
}

function sendPing() {
  if (!transport) return;
  const id = ++clock.pingId;
  const t0 = now();
  clock.pings.set(id, t0);
  if (clock.pings.size > 20) clock.pings.delete(clock.pings.keys().next().value);
  transport.sendDatagram(P.ping(id, t0));
}

function onPong(d) {
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const id = v.getUint32(4, true);
  const t0 = v.getFloat64(8, true);
  const hostUs = Number(v.getBigUint64(16, true));
  if (!clock.pings.has(id)) return;
  clock.pings.delete(id);
  const t1 = now();
  const rtt = t1 - t0;
  clock.rtt = clock.rtt ? clock.rtt * 0.8 + rtt * 0.2 : rtt;
  clock.samples.push({ rtt, offset: hostUs / 1000 - (t0 + rtt / 2), at: t1 });
  clock.samples = clock.samples.filter((s) => t1 - s.at < 30000).slice(-40);
  let best = clock.samples[0];
  for (const s of clock.samples) if (s.rtt < best.rtt) best = s;
  clock.offset = best.offset;
}

// ---------------------------------------------------------------------------
// Transports

function b64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function withTimeout(p, ms, what) {
  let t;
  return Promise.race([
    p.finally(() => clearTimeout(t)),
    new Promise((_, rej) => { t = setTimeout(() => rej(new Error(`${what} timed out`)), ms); }),
  ]);
}

/** Incremental parser for u32-length-prefixed messages on a byte stream. */
class MsgParser {
  constructor(onMsg) { this.buf = new Uint8Array(0); this.onMsg = onMsg; }
  push(chunk) {
    if (this.buf.length === 0) this.buf = chunk;
    else {
      const b = new Uint8Array(this.buf.length + chunk.length);
      b.set(this.buf); b.set(chunk, this.buf.length);
      this.buf = b;
    }
    while (this.buf.length >= 4) {
      const n = new DataView(this.buf.buffer, this.buf.byteOffset, 4).getUint32(0, true);
      if (n > (1 << 20)) throw new Error('control message too large');
      if (this.buf.length < 4 + n) break;
      this.onMsg(this.buf.subarray(4, 4 + n));
      this.buf = this.buf.subarray(4 + n);
    }
  }
}

/** Read a frame stream; also returns when its first bytes arrived. */
async function readAll(stream) {
  const r = stream.getReader();
  const chunks = [];
  let n = 0;
  let first = 0;
  for (;;) {
    const { value, done } = await r.read();
    if (done) break;
    if (!first) first = now();
    chunks.push(value);
    n += value.byteLength;
  }
  if (chunks.length === 1) return { buf: chunks[0], first };
  const out = new Uint8Array(n);
  let o = 0;
  for (const c of chunks) { out.set(c, o); o += c.byteLength; }
  return { buf: out, first };
}

async function openWebTransport(url, hashes, label) {
  const opts = { requireUnreliable: true, congestionControl: 'low-latency' };
  if (hashes && hashes.length) {
    opts.serverCertificateHashes = hashes.map((h) => ({ algorithm: 'sha-256', value: b64(h) }));
  }
  const wt = new WebTransport(url, opts);
  wt.closed.catch(() => {});
  try {
    await withTimeout(wt.ready, label === 'direct' ? 2500 : 6000, `WebTransport ${label}`);
  } catch (e) {
    try { wt.close(); } catch {}
    throw e;
  }
  const ctrl = await wt.createBidirectionalStream({ sendOrder: 100 });
  const input = await wt.createBidirectionalStream({ sendOrder: 1000 });
  const cw = ctrl.writable.getWriter();
  const iw = input.writable.getWriter();
  const dw = wt.datagrams.writable.getWriter();
  const swallow = () => {};
  cw.write(Uint8Array.of(P.KIND_CONTROL)).catch(swallow);
  iw.write(Uint8Array.of(P.KIND_INPUT)).catch(swallow);
  let closed = false;
  return {
    kind: 'webtransport',
    path: label,
    sendControl: (obj) => { if (!closed) cw.write(P.frameMsg(P.jsonBytes(obj))).catch(swallow); },
    sendInput: (b) => { if (!closed) iw.write(P.frameMsg(b)).catch(swallow); },
    sendDatagram: (b) => { if (!closed) dw.write(b).catch(swallow); },
    close: () => { closed = true; try { wt.close({ closeCode: 0, reason: 'bye' }); } catch {} },
    async run(h) {
      const ctlParser = new MsgParser((m) => h.control(JSON.parse(td.decode(m))));
      const ctlLoop = (async () => {
        const r = ctrl.readable.getReader();
        for (;;) {
          const { value, done } = await r.read();
          if (done) break;
          ctlParser.push(value);
        }
      })();
      const dgLoop = (async () => {
        const r = wt.datagrams.readable.getReader();
        for (;;) {
          const { value, done } = await r.read();
          if (done) break;
          h.datagram(value);
        }
      })();
      const uniLoop = (async () => {
        const r = wt.incomingUnidirectionalStreams.getReader();
        for (;;) {
          const { value, done } = await r.read();
          if (done) break;
          readAll(value).then(({ buf, first }) => h.frame(buf, now(), first)).catch(() => {});
        }
      })();
      ctlLoop.catch(() => {});
      dgLoop.catch(() => {});
      uniLoop.catch(() => {});
      try {
        const info = await wt.closed;
        return info?.reason || 'connection closed';
      } catch (e) {
        return e?.message || 'connection lost';
      } finally {
        closed = true;
      }
    },
  };
}

async function openWebSocket(url) {
  const ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';
  await withTimeout(new Promise((res, rej) => {
    ws.onopen = res;
    ws.onerror = () => rej(new Error('WebSocket failed'));
  }), 8000, 'WebSocket');
  const send = (ch, b) => {
    if (ws.readyState !== WebSocket.OPEN) return;
    const out = new Uint8Array(1 + b.byteLength);
    out[0] = ch;
    out.set(b, 1);
    ws.send(out);
  };
  return {
    kind: 'websocket',
    path: 'relay',
    sendControl: (obj) => send(P.WS_CONTROL, P.jsonBytes(obj)),
    sendInput: (b) => send(P.WS_INPUT, b),
    sendDatagram: (b) => send(P.WS_DATAGRAM, b),
    close: () => { try { ws.close(1000, 'bye'); } catch {} },
    run(h) {
      return new Promise((resolve) => {
        ws.onmessage = (ev) => {
          const d = new Uint8Array(ev.data);
          const body = d.subarray(1);
          switch (d[0]) {
            case P.WS_CONTROL: h.control(JSON.parse(td.decode(body))); break;
            case P.WS_FRAME: h.frame(body, now()); break; // whole message: first byte = last byte
            case P.WS_DATAGRAM: h.datagram(body); break;
          }
        };
        ws.onclose = (ev) => resolve(ev.reason || 'connection closed');
        ws.onerror = () => {};
      });
    },
  };
}

async function connect(ep) {
  const attempts = [];
  const wtOK = typeof WebTransport !== 'undefined' && prefs.transport !== 'websocket';
  if (wtOK && ep.direct && prefs.path !== 'relay') attempts.push(['direct', () => openWebTransport(ep.direct.url, ep.direct.hashes, 'direct'), ep.direct.ticket]);
  if (wtOK && prefs.path !== 'direct') attempts.push(['relay', () => openWebTransport(ep.relay.wt, ep.relay.hashes, 'relay'), '']);
  if (prefs.path !== 'direct') attempts.push(['websocket', () => openWebSocket(ep.relay.ws), '']);
  let lastErr;
  for (const [label, fn, ticket] of attempts) {
    try {
      post('status', { text: `Connecting (${label})…` });
      const t = await fn();
      return { t, ticket };
    } catch (e) {
      lastErr = e;
      post('log', { text: `${label} failed: ${e.message}` });
    }
  }
  throw lastErr || new Error('no transport available');
}

// ---------------------------------------------------------------------------
// Rendering

class Canvas2DRenderer {
  constructor(c) {
    this.c = c;
    this.ctx = c.getContext('2d', { alpha: false, desynchronized: true });
    this.name = 'canvas2d-desync';
  }
  draw(frame) {
    if (this.c.width !== frame.displayWidth || this.c.height !== frame.displayHeight) {
      this.c.width = frame.displayWidth;
      this.c.height = frame.displayHeight;
    }
    this.ctx.drawImage(frame, 0, 0);
    frame.close();
  }
}

const WGSL = `
@group(0) @binding(0) var samp: sampler;
@group(0) @binding(1) var tex: texture_external;
struct VOut { @builtin(position) pos: vec4f, @location(0) uv: vec2f };
@vertex fn vs(@builtin(vertex_index) i: u32) -> VOut {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  var o: VOut;
  o.pos = vec4f(p[i], 0.0, 1.0);
  o.uv = vec2f((p[i].x + 1.0) * 0.5, (1.0 - p[i].y) * 0.5);
  return o;
}
@fragment fn fs(v: VOut) -> @location(0) vec4f {
  return textureSampleBaseClampToEdge(tex, samp, v.uv);
}`;

class WebGPURenderer {
  // Runs the full import -> draw -> submit path on a scratch canvas first: a
  // canvas cannot switch context types, so the real one is only claimed once
  // WebGPU demonstrably works on this device/driver.
  static async selfTest(device, format, pipelineFor) {
    const scratch = new OffscreenCanvas(16, 16);
    const ctx = scratch.getContext('webgpu');
    ctx.configure({ device, format, alphaMode: 'opaque' });
    const src = new OffscreenCanvas(16, 16);
    src.getContext('2d').fillRect(0, 0, 16, 16);
    const vf = new VideoFrame(src, { timestamp: 0 });
    try {
      device.pushErrorScope('validation');
      const { pipeline, sampler } = pipelineFor;
      const ext = device.importExternalTexture({ source: vf });
      const bg = device.createBindGroup({ layout: pipeline.getBindGroupLayout(0), entries: [{ binding: 0, resource: sampler }, { binding: 1, resource: ext }] });
      const enc = device.createCommandEncoder();
      const pass = enc.beginRenderPass({ colorAttachments: [{ view: ctx.getCurrentTexture().createView(), loadOp: 'clear', storeOp: 'store' }] });
      pass.setPipeline(pipeline);
      pass.setBindGroup(0, bg);
      pass.draw(3);
      pass.end();
      device.queue.submit([enc.finish()]);
      await withTimeout(device.queue.onSubmittedWorkDone(), 1500, 'WebGPU self-test');
      const err = await device.popErrorScope();
      if (err) throw new Error(err.message);
    } finally {
      vf.close();
    }
  }

  static async create(c) {
    if (!self.navigator.gpu) throw new Error('WebGPU unavailable');
    const adapter = await navigator.gpu.requestAdapter({ powerPreference: 'high-performance' });
    if (!adapter) throw new Error('no WebGPU adapter');
    const device = await adapter.requestDevice();
    const formatProbe = navigator.gpu.getPreferredCanvasFormat();
    {
      const m = device.createShaderModule({ code: WGSL });
      const pl = device.createRenderPipeline({
        layout: 'auto', vertex: { module: m, entryPoint: 'vs' }, fragment: { module: m, entryPoint: 'fs', targets: [{ format: formatProbe }] },
        primitive: { topology: 'triangle-list' },
      });
      await WebGPURenderer.selfTest(device, formatProbe, { pipeline: pl, sampler: device.createSampler() });
    }
    const ctx = c.getContext('webgpu');
    const format = navigator.gpu.getPreferredCanvasFormat();
    ctx.configure({ device, format, alphaMode: 'opaque' });
    const module = device.createShaderModule({ code: WGSL });
    const pipeline = device.createRenderPipeline({
      layout: 'auto',
      vertex: { module, entryPoint: 'vs' },
      fragment: { module, entryPoint: 'fs', targets: [{ format }] },
      primitive: { topology: 'triangle-list' },
    });
    const r = new WebGPURenderer();
    Object.assign(r, { c, device, ctx, pipeline, sampler: device.createSampler({ magFilter: 'linear', minFilter: 'linear' }), prev: null, name: 'webgpu-zero-copy' });
    return r;
  }
  draw(frame) {
    if (this.c.width !== frame.displayWidth || this.c.height !== frame.displayHeight) {
      this.c.width = frame.displayWidth;
      this.c.height = frame.displayHeight;
    }
    const ext = this.device.importExternalTexture({ source: frame });
    const bg = this.device.createBindGroup({
      layout: this.pipeline.getBindGroupLayout(0),
      entries: [{ binding: 0, resource: this.sampler }, { binding: 1, resource: ext }],
    });
    const enc = this.device.createCommandEncoder();
    const pass = enc.beginRenderPass({
      colorAttachments: [{ view: this.ctx.getCurrentTexture().createView(), loadOp: 'clear', storeOp: 'store', clearValue: { r: 0, g: 0, b: 0, a: 1 } }],
    });
    pass.setPipeline(this.pipeline);
    pass.setBindGroup(0, bg);
    pass.draw(3);
    pass.end();
    this.device.queue.submit([enc.finish()]);
    // Keep the frame alive until the next one so the GPU never samples a closed frame.
    if (this.prev) this.prev.close();
    this.prev = frame;
  }
}

async function makeRenderer() {
  if (prefs.renderer === 'webgpu') {
    try {
      return await WebGPURenderer.create(canvas);
    } catch (e) {
      post('log', { text: `WebGPU renderer unavailable (${e.message}); using low-latency 2D canvas` });
    }
  }
  return new Canvas2DRenderer(canvas);
}

// ---------------------------------------------------------------------------
// Video

const isNewerGen = (a, b) => { const d = (a - b) & 0xff; return d > 0 && d < 128; };

async function configureDecoder(cfg) {
  video.ready = false;
  if (video.decoder && video.decoder.state !== 'closed') {
    try { video.decoder.close(); } catch {}
  }
  video.inflight.clear();
  const base = { codec: cfg.codec, optimizeForLatency: true, codedWidth: cfg.width, codedHeight: cfg.height };
  const wantHW = prefs.decoder !== 'software';
  let config = { ...base, hardwareAcceleration: wantHW ? 'prefer-hardware' : 'prefer-software' };
  let support = await VideoDecoder.isConfigSupported(config).catch(() => ({ supported: false }));
  video.hw = wantHW && support.supported;
  if (!support.supported) {
    config = { ...base, hardwareAcceleration: 'no-preference' };
    support = await VideoDecoder.isConfigSupported(config).catch(() => ({ supported: false }));
  }
  if (!support.supported) {
    post('notice', { level: 'error', msg: `This browser cannot decode ${cfg.codec}. Pick another codec in settings.` });
    return false;
  }
  if (cfg !== video.cfg) return false; // superseded while awaiting
  video.decoder = new VideoDecoder({ output: onDecoded, error: onDecodeError });
  video.decoder.configure(config);
  video.ready = true;
  return true;
}

function onDecodeError(e) {
  post('log', { text: `decoder error: ${e.message}` });
  requestKeyframe('decoder error');
  if (video.cfg) configureDecoder(video.cfg).then(() => drainEarly());
}

// Drop the current generation and wait for a fresh key frame. When send is
// false the caller asks the host for a restart some other way (congestion).
function requestKeyframe(reason, send = true) {
  const t = now();
  video.waitingKey = true;
  video.reorder.clear();
  if (t - video.keyRequested < 400) return;
  video.keyRequested = t;
  stats.keyRequests++;
  video.lostGen = video.cfg ? video.cfg.gen : -1;
  post('log', { text: `requesting key frame (${reason})` });
  if (send) transport?.sendControl({ t: 'keyframe' });
}

// Watchdog: if we have been waiting for a key frame (or a new generation) for
// more than a second, ask again. A request can be dropped by the host's rate
// limits or lost with a connection hiccup; this guarantees video resumes.
function videoWatchdog() {
  if (!transport || !video.cfg) return;
  const waiting = video.waitingKey || video.lostGen === video.cfg.gen;
  const t = now();
  if (!waiting) { video.waitSince = 0; return; }
  if (!video.waitSince) { video.waitSince = t; return; }
  if (t - video.waitSince > 1000 && t - video.keyRequested > 1000) {
    video.keyRequested = t;
    stats.keyRequests++;
    post('log', { text: 'still waiting for a key frame, asking again' });
    transport.sendControl({ t: 'keyframe' });
  }
}

async function onVideoConfig(cfg) {
  video.waitSince = 0;
  video.cfg = cfg;
  video.expectSeq = 0;
  video.waitingKey = true;
  video.reorder.clear();
  video.lostGen = -1;
  post('video', { cfg });
  if (await configureDecoder(cfg)) drainEarly();
}

function drainEarly() {
  const early = video.early;
  video.early = [];
  early.sort((a, b) => a.seq - b.seq);
  for (const f of early) onFrame(f);
}

function onFrameBytes(buf, recv, first = recv) {
  const h = P.parseFrameHeader(buf);
  if (!h) return;
  h.data = buf.subarray(h.headerLen);
  h.recv = recv;
  h.first = first || recv;
  stats.bytes += buf.length;
  checkCongestion(recv - hostToLocal(sentUs(h)));
  onFrame(h);
}

// Hosts before the frame extension stamped sendUs when the frame left the
// encoder; newer ones stamp the actual send and carry encodeDone separately.
// Congestion detection and frame acks keep the encoder-based reference.
const sentUs = (h) => h.ext?.encodeDoneUs ?? h.sendUs;

function onFrame(f) {
  const cfg = video.cfg;
  if (!cfg || isNewerGen(f.gen, cfg.gen) || (f.gen === cfg.gen && !video.ready)) {
    // Config for this generation not processed yet: hold briefly.
    if (video.early.length < 240) video.early.push(f);
    return;
  }
  if (f.gen !== cfg.gen || f.gen === video.lostGen) return; // superseded generation
  if (f.seq < video.expectSeq) return; // duplicate
  if (f.seq > video.expectSeq) {
    video.reorder.set(f.seq, f);
    if (!video.gapSince) video.gapSince = now();
    if (now() - video.gapSince > 150 || video.reorder.size > 30) {
      stats.dropped += f.seq - video.expectSeq;
      video.gapSince = 0;
      requestKeyframe('frame lost');
    }
    return;
  }
  decodeFrame(f);
  video.expectSeq++;
  while (video.reorder.has(video.expectSeq)) {
    const n = video.reorder.get(video.expectSeq);
    video.reorder.delete(video.expectSeq);
    decodeFrame(n);
    video.expectSeq++;
  }
  video.gapSince = video.reorder.size ? now() : 0;
}

// If the decoder cannot keep up (slow device, software decode), frames queue
// and latency grows without bound. Detect a sustained backlog, drop it, and
// restart from a fresh key frame (and ask the host to back off).
const overload = { since: 0, warned: false };
function checkDecoderBacklog() {
  const d = video.decoder;
  if (!d || d.state !== 'configured') return false;
  const fps = video.cfg?.fps || 60;
  const backlog = video.inflight.size;
  if (backlog <= Math.max(4, fps / 10)) { overload.since = 0; return false; }
  const t = now();
  if (!overload.since) overload.since = t;
  if (t - overload.since < 500) return false;
  overload.since = 0;
  try { d.reset(); } catch {}
  video.inflight.clear();
  configureDecoder(video.cfg).then(() => drainEarly());
  // One message: the host's congestion response lowers the bitrate *and*
  // restarts with a key frame.
  requestKeyframe('decoder backlog', false);
  transport?.sendControl({ t: 'congestion', delayMs: 0 });
  if (!overload.warned) {
    overload.warned = true;
    post('notice', { level: 'warn', msg: 'This device is not decoding fast enough — lowering bitrate. Try H.264, a lower resolution or frame rate.' });
  }
  return true;
}

function decodeFrame(f) {
  if (checkDecoderBacklog()) return;
  if (video.waitingKey) {
    if (!f.key) return;
    video.waitingKey = false;
  }
  const d = video.decoder;
  if (!d || d.state !== 'configured') return;
  video.inflight.set(f.ptsUs, {
    recv: f.recv, first: f.first, sendUs: f.sendUs, ext: f.ext, seq: f.seq, gen: f.gen, t: now(),
  });
  if (video.inflight.size > 120) video.inflight.delete(video.inflight.keys().next().value);
  try {
    d.decode(new EncodedVideoChunk({ type: f.key ? 'key' : 'delta', timestamp: f.ptsUs, data: f.data }));
  } catch (e) {
    onDecodeError(e);
  }
}

let firstFrame = true;

function onDecoded(frame) {
  const meta = video.inflight.get(frame.timestamp);
  video.inflight.delete(frame.timestamp);
  const decoded = now();
  const size = `${frame.displayWidth}x${frame.displayHeight}`;
  if (size !== video.lastSize) {
    video.lastSize = size;
    post('resolution', { w: frame.displayWidth, h: frame.displayHeight });
  }
  try {
    renderer.draw(frame);
  } catch (e) {
    frame.close();
    post('log', { text: `render error: ${e.message}` });
  }
  const presented = now();
  stats.frames++;
  if (firstFrame) {
    firstFrame = false;
    post('firstFrame', { renderer: renderer.name });
  }
  if (!meta) return;
  const decodeMs = decoded - meta.t;
  stats.decodeSum += decodeMs;
  stats.decodeN++;
  if (clock.offset !== null) {
    const owd = meta.recv - hostToLocal(sentUs(meta));
    const total = recordStages(meta, decoded, presented);
    stats.owdSum += owd;
    stats.owdN++;
    stats.totalSum += total;
    stats.totalN++;
    stats.totalMin = Math.min(stats.totalMin, total);
    stats.totalMax = Math.max(stats.totalMax, total);
    transport?.sendDatagram(P.frameAck(meta.gen, meta.seq, owd * 1000, decodeMs * 1000));
  }
}

// ---------------------------------------------------------------------------
// Per-stage latency. Host stamps (capture, encodeDone: frame header extension;
// send: header) are host-clock µs, converted with the clock sync; the client
// adds first/last byte, decode submit/output, drawn and displayed (estimate:
// the main thread's next requestAnimationFrame after the draw, sampled).
//
//   capture→encodeDone | host queue | network (send→first byte) | transfer |
//   reorder/wait (last byte→submit) | decode | draw | display (est.)
//
// End-to-end runs from capture (or send, when the host cannot stamp the
// capture) to drawn and is the per-frame sum of the stages in that span; the
// sampled display estimate comes on top.

const STAGES = ['capture', 'queue', 'network', 'transfer', 'wait', 'decode', 'draw', 'display'];
const STAGE_WINDOW_MS = 10000;
const DISPLAY_SAMPLE_MS = 50; // display marks: ~20/s keeps the main thread's rAF work small
const lat = { recs: [], pending: null, markId: 0, lastMark: 0, lastReport: now() };

function recordStages(m, decoded, drawn) {
  const capUs = m.ext?.captureUs;
  const doneUs = m.ext?.encodeDoneUs;
  const sendL = hostToLocal(m.sendUs);
  const s = new Array(STAGES.length).fill(null);
  if (capUs !== undefined && doneUs !== undefined) s[0] = (doneUs - capUs) / 1000;
  if (doneUs !== undefined) s[1] = (m.sendUs - doneUs) / 1000;
  s[2] = m.first - sendL;
  s[3] = m.recv - m.first;
  s[4] = m.t - m.recv;
  s[5] = decoded - m.t;
  s[6] = drawn - decoded;
  const fromCapture = s[0] !== null;
  const rec = {
    t: drawn, s, e2e: drawn - (fromCapture ? hostToLocal(capUs) : sendL), e2eSend: drawn - sendL, fromCapture,
    raw: { captureUs: capUs, encodeDoneUs: doneUs, sendUs: m.sendUs, offset: clock.offset, first: m.first, last: m.recv, submit: m.t, output: decoded, drawn },
  };
  lat.recs.push(rec);
  while (lat.recs.length && lat.recs[0].t < drawn - STAGE_WINDOW_MS) lat.recs.shift();
  // At most one display mark in flight: the main thread answers within a refresh.
  if (lat.pending && drawn - lat.pending.t > 250) lat.pending = null; // main thread throttled (hidden)
  if (!lat.pending && drawn - lat.lastMark >= DISPLAY_SAMPLE_MS) {
    lat.pending = rec;
    lat.lastMark = drawn;
    rec.mark = ++lat.markId;
    post('drawn', { id: rec.mark, t: performance.timeOrigin + drawn });
  }
  return rec.e2e;
}

// Main thread: absolute time of its first requestAnimationFrame after the draw.
function onDisplayed(id, abs) {
  const rec = lat.pending;
  if (!rec || rec.mark !== id) return;
  lat.pending = null;
  const shown = abs - performance.timeOrigin;
  if (shown < rec.t) return;
  rec.raw.displayed = shown;
  rec.s[7] = shown - rec.t;
}

function pct(v) {
  if (!v.length) return null;
  const a = Float64Array.from(v).sort();
  const q = (p) => +a[Math.min(a.length - 1, Math.floor(p * a.length))].toFixed(2);
  return { p50: q(0.5), p95: q(0.95), p99: q(0.99), n: a.length };
}

/** Percentiles per stage and end-to-end over the last 10 s. */
function stageSummary() {
  const recs = lat.recs;
  if (!recs.length) return null;
  // One definition per window: from capture only if every frame had it.
  const fromCapture = recs.every((r) => r.fromCapture);
  const first = fromCapture ? 0 : 2;
  const rows = {};
  STAGES.forEach((name, i) => { rows[name] = pct(recs.filter((r) => r.s[i] !== null).map((r) => r.s[i])); });
  let sum = 0;
  let e2eSum = 0;
  for (const r of recs) {
    for (let i = first; i < 7; i++) sum += r.s[i];
    e2eSum += fromCapture ? r.e2e : r.e2eSend;
  }
  return {
    from: fromCapture ? 'capture' : 'send',
    e2e: pct(recs.map((r) => (fromCapture ? r.e2e : r.e2eSend))),
    stages: rows,
    // Mean per-frame sum of the stages inside end-to-end vs mean end-to-end.
    check: { n: recs.length, sumMean: sum / recs.length, e2eMean: e2eSum / recs.length },
  };
}

// Every 10 s the host logs the summary next to its encoder (results per vendor).
function reportStages(sum) {
  const t = now();
  if (!sum || t - lat.lastReport < 10000 || !transport) return;
  lat.lastReport = t;
  const rows = [];
  for (const name of STAGES) if (sum.stages[name]) rows.push({ name, ...sum.stages[name] });
  rows.push({ name: 'e2e', from: sum.from, ...sum.e2e });
  transport.sendControl({ t: 'stages', stages: rows });
}

// Delay-based congestion detection: if one-way delay rises well above its
// recent minimum for a sustained period, the path is queueing. Ask the host to
// back off before latency balloons.
function checkCongestion(owd) {
  if (clock.offset === null || !isFinite(owd)) return;
  const t = now();
  const h = congestion.owdHist;
  h.push([t, owd]);
  while (h.length && t - h[0][0] > 8000) h.shift();
  let base = Infinity;
  for (const [, v] of h) base = Math.min(base, v);
  const excess = owd - base;
  if (excess > 45) congestion.over++;
  else congestion.over = Math.max(0, congestion.over - 2);
  if (congestion.over > 20 && t - congestion.lastSent > 5000 && prefs.adaptive !== false) {
    congestion.lastSent = t;
    congestion.over = 0;
    transport?.sendControl({ t: 'congestion', delayMs: Math.round(excess) });
    post('log', { text: `congestion: +${Math.round(excess)} ms queueing delay` });
  }
}

// ---------------------------------------------------------------------------
// Audio

class RingWriter {
  constructor(sab) {
    this.idx = new Int32Array(sab, 0, 2);
    this.data = new Float32Array(sab, 8);
    this.cap = this.data.length / 2;
  }
  push(L, R, n) {
    const w = Atomics.load(this.idx, 0);
    const r = Atomics.load(this.idx, 1);
    const used = (w - r + this.cap) % this.cap;
    const free = this.cap - 1 - used;
    if (n > free) n = free;
    let p = w;
    for (let i = 0; i < n; i++) {
      this.data[2 * p] = L[i];
      this.data[2 * p + 1] = R[i];
      if (++p === this.cap) p = 0;
    }
    Atomics.store(this.idx, 0, p);
  }
}

function audioOut(L, R, n) {
  if (audio.ring) audio.ring.push(L, R, n);
  else if (audio.port) {
    const inter = new Float32Array(n * 2);
    for (let i = 0; i < n; i++) { inter[2 * i] = L[i]; inter[2 * i + 1] = R[i]; }
    audio.port.postMessage(inter, [inter.buffer]);
  }
}

function silence(n) {
  const z = new Float32Array(n);
  audioOut(z, z, n);
}

function onAudioConfig(cfg) {
  audio.cfg = cfg;
  if (audio.decoder) { try { audio.decoder.close(); } catch {} audio.decoder = null; }
  post('audio', { cfg });
  if (!cfg.enabled || cfg.codec !== 'opus') return;
  audio.decoder = new AudioDecoder({
    output: (ad) => {
      const n = ad.numberOfFrames;
      if (!audio.L || audio.L.length < n) { audio.L = new Float32Array(n); audio.R = new Float32Array(n); }
      ad.copyTo(audio.L, { planeIndex: 0, format: 'f32-planar' });
      ad.copyTo(audio.R, { planeIndex: ad.numberOfChannels > 1 ? 1 : 0, format: 'f32-planar' });
      ad.close();
      audioOut(audio.L, audio.R, n);
    },
    error: (e) => post('log', { text: `audio decoder: ${e.message}` }),
  });
  audio.decoder.configure({ codec: 'opus', sampleRate: 48000, numberOfChannels: 2 });
}

function onAudioPacket(d) {
  if (!audio.cfg || !audio.cfg.enabled || d.length < 8) return;
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const seq = v.getUint16(2, true);
  const pts = v.getUint32(4, true);
  const payload = d.subarray(8);
  const frameSamples = 48 * audio.cfg.frameMs;
  stats.audioPackets++;
  if (audio.lastSeq >= 0) {
    const gap = (seq - audio.lastSeq - 1) & 0xffff;
    if (gap > 0 && gap < 4) { stats.audioLost += gap; silence(gap * frameSamples); }
    else if (gap >= 0x8000) return; // late/duplicate
  }
  audio.lastSeq = seq;
  if (d[1] === P.AUDIO_OPUS && audio.decoder?.state === 'configured') {
    audio.decoder.decode(new EncodedAudioChunk({ type: 'key', timestamp: Math.round(pts * 1e6 / 48000), data: payload }));
  } else if (d[1] === P.AUDIO_PCM) {
    const n = payload.length >> 2;
    const pv = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
    const L = new Float32Array(n);
    const R = new Float32Array(n);
    for (let i = 0; i < n; i++) {
      L[i] = pv.getInt16(4 * i, true) / 32768;
      R[i] = pv.getInt16(4 * i + 2, true) / 32768;
    }
    audioOut(L, R, n);
  }
}

// ---------------------------------------------------------------------------
// Session

function onControl(m) {
  switch (m.t) {
    case 'welcome': post('welcome', { info: m }); break;
    case 'video': onVideoConfig(m); break;
    case 'audio': onAudioConfig(m); break;
    case 'cursor': post('cursor', { shape: m }); break;
    case 'notice': post('notice', { level: m.level, msg: m.msg }); break;
    case 'bye': byeReason = m.msg || 'Session ended by the host'; post('notice', { level: 'warn', msg: byeReason }); break;
    case 'error': post('notice', { level: 'error', msg: m.msg }); break;
  }
}

function onDatagram(d) {
  switch (d[0]) {
    case P.DG_AUDIO: onAudioPacket(d); break;
    case P.DG_PONG: onPong(d); break;
    case P.DG_CURSOR_POS: {
      const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
      post('cursorPos', { visible: (d[1] & 1) === 1, x: v.getUint16(8, true), y: v.getUint16(10, true) });
      break;
    }
    case P.DG_RUMBLE: post('rumble', { idx: d[1], large: d[2], small: d[3] }); break;
  }
}

function postStats() {
  const t = now();
  const dt = (t - stats.lastPost) / 1000;
  stats.lastPost = t;
  const avg = (s, n) => (n ? s / n : null);
  let audioMs = null;
  if (audio.ring) {
    const w = Atomics.load(audio.ring.idx, 0);
    const r = Atomics.load(audio.ring.idx, 1);
    audioMs = (((w - r + audio.ring.cap) % audio.ring.cap) / 48);
  }
  const stages = stageSummary();
  reportStages(stages);
  post('stats', {
    stages,
    fps: stats.frames / dt,
    mbps: (stats.bytes * 8) / dt / 1e6,
    rtt: clock.rtt,
    owd: avg(stats.owdSum, stats.owdN),
    decode: avg(stats.decodeSum, stats.decodeN),
    total: avg(stats.totalSum, stats.totalN),
    totalMin: isFinite(stats.totalMin) ? stats.totalMin : null,
    totalMax: stats.totalMax || null,
    dropped: stats.dropped,
    keyRequests: stats.keyRequests,
    audioPackets: stats.audioPackets,
    audioLost: stats.audioLost,
    audioMs,
    queue: video.decoder ? video.decoder.decodeQueueSize : 0,
    hw: video.hw,
    synced: clock.offset !== null,
  });
  Object.assign(stats, { frames: 0, bytes: 0, decodeSum: 0, decodeN: 0, owdSum: 0, owdN: 0, totalSum: 0, totalN: 0, totalMin: Infinity, totalMax: 0 });
}

async function probeDecoders() {
  const tests = [['h264', 'avc1.640033'], ['hevc', 'hev1.1.6.L153.B0'], ['av1', 'av01.0.13M.08']];
  const out = [];
  for (const [family, codec] of tests) {
    const q = (hardwareAcceleration) => VideoDecoder.isConfigSupported({ codec, codedWidth: 1920, codedHeight: 1080, hardwareAcceleration })
      .then((r) => r.supported).catch(() => false);
    const hw = await q('prefer-hardware');
    const any = hw || (await q('no-preference'));
    if (any) out.push({ family, hw });
  }
  return out;
}

async function start(msg) {
  prefs = msg.prefs || {};
  canvas = msg.canvas;
  if (msg.audioSab) audio.ring = new RingWriter(msg.audioSab);
  if (msg.audioPort) audio.port = msg.audioPort;
  renderer = await makeRenderer();
  const decoders = await probeDecoders();
  post('decoders', { decoders });
  let conn;
  try {
    conn = await connect(msg.endpoints);
  } catch (e) {
    post('closed', { reason: `Could not connect: ${e.message}`, retry: true });
    return;
  }
  transport = conn.t;
  post('connected', { transport: transport.kind, path: transport.path, renderer: renderer.name });
  const opusOK = typeof AudioDecoder !== 'undefined' &&
    (await AudioDecoder.isConfigSupported({ codec: 'opus', sampleRate: 48000, numberOfChannels: 2 }).then((r) => r.supported).catch(() => false));
  transport.sendControl({
    t: 'hello', v: P.HELLO_VERSION, ticket: conn.ticket,
    client: msg.client, decoders, audio: { opus: opusOK, pcm: true }, prefs: msg.hostPrefs,
  });
  for (let i = 0; i < 5; i++) setTimeout(sendPing, i * 60);
  const pingTimer = setInterval(sendPing, 1000);
  const watchdogTimer = setInterval(videoWatchdog, 250);
  const statsTimer = setInterval(postStats, 500);
  const reason = await transport.run({ control: onControl, datagram: onDatagram, frame: onFrameBytes });
  clearInterval(pingTimer);
  clearInterval(statsTimer);
  clearInterval(watchdogTimer);
  transport = null;
  // A deliberate "bye" (e.g. another device took over) must not trigger an
  // automatic reconnect, or two clients would keep stealing the session.
  post('closed', { reason: byeReason || reason, retry: !byeReason });
}

self.onmessage = (ev) => {
  const m = ev.data;
  switch (m.type) {
    case 'start': start(m).catch((e) => post('closed', { reason: e.message, retry: true })); break;
    case 'in': transport?.sendInput(m.b); break;
    case 'dg': transport?.sendDatagram(m.b); break;
    case 'ctl': transport?.sendControl(m.m); break;
    case 'prefs': prefs = { ...prefs, ...m.prefs }; break;
    case 'displayed': onDisplayed(m.id, m.t); break;
    case 'stageDump': post('stageDump', { recs: lat.recs.map((r) => ({ ...r.raw, stages: r.s, e2e: r.e2e, fromCapture: r.fromCapture })) }); break;
    case 'close':
      if (transport) { transport.sendControl({ t: 'bye' }); setTimeout(() => transport?.close(), 50); }
      break;
  }
};
