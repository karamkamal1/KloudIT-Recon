// Stream worker: owns the network transport, the WebCodecs decoders and the
// OffscreenCanvas renderer so that nothing on the main thread (layout, GC,
// input handling) can delay a frame.
//
//   WebTransport (per-frame QUIC streams + datagrams) or WebSocket fallback
//     (over a high round trip WebTransport frames may come as datagram shards
//     with Reed-Solomon parity instead: fec.js rebuilds them)
//     -> reorder by sequence -> VideoDecoder (optimizeForLatency, at most 2 queued)
//     -> frame pacing (pacing.js): draw on decode (Lowest latency) or at the
//        next display refresh (Smooth)
//     -> desynchronized 2D canvas, WebGL2 texture upload or WebGPU external
//        texture (renderers.js), on a canvas sized to device pixels; WebGPU
//        upscales a picture shown larger than it streams with FSR 1 (fsr1.js)
//        and draws HDR10 streams from a copy of the decoded planes (hdr.js)
//   Opus datagrams -> AudioDecoder -> lock-free SharedArrayBuffer ring -> AudioWorklet

import * as P from './protocol.js';
import { runSelfTests, helloDecoder } from './decoder-selftest.js';
import { createRenderer, LABELS, PATHS, PICK, pickPath, Samples, withTimeout } from './renderers.js';
import { Pacer } from './pacing.js';
import { FecReceiver } from './fec.js';
import { HDR_CODECS, HDR_WHITE, hdrWhite, sourcePeak } from './hdr.js';

const td = new TextDecoder();
const post = (type, data = {}) => self.postMessage({ type, ...data });
const now = () => performance.now();

// ---------------------------------------------------------------------------
// State

let transport = null;
let prefs = {};
let byeReason = '';
let hostFeatures = []; // welcome.features
let renderer = null; // the active presentation path (see Presentation)

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
  hostDropped: new Set(), // seqs of the current generation the host reported dropped
  // seqs of the current generation the host left out on purpose (the frame
  // extension's "thinned" mask: temporal SVC thinning under congestion);
  // not losses: skipped at once (skipThinned). The last THIN_KEEP are kept.
  thinned: new Set(),
  fecLost: new Set(), // seqs of the current generation sent as datagram shards that could not be rebuilt (fec.js)
  // Reference recovery (VideoConfig.recovery "ltr" / "invalidate"): the loss
  // being recovered ({ gen, from, since, discarded }: nothing from seq `from`
  // on is decoded until a frame ends it), when the last recovery frame was
  // decoded ({ t, codec }), and the codecs whose decoder rejected one (their
  // losses then ask for key frames).
  recover: null,
  recoveredAt: null,
  refRejected: new Set(),
  queue: [], // chunks waiting for room in the decoder (feedDecoder)
  submitted: 0, // chunks submitted to this decoder
  queueMax: 0, // highest decodeQueueSize after a decode() this session
  waitingMax: 0, // most chunks waiting in video.queue at once this session
  selfTest: [], // decoder self-test per family (decoder-selftest.js)
  softwareFor: new Set(), // families decoded in software: their hardware decoder held frames back
  hwFailed: new Set(), // families decoded in software: their hardware decoder kept failing (HW_FAIL_LIMIT)
  hwFailNotice: null, // the family whose next configure tells the user about it
  errors: { n: 0, at: 0 }, // decoder errors in a row without a frame out, the last one's time
};

// A hardware decoder that keeps failing while the browser still reports it
// supported (its creation refused, a driver that rejects this stream's
// bitstream): HW_FAIL_LIMIT errors in a row without a frame out, each within
// HW_FAIL_WINDOW_MS of the one before, and the family decodes in software for
// the rest of this connection (the next one tries the hardware decoder again).
// Each further error in a row waits longer before the decoder is configured
// again (up to ERROR_BACKOFF_MS), so that a decoder failing at once does not
// spin.
const HW_FAIL_LIMIT = 3;
const HW_FAIL_WINDOW_MS = 10000;
const ERROR_BACKOFF_MS = 2000;
const FAMILY_NAMES = { h264: 'H.264', hevc: 'HEVC', av1: 'AV1' };

const clock = { offset: null, samples: [], pingId: 0, pings: new Map(), rtt: 0, minRtt: 0 };

const stats = {
  frames: 0, bytes: 0, decodeSum: 0, decodeN: 0, owdSum: 0, owdN: 0, totalSum: 0, sendSum: 0, totalN: 0,
  dropped: 0, skipped: 0, hostDropped: 0, thinned: 0, keyRequests: 0, lastPost: now(), totalMin: Infinity, totalMax: 0,
  recovered: 0, recoveredByKey: 0, recoveryDiscarded: 0, recoveryRejected: 0, keyFrames: 0, streamResets: 0,
  audioPackets: 0, audioLost: 0, freezes: 0, lastFreeze: 0, superseded: 0, supersededChunks: 0, lagMin: Infinity,
  stalls: 0,
};

// Freezes: the picture stood still more than FREEZE_MS longer than the
// source did. Between consecutive frames of a generation (seq + 1) the source
// may have been still (a still desktop sends nothing): the host's time
// between their encode-done stamps is allowed. After frames that were encoded
// but never drawn (dropped by the host, lost, skipped, discarded while
// waiting for a key frame or at an encoder switch) the source was busy, so
// only one frame interval is allowed, also across the encoder restart that
// follows; that counts the network, the decoder, loss recovery and urgent
// restarts. A new generation after nothing more of the old one than the last
// drawn frame (a still source, then a switch) is judged like consecutive
// frames. Counted for the session with the latest one's length, each logged
// with the frame that ended it (seq 0: the first frame of a new encoder
// generation). A pause (hidden tab) starts over. seen: generation -> the
// highest seq the client knows the encoder produced (received, or reported
// dropped).
const FREEZE_MS = 100;
// Stalls: the same measure over STALL_MS (GUIDE 2.5 compares the datagram +
// FEC mode with frame streams by these).
const STALL_MS = 50;
const freeze = { drawn: 0, sentUs: 0, gen: -1, seq: 0, seen: new Map() };

function freezeSeen(gen, seq) {
  if (seq > (freeze.seen.get(gen) ?? -1)) freeze.seen.set(gen, seq);
  if (freeze.seen.size > 8) freeze.seen.delete(freeze.seen.keys().next().value);
}

// The picture's stand-still before this drawn frame beyond what the source
// explains (ms), or 0 for the first frame. Frames the host left out on purpose
// in between (thinned) count as consecutive: the source's time between the
// two drawn frames is allowed.
function freezeStall(meta, sent, presented) {
  if (!freeze.drawn) return 0;
  const hostMs = (sent - freeze.sentUs) / 1000;
  const next = meta.gen === freeze.gen && (meta.seq === freeze.seq + 1 || onlyThinnedBetween(freeze.seq, meta.seq));
  const missed = !next && (meta.gen === freeze.gen || (freeze.seen.get(freeze.gen) ?? -1) > freeze.seq);
  return presented - freeze.drawn - (missed ? Math.min(hostMs, 1000 / (video.cfg?.fps || 60)) : hostMs);
}

const congestion = { owdHist: [], over: 0, lastSent: 0 };

// Rate reports (GUIDE 2.2): to hosts that list P.FEATURE_RATE_REPORT the
// client reports every RATE_REPORT_MS what it received (cumulative frames,
// bytes, losses, audio packets; the one-way delays of the frames since the
// last report; the newest frame; the decoder's backlog), and the host's rate
// controller decides the bitrate from them. Counters are cumulative so a lost
// report costs only its delay samples.
const RATE_REPORT_MS = 25;
const fb = { on: false, timer: 0, sent: 0, frames: 0, bytes: 0, lost: 0, audio: 0, owd: [], gen: -1, lastSeq: 0 };

function sendRateReport() {
  if (!transport) return;
  let flags = 0;
  let p50 = 0;
  let max = 0;
  if (fb.owd.length) {
    const o = fb.owd.sort((a, b) => a - b);
    p50 = o[(o.length - 1) >> 1];
    max = o[o.length - 1];
    flags |= P.RATE_REPORT_OWD;
    fb.owd = [];
  }
  if (fb.gen >= 0) flags |= P.RATE_REPORT_FRAME;
  // The video shards (datagram + FEC mode): the host sizes its parity from
  // their loss (the frames fec.js has accounted: received and never received
  // shards of the same frames).
  if (hostFeatures.includes(P.FEATURE_VIDEO_FEC)) flags |= P.RATE_REPORT_SHARDS;
  fb.sent++;
  const fs = fecRx.stats;
  transport.sendDatagram(P.rateReport({
    flags, gen: Math.max(0, fb.gen), timeMs: now(), lastSeq: fb.lastSeq, frames: fb.frames, bytes: fb.bytes,
    owdP50Us: p50 * 1000, owdMaxUs: max * 1000, lost: fb.lost, audio: fb.audio,
    // The decoder's backlog: chunks in the decoder and waiting in front of it
    // (MAX_DECODE_QUEUE), as checkDecoderBacklog counts it.
    decodeQueue: video.inflight.size + video.queue.length,
    shards: fs.counted, shardsLost: fs.shardsLost,
  }));
}

// A complete frame (parsed header h, buf.length bytes, last byte at recv).
// repaired: a frame sent as shards that needed a NACK (fec.js): its delay is
// the repair's round trip, not a queue, and is left out of the report's.
function rateReportFrame(h, bytes, recv, repaired) {
  fb.frames++;
  fb.bytes += bytes;
  if (clock.offset !== null && fb.owd.length < 1000 && !repaired) fb.owd.push(recv - hostToLocal(sentUs(h)));
  if (fb.gen < 0 || isNewerGen(h.gen, fb.gen)) {
    fb.gen = h.gen;
    fb.lastSeq = h.seq;
  } else if (h.gen === fb.gen && h.seq > fb.lastSeq) {
    fb.lastSeq = h.seq;
  }
}

const audio = { cfg: null, decoder: null, ring: null, port: null, lastSeq: -1, nextPts: 0, samples: 0, L: null, R: null };
// An audio packet further behind the last one than this (packets: 320 ms of
// 5 ms ones) is not late but of another stream: datagrams and the control
// stream are read apart, so the old stream's last packets can come after the
// new stream's config (which restarted the sequence), and taking them as the
// newest made the new stream's packets "late" until its sequence passed the
// old one's (silence for as long as the old stream had run, up to minutes).
// The sequence goes on from such a packet.
const AUDIO_MAX_LATE = 64;

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
  // The minimum RTT of the last 30 s tells the host whether this is a LAN
  // (5 ms Opus frames) or a WAN (10 ms; step 4.6).
  transport.sendDatagram(P.ping(id, t0, clock.minRtt));
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
  clock.minRtt = best.rtt;
}

// ---------------------------------------------------------------------------
// Transports

function b64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
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

/**
 * Read a frame stream; also returns when its first bytes arrived. A stream
 * the host reset (it cancelled the frame) returns reset: true with what came
 * of it: where the browser negotiated RESET_STREAM_AT (GUIDE 2.4) at least
 * the frame's header, else what was read before the reset.
 */
async function readAll(stream) {
  const r = stream.getReader();
  const chunks = [];
  let n = 0;
  let first = 0;
  let reset = false;
  for (;;) {
    let next;
    try {
      next = await r.read();
    } catch {
      reset = true;
      break;
    }
    if (next.done) break;
    if (!first) first = now();
    chunks.push(next.value);
    n += next.value.byteLength;
  }
  if (chunks.length === 1) return { buf: chunks[0], first, reset };
  const out = new Uint8Array(n);
  let o = 0;
  for (const c of chunks) { out.set(c, o); o += c.byteLength; }
  return { buf: out, first, reset };
}

// "Datagram + FEC" video mode (GUIDE 2.5, fec.js): over a high round trip the
// host may send a frame as datagram shards with Reed-Solomon parity instead of
// on its own stream. fecRx rebuilds the frame and hands it on exactly as a
// frame stream would (onFrameBytes); the shards a frame lacks once its shards
// stop coming it asks for again (NACK) while there is time, then gives the
// frame up (onFecLost: a loss, as a gap that outlasted its wait). Clients on
// WebTransport offer it in their hello (hello.fec); WebSocket has no
// datagrams.
const fecRx = new FecReceiver({
  deliver: (buf, recv, first, repaired) => onFrameBytes(buf, recv, first, repaired),
  lost: (gen, seq, why) => onFecLost(gen, seq, why),
  // A NACK is due before the frame's give-up time: the transport's input
  // class where it has one (GUIDE 2.7's send priorities: its telemetry
  // datagrams are dropped while their queue stands still), else its only
  // datagram writer.
  nack: (b) => (transport?.sendInputDatagram ?? transport?.sendDatagram)?.(b),
  rtt: () => clock.minRtt || clock.rtt,
  interval: () => 1000 / (video.cfg?.fps || 60),
});
const fecTimer = { id: 0, at: Infinity };
// Whether the video comes as shards now (the overlay's Transport row): the
// host switches between shards and frame streams with the round trip, and
// fecRx's counters cover the whole session. on: frames were rebuilt from
// shards in the last stats period, or none were drawn then and they were
// before; frames: fecRx's frame count at that period's end.
const fecMode = { frames: 0, on: false };

// Runs fecRx's NACKs, give-ups and loss accounting when due.
function fecSchedule() {
  const due = fecRx.nextDue();
  if (due >= fecTimer.at) return;
  clearTimeout(fecTimer.id);
  fecTimer.at = due;
  if (due === Infinity) return;
  fecTimer.id = setTimeout(() => {
    fecTimer.at = Infinity;
    fecRx.tick(now());
    fecSchedule();
  }, Math.max(1, due - now()));
}

function onFecShard(d) {
  fecRx.shard(d, now());
  fecSchedule();
}

// A frame sent as shards that could not be rebuilt in time: lost (not
// reported by the host), at once instead of after the gap timeout.
function onFecLost(gen, seq, why) {
  const cfg = video.cfg;
  if (!cfg || gen !== cfg.gen || gen === video.lostGen || seq < video.expectSeq) return;
  post('log', { text: `frame ${gen}/${seq} lost: its shards could not be rebuilt in time (${why})` });
  video.fecLost.add(seq);
  if (video.fecLost.size > 256) video.fecLost.delete(video.fecLost.values().next().value);
  if (video.ready) checkGap();
}

// Send priorities (GUIDE 2.7). The client sends input (the input stream:
// keys, buttons, wheel, text; datagrams: mouse motion, gamepads), control (the
// control stream: key-frame requests, losses, settings) and telemetry
// (datagrams: frame acks, rate reports, pings). Input outranks control, which
// outranks telemetry, by WebTransport sendOrder where the browser schedules by
// it: in one send group (createSendGroup) where it has them, and with separate
// datagram writables for input and telemetry (datagrams.createWritable). Each
// is feature-detected (prio, in the stats). Without separate writables input
// and telemetry datagrams share one queue: while that queue does not move
// (its oldest telemetry write pending for TELEMETRY_STALL_MS: the browser
// cannot send) telemetry is dropped instead of queued, so input never waits
// behind a telemetry backlog (telemetry is lossy by design: rate reports are
// cumulative, acks and pings are samples). A pending write alone is no
// backlog: Chromium resolves datagram writes late enough that a cap of two
// pending writes dropped 3-28 % of the telemetry in the browser E2E, on
// paths with nothing to wait for. prio.telemetryStallMs is the longest the
// queue stood still.
const SEND_ORDER = { input: 1000, control: 100, telemetry: 10 };
const TELEMETRY_STALL_MS = 50;

async function openSendChannels(wt) {
  const prio = { sendOrder: false, sendGroup: false, datagramWritables: false, telemetrySent: 0, telemetryDropped: 0, telemetryStallMs: 0 };
  let group = null;
  if (typeof wt.createSendGroup === 'function') {
    try {
      group = wt.createSendGroup();
      prio.sendGroup = true;
    } catch {}
  }
  const opts = (sendOrder) => (group ? { sendGroup: group, sendOrder } : { sendOrder });
  const ctrl = await wt.createBidirectionalStream(opts(SEND_ORDER.control));
  const input = await wt.createBidirectionalStream(opts(SEND_ORDER.input));
  // Browsers that schedule by sendOrder hand out a WebTransportSendStream,
  // which has it as an attribute; older ones a plain WritableStream (and
  // ignore the option).
  prio.sendOrder = 'sendOrder' in input.writable;
  let dgInput = null;
  let dgTelemetry = null;
  if (typeof wt.datagrams.createWritable === 'function') {
    try { dgInput = wt.datagrams.createWritable(opts(SEND_ORDER.input)).getWriter(); } catch {}
    try { dgTelemetry = wt.datagrams.createWritable(opts(SEND_ORDER.telemetry)).getWriter(); } catch {}
  }
  prio.datagramWritables = !!(dgInput && dgTelemetry);
  dgInput ||= dgTelemetry || wt.datagrams.writable.getWriter();
  dgTelemetry ||= dgInput;
  return { ctrl, input, dgInput, dgTelemetry, prio };
}

/** The send function for telemetry datagrams on writer w (see above). */
function telemetrySender(w, prio, isClosed) {
  const pending = []; // start times of the writes not yet resolved, oldest first (they resolve in order)
  return (b) => {
    if (isClosed()) return;
    const t = performance.now();
    const stall = pending.length ? t - pending[0] : 0; // how long the queue has not moved
    prio.telemetryStallMs = Math.max(prio.telemetryStallMs, Math.round(stall));
    if (!prio.datagramWritables && stall > TELEMETRY_STALL_MS) {
      prio.telemetryDropped++;
      return;
    }
    prio.telemetrySent++;
    pending.push(t);
    w.write(b).catch(() => {}).finally(() => { pending.shift(); });
  };
}

async function openWebTransport(url, hashes, label, timeoutMs) {
  const opts = { requireUnreliable: true, congestionControl: 'low-latency' };
  if (hashes && hashes.length) {
    opts.serverCertificateHashes = hashes.map((h) => ({ algorithm: 'sha-256', value: b64(h) }));
  }
  const wt = new WebTransport(url, opts);
  wt.closed.catch(() => {});
  try {
    await withTimeout(wt.ready, timeoutMs, `WebTransport ${label}`);
  } catch (e) {
    try { wt.close(); } catch {}
    throw e;
  }
  const { ctrl, input, dgInput, dgTelemetry, prio } = await openSendChannels(wt);
  const cw = ctrl.writable.getWriter();
  const iw = input.writable.getWriter();
  const swallow = () => {};
  cw.write(Uint8Array.of(P.KIND_CONTROL)).catch(swallow);
  iw.write(Uint8Array.of(P.KIND_INPUT)).catch(swallow);
  let closed = false;
  return {
    kind: 'webtransport',
    path: label,
    prio,
    sendControl: (obj) => { if (!closed) cw.write(P.frameMsg(P.jsonBytes(obj))).catch(swallow); },
    sendInput: (b) => { if (!closed) iw.write(P.frameMsg(b)).catch(swallow); },
    sendInputDatagram: (b) => { if (!closed) dgInput.write(b).catch(swallow); },
    sendDatagram: telemetrySender(dgTelemetry, prio, () => closed), // acks, rate reports, pings
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
          readAll(value).then(({ buf, first, reset }) => (reset ? h.frameReset(buf) : h.frame(buf, now(), first))).catch(() => {});
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
    sendInputDatagram: (b) => send(P.WS_DATAGRAM, b),
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

// UDP relay (guide step 2.6): the gateway allocates a UDP port, the host
// binds to it from the inside, and the browser runs one QUIC connection with
// the host's WebTransport server through it (the host's certificate, pinned by
// hash, and a host ticket bound to the allocation).
async function allocateRelay(url) {
  const ac = new AbortController();
  const t = setTimeout(() => ac.abort(), 8000);
  let r, body;
  try {
    r = await fetch(url, { method: 'POST', credentials: 'same-origin', signal: ac.signal });
    body = await r.json().catch(() => ({}));
  } catch (e) {
    throw new Error(`relay allocation: ${e.name === 'AbortError' ? 'timed out' : e.message}`);
  } finally {
    clearTimeout(t);
  }
  if (!r.ok) {
    const e = new Error(`relay allocation: ${body.error || `HTTP ${r.status}`}`);
    e.portBlocked = r.status === 504; // the host's bind did not reach the relay port
    throw e;
  }
  return body;
}

async function connect(ep) {
  const attempts = [];
  const wtOK = typeof WebTransport !== 'undefined' && prefs.transport !== 'websocket';
  if (wtOK && ep.direct && prefs.path !== 'relay') {
    attempts.push(['direct', async () => ({ t: await openWebTransport(ep.direct.url, ep.direct.hashes, 'direct', 2500), ticket: ep.direct.ticket })]);
  }
  if (wtOK && ep.relay.udp && prefs.path !== 'direct' && !prefs.skipUdpRelay) {
    attempts.push(['relay', async () => {
      let a;
      try {
        a = await allocateRelay(ep.relay.udp);
        return { t: await openWebTransport(a.url, a.hashes, 'relay', 3000), ticket: a.ticket };
      } catch (e) {
        // The relay ports are probably blocked, for the browser or for the host:
        // skip them for a while.
        if (a || e.portBlocked) post('udpRelayFailed', {});
        throw e;
      }
    }]);
  }
  if (wtOK && prefs.path !== 'direct') {
    attempts.push(['relay-splice', async () => ({ t: await openWebTransport(ep.relay.wt, ep.relay.hashes, 'relay-splice', 6000), ticket: '' })]);
  }
  if (prefs.path !== 'direct') attempts.push(['websocket', async () => ({ t: await openWebSocket(ep.relay.ws), ticket: '' })]);
  let lastErr;
  for (const [label, fn] of attempts) {
    try {
      post('status', { text: `Connecting (${label})…` });
      return await fn();
    } catch (e) {
      lastErr = e;
      post('log', { text: `${label} failed: ${e.message}` });
    }
  }
  throw lastErr || new Error('no transport available');
}

// ---------------------------------------------------------------------------
// Presentation (guide step 4.3; the paths are in renderers.js). The main
// thread hands over one canvas per path the session may use (msg.canvases,
// in order) and how to choose (msg.present.mode):
//
//   setting   the user picked a path: its canvas only (2D on it if that path
//             does not work here)
//   auto      renderer "auto" with a stored result for this browser/OS: the
//             winner's canvas (2D on it if the winner no longer works)
//   bakeoff   renderer "auto" without one: a canvas per path; the paths that
//             work take turns on the live stream, their draw and display
//             stages (Phase 0) are measured, Auto's pick (pickPath) stays
//             and the main thread stores it (localStorage) with the numbers
//
// A path Auto picked that then fails FAIL_STREAK draws in a row (a lost
// context, frames that do not upload) is given up: the main thread forgets
// it and reconnects with the 2D canvas ('presentFailed'; a canvas keeps its
// context type). A path picked in the settings stays (errors in the overlay).
//
// The main thread shows the active renderer's canvas ('renderer', posted
// after the renderer's first frame), removes the canvases of paths that are
// gone ('gone'), and reports the canvas box in device pixels ('resize'):
// every renderer sizes its canvas to it, so the compositor never scales the
// picture.

const pres = {
  mode: 'setting',
  list: [], // renderers, each on its own canvas (r.slot: the path the canvas was made for)
  errors: {}, // path -> why it does not work here
  box: null, // [w, h] device pixels of the canvas box
  next: null, // the renderer that takes over before the next draw
  retire: [], // renderers dropped once the next one has drawn
  announce: false, // post 'renderer' after the next draw
  bake: null,
  drawErrors: 0,
  lastError: '',
  failStreak: 0, // draws in a row that failed
  refreshMs: 1000 / 60, // the display's refresh interval (main thread's measurement at page load)
};

const FAIL_STREAK = 30;

// Client-side upscaling (Phase 5, fsr1.js; prefs.upscale, sharpness,
// fsrDenoise, applied live): every renderer gets the setting, only WebGPU
// runs FSR (EASU + RCAS) and reports why not when it does not. The draw stage
// of the frames drawn with and without FSR this session (the CPU-side cost of
// the extra passes, where the device has no timestamp-query).
const upscalePrefs = () => ({ mode: prefs.upscale, sharpness: prefs.sharpness, denoise: prefs.fsrDenoise, input: prefs.fsrInput });
const drawUp = { fsr: new Samples(300), plain: new Samples(300) };

async function setupRenderers(msg) {
  pres.mode = msg.present?.mode || 'setting';
  if (msg.box?.w > 0 && msg.box?.h > 0) pres.box = [msg.box.w, msg.box.h];
  if (msg.client?.hz > 0) pres.refreshMs = 1000 / msg.client.hz;
  const log = (text) => post('log', { text });
  const opts = { log, upscale: upscalePrefs() };
  for (const [slot, c] of Object.entries(msg.canvases || {})) {
    let r = null;
    try {
      r = await createRenderer(slot, c, opts);
    } catch (e) {
      pres.errors[slot] = e.message;
      log(`${LABELS[slot] || slot} renderer unavailable (${e.message})`);
      if (pres.mode !== 'bakeoff' && slot !== 'canvas2d') {
        log('using the low-latency 2D canvas instead');
        r = await createRenderer('canvas2d', c, opts).catch(() => null); // the canvas may be claimed already
      }
    }
    if (!r) {
      post('gone', { slot });
      continue;
    }
    r.slot = slot;
    if (pres.box) r.resize(...pres.box);
    pres.list.push(r);
  }
  renderer = pres.list[0];
  if (!renderer) throw new Error('no renderer works in this browser');
  pres.announce = true;
  if (pres.mode === 'bakeoff') {
    const order = pres.list.map((r) => r.name);
    const per = (v) => Object.fromEntries(order.map((p) => [p, v()]));
    pres.bake = {
      order, slot: 0, t0: 0, slotStart: 0, lastT: 0, recs: per(() => Array.from({ length: BAKE.rounds }, () => [])), dur: per(() => 0), errors: per(() => 0),
      done: false, result: null,
    };
    post('log', { text: `presentation bake-off: ${order.join(', ')}` });
  }
}

function rendererInfo() {
  const r = renderer;
  return {
    name: r.name, slot: r.slot, mode: pres.mode, desynchronized: r.desynchronized, gpu: r.gpu, canvas: r.canvasSize(),
    errors: pres.errors, drawErrors: pres.drawErrors, bake: bakeState(),
    upscale: { ...r.upscaleInfo(), cpu: { fsr: drawUp.fsr.summary(), plain: drawUp.plain.summary() } },
    hdr: r.hdrInfo(),
  };
}

function applyUpscale() {
  for (const r of pres.list) r.setUpscale(upscalePrefs());
  try {
    renderer?.redraw(); // a still picture shows the change at once (WebGL2 / WebGPU)
  } catch (e) {
    renderError(e);
  }
}

// ---------------------------------------------------------------------------
// HDR10 (guide step 4.5, hdr.js). The client offers HDR to the host (hello
// and settings prefs.hdr) when the user's setting is Auto, the display is in
// HDR mode (the main thread's matchMedia "(dynamic-range: high)"), the
// renderer that draws is WebGPU (not during Auto's bake-off: the 2D canvas
// and WebGL2 never get HDR streams) and its canvas keeps extended range
// (WebGPURenderer.hdrCanvasCheck), and lists its 10-bit decoders
// (isConfigSupported of HEVC Main 10 / AV1 10-bit); the host decides
// (internal/host/hdr.go). Frames of an HDR10 generation are copied plane by
// plane before the frame pacer (hdrPrepare: one copy at a time, a newer frame
// supersedes one waiting for it; the copy counts in the draw stage) and drawn
// with extended range, or tone-mapped to SDR while the setting is Off, the
// display is not HDR or the canvas lacks extended range (until the host's SDR
// generation comes). A frame that cannot take that path (renderer.hdrBlocked:
// format null, which Chrome's hardware decoders give for 10-bit frames; a
// failed copy; failed HDR shaders; another renderer) withdraws the offer
// (hdrWithdraw): the family's decoder, or the canvas, and the page sends the
// host a settings message, which brings an SDR generation; meanwhile Chrome's
// SDR conversion (importExternalTexture) draws. Withdrawn families stay
// withdrawn for the page's later connections (prefs.hdrWithdrawn).

const hdr = {
  mode: 'auto', display: false, white: HDR_WHITE, space: 'srgb',
  canvas: { ok: false, why: 'no renderer yet' }, // the drawing renderer's extended-range check
  decoders: [], // 10-bit decoders: [{ family, hw }]
  withdrawn: {}, // family -> why its frames cannot be drawn as HDR (hdrWithdraw)
  opaqueTest: false, // test hook (hdrOpaque): HDR frames count as format null
  hostOffers: false, // welcome feature hdr10 (host config "hdr": "auto")
  copying: false, next: null, // hdrPrepare's copy and the frame waiting for it
  check: null, // test hook (hdrCheck): canvas pixels and decoded codes at points
  failedLog: new Set(),
};

// Why this client cannot present HDR ('' when it can).
function hdrWhy() {
  if (!hdr.canvas.ok) return hdr.canvas.why;
  if (!hdr.decoders.length) {
    const w = Object.keys(hdr.withdrawn);
    return w.length ? `no 10-bit decoder whose frames can be drawn as HDR (${w.map((f) => `${f}: ${hdr.withdrawn[f]}`).join('; ')})`
      : 'no 10-bit decoder (HEVC Main 10 or AV1 10-bit) in this browser';
  }
  if (hdr.mode !== 'auto') return 'HDR is Off in the settings';
  if (!hdr.display) return 'the display is not in HDR mode';
  return '';
}

// An HDR10 generation's frame cannot take the HDR path (b: renderer.hdrBlocked):
// the offer is withdrawn (the family's decoder for a format, the canvas for
// the renderer) and the page tells the host (a settings message: an SDR
// generation follows). Once per family / renderer.
function hdrWithdraw(b, family) {
  if (b.scope === 'lost') return; // the renderer is replaced (its successor's frames decide), or the frame closed
  if (b.scope === 'renderer') {
    if (!hdr.canvas.ok) return;
    hdr.canvas = { ok: false, why: b.why };
  } else {
    if (hdr.withdrawn[family]) return;
    hdr.withdrawn[family] = b.why;
    hdr.decoders = hdr.decoders.filter((d) => d.family !== family);
  }
  post('log', { text: `HDR: withdrawn for ${b.scope === 'renderer' ? 'this renderer' : `${family} streams`} (${b.why}); asking the host for SDR` });
  post('hdrWithdrawn', { family: b.scope === 'renderer' ? null : family, why: b.why });
  applyHdr(false);
}

// The prefs.hdr the host gets (hello and every settings message).
const hdrPrefs = () => ({ mode: hdr.mode, display: hdr.display, canvas: hdr.canvas.ok, decoders: hdr.decoders.map((d) => d.family), why: hdrWhy() });

// The renderer's output for HDR frames: extended range when the client could
// ask for HDR now, else tone mapping (and why).
function applyHdr(redraw = true) {
  const why = hdrWhy();
  for (const r of pres.list) r.setHdr({ want: !why, why, white: hdr.white, space: hdr.space, peak: sourcePeak(video.cfg?.hdrMetadata) });
  if (!redraw) return;
  try {
    renderer?.redraw(); // a still HDR picture shows the change at once
  } catch (e) {
    renderError(e);
  }
}

// 10-bit decoders of the HDR10 codecs (prefer-hardware first, as the stream's).
async function hdrDecoders() {
  const out = [];
  for (const [family, codec] of Object.entries(HDR_CODECS)) {
    const q = (hardwareAcceleration) => VideoDecoder.isConfigSupported({ codec, codedWidth: 1920, codedHeight: 1080, hardwareAcceleration })
      .then((r) => r.supported).catch(() => false);
    const hw = prefs.decoder !== 'software' && (await q('prefer-hardware'));
    if (hw || (await q('no-preference'))) out.push({ family, hw });
  }
  return out;
}

// Copies an HDR10 frame's planes (renderer.prepare), then hands it to the
// pacer; one copy at a time, a newer frame replaces the one waiting.
function hdrPrepare(item) {
  if (hdr.copying) {
    if (hdr.next) dropFrame(hdr.next, 'superseded');
    hdr.next = item;
    return;
  }
  hdr.copying = true;
  const r = renderer;
  const f = item.frame;
  const fmt = f.format;
  const family = video.cfg?.family;
  const check = hdr.check;
  hdr.check = null;
  r.prepare(f, (buf, layout, planes) => {
    // The latency probe reads the barcode from the copy (the staging buffer is reused).
    const vr = f.visibleRect;
    const cell = probe.mode === 'seq' ? P.BARCODE_CELL : P.visibleArea(video.cfg, vr.width, vr.height, f.displayWidth, f.displayHeight).w / P.BARCODE_WALLCLOCK_CELLS;
    const p0 = plane0(fmt);
    if (probe.mode !== 'off' && p0 && P.BARCODE_COLS * cell <= vr.width && P.BARCODE_ROWS * cell <= vr.height) {
      planes.luma = lumaFromPixels(buf, layout[0].offset, layout[0].stride, p0, cell);
      planes.lumaMethod = `copyTo ${fmt} (HDR planes)`;
    }
    if (check) planes.capture = { points: check.points, codes: hdrCodes(buf, layout, planes.layout, check.points), resolve: (res) => post('hdrCheck', { result: { ...res, codes: planes.capture.codes, format: fmt, frameColorSpace: f.colorSpace?.toJSON?.() } }) };
  }).then((planes) => {
    item.planes = planes;
    item.copied = now();
  }, (e) => {
    // This format has no working plane path: its frames draw through
    // importExternalTexture (Chrome's SDR conversion) from now on.
    if (!r.destroyed && !r.lost) {
      r.hdr.failed[fmt] = `copying ${fmt} frames failed (${e.message})`;
      hdrWithdraw({ scope: r.hdr.error ? 'renderer' : 'format', why: r.hdr.error || r.hdr.failed[fmt] }, family);
    }
    if (!hdr.failedLog.has(fmt)) {
      hdr.failedLog.add(fmt);
      post('log', { text: `HDR: copying ${fmt || 'opaque'} frames failed (${e.message}); drawing them through importExternalTexture` });
    }
    if (check) hdr.check = check;
  }).finally(() => {
    hdr.copying = false;
    if (f.codedWidth) pacer.offer(item);
    else item.planes?.release(); // closed meanwhile (teardown)
    const n = hdr.next;
    hdr.next = null;
    if (n) hdrPrepare(n);
  });
}

// The 10-bit codes [Y, Cb, Cr] at points of a copied frame (nearest chroma sample; test hook).
function hdrCodes(buf, layout, L, points) {
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const bytes = L.planes[0].format.startsWith('r8') || L.planes[0].format.startsWith('rg8') ? 1 : 2;
  const at = (i, x, y, k = 0, n = 1) => {
    const o = layout[i].offset + y * layout[i].stride + (x * n + k) * bytes;
    return (bytes === 2 ? dv.getUint16(o, true) : dv.getUint8(o)) * L.scale;
  };
  return points.map(([x, y]) => {
    const cx = Math.floor(x / L.sub[0]);
    const cy = Math.floor(y / L.sub[1]);
    return L.semi ? [at(0, x, y), at(1, cx, cy, 0, 2), at(1, cx, cy, 1, 2)] : [at(0, x, y), at(1, cx, cy), at(2, cx, cy)];
  });
}

// The next renderer takes over before a draw; it is announced after it.
function switchRenderer() {
  const r = pres.next;
  pres.next = null;
  if (!r || r === renderer) return;
  renderer.idle();
  renderer = r;
  pres.announce = true;
  pres.failStreak = 0;
}

function announceRenderer() {
  pres.announce = false;
  post('renderer', { info: rendererInfo() });
  if (pres.retire.length) post('log', { text: `presentation: ${renderer.name} draws; ${pres.retire.map((r) => r.name).join(', ')} released` });
  for (const r of pres.retire) {
    try { r.destroy(); } catch {}
    post('gone', { slot: r.slot });
  }
  pres.retire = [];
}

function renderError(e) {
  pres.drawErrors++;
  if (e.message !== pres.lastError) post('log', { text: `render error (${renderer.name}): ${e.message}` });
  pres.lastError = e.message;
}

// After every draw: Auto gives up on a path it picked once FAIL_STREAK draws
// in a row failed (see Presentation above).
function drawResult(ok) {
  pres.failStreak = ok ? 0 : pres.failStreak + 1;
  if (pres.failStreak !== FAIL_STREAK || renderer.name === 'canvas2d') return;
  if (pres.mode !== 'auto' && !(pres.mode === 'bakeoff' && pres.bake?.done)) return;
  post('log', { text: `presentation: ${renderer.name} failed ${FAIL_STREAK} draws in a row (${pres.lastError}); reconnecting with the 2D canvas` });
  post('presentFailed', { path: renderer.name, reason: pres.lastError });
}

function onResize(w, h) {
  if (!(w > 0 && h > 0)) return;
  pres.box = [w, h];
  for (const r of pres.list) r.resize(w, h);
  try {
    renderer?.redraw(); // WebGL2 / WebGPU still hold the last picture; 2D resizes with the next frame
  } catch (e) {
    renderError(e);
  }
}

// Presentation bake-off: after BAKE.warmupMs of streaming (decoder warm-up)
// the paths take turns, A B C C B A (no path always measured first),
// BAKE.slotMs of drawn frames each (the first BAKE.skipMs after a switch do
// not count). Meanwhile display marks are taken as often as the main thread
// answers them. Per path: the draw and display stages (p50, p95, mean), the
// draw p50 per round, the frames per second it drew and its failed draws
// (they count for nothing else); renderers.js pickPath() picks from them.
// The main thread keeps everything off the canvas while this runs. The
// result names the frame pacing mode(s) it ran in (step 4.4; the draw stage
// does not depend on it, the display stage does, alike for every path).
const BAKE = { warmupMs: 2000, rounds: 2, slotMs: 1500, skipMs: 250 };

const baking = () => !!pres.bake && !pres.bake.done;

// Slot i's path: forward in even rounds, backward in odd ones.
function bakePath(b, i) {
  const n = b.order.length;
  const k = i % n;
  return b.order[Math.floor(i / n) % 2 ? n - 1 - k : k];
}

// After each draw at time t, with its stage record (null: the draw failed).
// A slot's time runs from its first frame; the frame rate counts the time
// between counted frames (gaps capped at 100 ms, so a pause does not count).
function bakeTick(t, rec) {
  const b = pres.bake;
  if (!b || b.done) return;
  const path = renderer.name;
  if (!rec && path in b.errors) b.errors[path]++;
  const gap = Math.min(t - b.lastT, 100);
  b.lastT = t;
  if (!b.t0) b.t0 = t;
  if (t - b.t0 < BAKE.warmupMs) return;
  if (!b.slotStart) b.slotStart = t;
  if (rec && t - b.slotStart >= BAKE.skipMs && b.recs[path]) {
    b.recs[path][Math.floor(b.slot / b.order.length)].push(rec);
    b.dur[path] += gap;
  }
  if (t - b.slotStart < BAKE.slotMs) return;
  b.slotStart = 0;
  if (++b.slot >= b.order.length * BAKE.rounds) {
    bakeFinish();
    return;
  }
  pres.next = pres.list.find((r) => r.name === bakePath(b, b.slot));
}

function bakeStat(v) {
  const s = pct(v);
  return s ? { p50: s.p50, p95: s.p95, mean: +(v.reduce((a, x) => a + x, 0) / v.length).toFixed(2), n: s.n } : { n: 0 };
}

function bakeFinish() {
  const b = pres.bake;
  b.done = true;
  const results = {};
  for (const path of PATHS) {
    const r = pres.list.find((x) => x.name === path);
    if (!r) {
      results[path] = { error: pres.errors[path] || 'not tried' };
      continue;
    }
    const recs = b.recs[path].flat();
    const draw = recs.map((x) => x.s[ST.draw]);
    const display = recs.filter((x) => x.s[ST.display] !== null).map((x) => x.s[ST.display]);
    results[path] = {
      desynchronized: r.desynchronized, gpu: r.gpu, draw: bakeStat(draw), display: bakeStat(display),
      drawRounds: b.recs[path].map((v) => pct(v.map((x) => x.s[ST.draw]))?.p50 ?? null),
      fps: b.dur[path] ? +((1000 * recs.length) / b.dur[path]).toFixed(1) : 0, errors: b.errors[path], lost: !!r.lost,
    };
  }
  const pick = pickPath(results, pres.refreshMs);
  for (const [p, why] of Object.entries(pick.out)) results[p].out = why;
  const winner = pick.winner;
  const pacing = pacingOf(PATHS.flatMap((p) => b.recs[p]?.flat() || []));
  b.result = { winner, why: pick.why, results, pacing, rule: { ...PICK, refreshMs: +pres.refreshMs.toFixed(2), rounds: BAKE.rounds, slotMs: BAKE.slotMs, skipMs: BAKE.skipMs } };
  const txt = (p) => {
    const x = results[p];
    return x.error ? `${p} unavailable` : `${p} draw p50 ${x.draw.p50 ?? '—'} ms (rounds ${x.drawRounds.join('/')}), display p50 ${x.display.p50 ?? '—'} ms, ${x.fps} fps${x.out ? ` (${x.out})` : ''}`;
  };
  post('log', { text: `presentation bake-off (frame pacing ${pacing}): ${winner ? `${winner} (${pick.why})` : 'inconclusive'}; ${PATHS.map(txt).join('; ')}` });
  post('bakeoff', { result: b.result });
  // The winner stays (inconclusive: the first path); the others go once it has drawn.
  const keep = pres.list.find((r) => r.name === winner) || pres.list[0];
  pres.next = keep;
  pres.retire = pres.list.filter((r) => r !== keep);
  pres.list = [keep];
  pres.announce = true;
}

function bakeState() {
  const b = pres.bake;
  if (!b) return null;
  if (b.done) return { done: true, ...b.result };
  const warming = !b.t0 || now() - b.t0 < BAKE.warmupMs;
  return { done: false, warming, path: renderer?.name, slot: b.slot + 1, slots: b.order.length * BAKE.rounds, order: b.order };
}

// ---------------------------------------------------------------------------
// Video

const isNewerGen = (a, b) => { const d = (a - b) & 0xff; return d > 0 && d < 128; };

// Decoder hygiene (guide step 4.1): prefer-hardware + optimizeForLatency;
// flush() is never called while streaming (it waits for every output and
// makes the next chunk a key frame): the backlog recovery and decoder errors
// reset and reconfigure instead. A family whose hardware decoder held frames
// back in the startup self-test decodes in software when that passed.
async function configureDecoder(cfg) {
  video.ready = false;
  if (video.decoder && video.decoder.state !== 'closed') {
    try { video.decoder.close(); } catch {}
  }
  video.inflight.clear();
  video.queue = [];
  video.submitted = 0;
  const base = { codec: cfg.codec, optimizeForLatency: true, codedWidth: cfg.codedWidth || cfg.width, codedHeight: cfg.codedHeight || cfg.height };
  if (cfg.hdr && cfg.colorSpace) base.colorSpace = cfg.colorSpace; // HDR10: BT.2020 PQ (the bitstream says so too)
  const failed = video.hwFailed.has(cfg.family);
  const avoidHW = video.softwareFor.has(cfg.family) || failed;
  const wantHW = prefs.decoder !== 'software' && !avoidHW;
  if (avoidHW && prefs.decoder !== 'software') {
    post('log', { text: `${cfg.codec}: decoding in software, the hardware decoder ${failed ? 'kept failing' : 'held frames back in the self-test'}` });
  }
  let config = { ...base, hardwareAcceleration: wantHW ? 'prefer-hardware' : 'prefer-software' };
  let support = await VideoDecoder.isConfigSupported(config).catch(() => ({ supported: false }));
  // Without a decoder of the preferred kind, no-preference gets the other
  // kind (Chrome has no software HEVC decoder: Prefer software decodes HEVC
  // in hardware).
  video.hw = support.supported ? wantHW : !wantHW;
  if (!support.supported) {
    if (!wantHW) post('log', { text: `${cfg.codec}: no software decoder, decoding in hardware` });
    config = { ...base, hardwareAcceleration: 'no-preference' };
    support = await VideoDecoder.isConfigSupported(config).catch(() => ({ supported: false }));
  }
  if (!support.supported) {
    post('notice', { level: 'error', msg: `This browser cannot decode ${cfg.codec}. Pick another codec in settings.` });
    return false;
  }
  if (video.hwFailNotice === cfg.family) {
    video.hwFailNotice = null;
    const name = FAMILY_NAMES[cfg.family] || cfg.codec;
    post('notice', video.hw
      ? { level: 'error', msg: `The hardware ${name} decoder keeps failing, and this browser has no software ${name} decoder. Pick another codec in settings.` }
      : { level: 'warn', msg: `The hardware ${name} decoder keeps failing: decoding in software for this connection.` });
  }
  if (cfg !== video.cfg) return false; // superseded while awaiting
  video.decoder = new VideoDecoder({ output: onDecoded, error: onDecodeError });
  video.decoder.ondequeue = feedDecoder; // room in the decoder (browsers without the event: see feedDecoder)
  video.decoder.configure(config);
  video.ready = true;
  return true;
}

function onDecodeError(e) {
  post('log', { text: `decoder error: ${e.message}` });
  const t = now();
  const errs = video.errors;
  errs.n = t - errs.at <= HW_FAIL_WINDOW_MS ? errs.n + 1 : 1;
  errs.at = t;
  const family = video.cfg?.family;
  if (video.hw && family && errs.n >= HW_FAIL_LIMIT && !video.hwFailed.has(family)) {
    video.hwFailed.add(family);
    video.hwFailNotice = family;
    errs.n = 0; // the software decoder starts afresh
    post('log', { text: `the hardware decoder failed ${HW_FAIL_LIMIT} times in a row (${video.cfg.codec}): decoding in software for this connection` });
  }
  if (dropTest.run && !dropTest.run.error) dropTest.run.error = e.message;
  // Right after a recovery frame: this decoder does not take recovery frames
  // after skipped ones (docs/VENDOR_NOTES.md 3.5); this codec's losses ask
  // for key frames from now on.
  const ra = video.recoveredAt;
  if (ra && now() - ra.t < RECOVERY_REJECT_MS && !video.refRejected.has(ra.codec)) {
    video.refRejected.add(ra.codec);
    stats.recoveryRejected++;
    post('log', { text: `the decoder rejected a recovery frame (${ra.codec}, ${video.hw ? 'hardware' : 'software'}): losses ask for key frames from now on` });
  }
  video.recoveredAt = null;
  requestKeyframe('decoder error');
  if (!video.cfg) return;
  video.ready = false; // frames wait (onFrame) until the decoder is configured again
  const cfg = video.cfg;
  const again = () => { if (video.cfg === cfg) configureDecoder(cfg).then(() => drainEarly()); };
  const wait = Math.min(ERROR_BACKOFF_MS, 250 * Math.max(0, errs.n - 1));
  if (wait) setTimeout(again, wait);
  else again();
}

// Drop the current generation and wait for a fresh key frame. When send is
// false the caller asks the host for a restart some other way (congestion).
function requestKeyframe(reason, send = true) {
  const t = now();
  video.waitingKey = true;
  video.recover = null; // the key frame ends any recovery
  video.reorder.clear();
  // Nothing of this generation is decoded any more, also when the request is
  // not sent again so soon (its gaps must not count as losses).
  video.lostGen = video.cfg ? video.cfg.gen : -1;
  if (t - video.keyRequested < 400) return;
  video.keyRequested = t;
  stats.keyRequests++;
  post('log', { text: `requesting key frame (${reason})` });
  if (send) transport?.sendControl({ t: 'keyframe' });
}

// Watchdog: if we have been waiting for a key frame (or a new generation) for
// more than a second, ask again. A request can be dropped by the host's rate
// limits or lost with a connection hiccup; this guarantees video resumes.
function videoWatchdog() {
  feedDecoder();
  if (!transport || !video.cfg) return;
  const t = now();
  if (video.recover && t - video.recover.since > recoveryWait()) {
    // The recovery frame did not come (the report or the frame got lost, or
    // the encoder could not make one): fall back to a key frame.
    requestKeyframe('no recovery frame');
    return;
  }
  const waiting = video.waitingKey || video.lostGen === video.cfg.gen;
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
  video.hostDropped.clear();
  video.thinned.clear();
  video.fecLost.clear();
  video.gapSince = 0;
  video.lostGen = -1;
  video.recover = null;
  post('video', { cfg });
  applyHdr(false); // the stream's peak for tone mapping
  if (await configureDecoder(cfg)) drainEarly();
}

function drainEarly() {
  const early = video.early;
  video.early = [];
  early.sort((a, b) => a.seq - b.seq);
  for (const f of early) onFrame(f);
}

function onFrameBytes(buf, recv, first = recv, repaired = false) {
  const h = P.parseFrameHeader(buf);
  if (!h) return;
  h.data = buf.subarray(h.headerLen);
  h.recv = recv;
  h.first = first || recv;
  stats.bytes += buf.length;
  freezeSeen(h.gen, h.seq);
  rateReportFrame(h, buf.length, recv, repaired);
  if (!repaired) checkCongestion(recv - hostToLocal(sentUs(h)));
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
  if (f.ext?.thinned) noteThinned(f);
  skipThinned();
  if (f.seq < video.expectSeq) return; // duplicate, or late after its loss was handled
  if (f.seq > video.expectSeq) {
    video.reorder.set(f.seq, f);
    if (!video.gapSince) video.gapSince = now();
    checkGap();
    return;
  }
  decodeFrame(f);
  video.expectSeq++;
  decodeInOrder();
}

// Decode the buffered frames that are next in sequence (past the frames the
// host left out on purpose), then see whether the next one is missing.
function decodeInOrder() {
  for (skipThinned(); video.reorder.has(video.expectSeq); skipThinned()) {
    const n = video.reorder.get(video.expectSeq);
    video.reorder.delete(video.expectSeq);
    decodeFrame(n);
    video.expectSeq++;
  }
  video.gapSince = video.reorder.size ? now() : 0;
  checkGap();
}

// Temporal SVC thinning (Phase 5): under congestion the host leaves out
// frames no other frame references, and every frame it sends after one
// carries the "thinned" mask of those among the 32 before it. Their seqs are
// no gap: nothing waits for them, nothing is reported lost or recovered, the
// next frame decodes as it is (it references none of them).
const THIN_KEEP = 64;

function noteThinned(f) {
  for (const s of P.thinnedSeqs(f)) {
    if (s < video.expectSeq || video.thinned.has(s)) continue;
    video.thinned.add(s);
    stats.thinned++;
    // Datagram + FEC (fec.js): a frame thinned between two frames sent as
    // shards left a gap the receiver took for frames without a shard: no
    // more NACKs for it (the host has nothing to repair).
    fecRx.skip(f.gen, s);
  }
  for (const s of video.thinned) if (s + THIN_KEEP < f.seq) video.thinned.delete(s);
}

// Moves the next expected seq past frames the host left out on purpose.
function skipThinned() {
  while (video.thinned.has(video.expectSeq)) {
    video.hostDropped.delete(video.expectSeq);
    video.expectSeq++;
  }
}

// Whether every seq strictly between a and b (same generation) was left out
// on purpose.
function onlyThinnedBetween(a, b) {
  if (b <= a + 1 || b - a > 33) return false;
  for (let s = a + 1; s < b; s++) if (!video.thinned.has(s)) return false;
  return true;
}

// Frames travel on reliable streams: a gap in the sequence is a late frame
// (a retransmission takes a few round trips), not a lost one, until it lasts
// longer than that. Frames the host dropped are reported ({"t":"dropped"}).
const gapTimeout = () => Math.max(250, 4 * clock.rtt);

// The next frame in sequence is missing. It is lost when the host reported it
// dropped (act at once) or when the gap outlasts gapTimeout() (no report: an
// older host, or a frame lost on the way); until then frames wait in the
// reorder buffer, except a frame that ends a wait for a recovery frame
// (skipToRecovery).
function checkGap() {
  const cfg = video.cfg;
  if (!cfg || cfg.gen === video.lostGen) return;
  if (video.thinned.has(video.expectSeq)) { decodeInOrder(); return; } // left out on purpose: no gap
  if (video.recover && skipToRecovery()) return;
  const reported = video.hostDropped.has(video.expectSeq);
  // A frame sent as shards that fec.js gave up is lost at once (it waited
  // for its repairs already).
  const fecLost = video.fecLost.has(video.expectSeq);
  if (!reported) {
    if (!video.reorder.size) return;
    const timeout = gapTimeout();
    const maxBuffered = Math.max(30, 2 * Math.ceil(((cfg.fps || 60) * timeout) / 1000));
    if (!fecLost && now() - video.gapSince <= timeout && video.reorder.size <= maxBuffered) return;
  }
  frameLost(reported ? 'dropped by host' : fecLost ? 'shards lost' : 'frame lost');
}

// A confirmed loss of the frames from expectSeq on: the run the host
// reported, else everything up to the oldest buffered frame. Recovery "skip"
// (the encoder heals the picture by itself: intra refresh) continues with the
// next frame; "ltr" / "invalidate" (reference recovery) continue too, but
// decode nothing until the encoder's recovery frame (awaitRecovery);
// "keyframe" asks for a key frame (on the FFmpeg path a new encoder
// generation), as do a loss before this generation's key frame and reference
// recovery on a decoder that rejected a recovery frame.
function frameLost(reason) {
  const from = video.expectSeq;
  const reported = video.hostDropped.has(from);
  let to = from;
  while (video.hostDropped.has(to)) video.hostDropped.delete(to++);
  if (to === from) to = Math.min(...video.reorder.keys());
  for (let s = from; s < to; s++) video.fecLost.delete(s);
  let missing = to - from;
  for (let s = from; s < to; s++) if (video.thinned.has(s)) missing--; // left out on purpose: no loss
  stats.dropped += missing;
  if (!reported) fb.lost += missing; // lost on the way (the host knows its own drops)
  const mode = P.recoveryOf(video.cfg);
  const decoding = !video.waitingKey && video.decoder?.state === 'configured';
  if (decoding && mode === P.RECOVERY_SKIP) {
    stats.skipped += missing;
    post('log', { text: `skipping ${missing} lost frame(s) from ${video.cfg.gen}/${from} (${reason}); the encoder heals the picture` });
  } else if (decoding && P.isRefRecovery(mode) && !video.refRejected.has(video.cfg.codec)) {
    awaitRecovery(from, reported, reason);
  } else {
    video.gapSince = 0;
    requestKeyframe(reason);
    return;
  }
  video.expectSeq = to;
  for (const k of video.reorder.keys()) if (k < to) video.reorder.delete(k);
  decodeInOrder();
}

// Reference recovery: the last good picture stays on screen and nothing from
// seq `from` on is decoded (decodeFrame discards) until a frame that ends the
// wait (P.endsRecovery: a recovery frame whose references are all older than
// the oldest lost frame, or a key frame). A loss the host did not report is
// reported to it ({"t":"lost"}); it answers with a recovery frame. A later
// loss while waiting keeps the oldest one.
function awaitRecovery(from, reported, reason) {
  const cfg = video.cfg;
  if (!video.recover) {
    video.recover = { gen: cfg.gen, from, since: now(), discarded: 0 };
    post('log', { text: `lost frame(s) from ${cfg.gen}/${from} (${reason}): waiting for a recovery frame (${cfg.recovery})` });
  } else {
    video.recover.from = Math.min(video.recover.from, from);
  }
  if (!reported) transport?.sendControl({ t: P.MSG_LOST, gen: cfg.gen, fromSeq: from });
}

// While waiting for a recovery frame every frame before the one that ends the
// wait is discarded anyway: when that frame is buffered, continue with it at
// once instead of waiting for the late frames before it (which would keep the
// picture frozen and, past gapTimeout(), report them lost for a second
// recovery frame). Frames before it that still arrive are ignored (onFrame).
function skipToRecovery() {
  const r = video.recover;
  let to = Infinity;
  for (const [s, f] of video.reorder) if (s < to && P.endsRecovery(f, r.from)) to = s;
  if (to === Infinity) return false;
  let late = 0;
  for (let s = video.expectSeq; s < to; s++) {
    const buffered = video.reorder.delete(s);
    video.fecLost.delete(s);
    if (video.thinned.has(s)) continue; // left out on purpose: neither late nor discarded
    if (video.hostDropped.delete(s)) { stats.dropped++; continue; }
    if (!buffered) late++;
    r.discarded++;
    stats.recoveryDiscarded++;
  }
  if (late) post('log', { text: `frame ${r.gen}/${to} ends the recovery wait: not waiting for ${late} late frame(s) before it` });
  video.expectSeq = to;
  decodeInOrder();
  return true;
}

// How long to wait for a recovery frame before asking for a key frame: the
// host answers within a round trip and a frame (the helper repeats a still
// picture every 100 ms).
const RECOVERY_WAIT_MS = 1000;
const recoveryWait = () => Math.max(RECOVERY_WAIT_MS, 4 * clock.rtt);
// A decoder error this soon after a recovery frame was fed counts as the
// decoder rejecting it.
const RECOVERY_REJECT_MS = 1000;

// The frame f ends the wait for a recovery frame: decode it and resume.
function endRecovery(f) {
  const r = video.recover;
  video.recover = null;
  const t = now();
  if (f.key) {
    stats.recoveredByKey++;
  } else {
    stats.recovered++;
    video.recoveredAt = { t, codec: video.cfg?.codec };
  }
  const how = f.key ? 'key frame' : `recovery frame (refFloor ${f.ext.refFloor})`;
  post('log', { text: `recovered from the loss at ${r.gen}/${r.from} with ${how} ${f.gen}/${f.seq} after ${Math.round(t - r.since)} ms, ${r.discarded} frame(s) discarded` });
  const dt = dropTest.run;
  if (dt?.recovery && !dt.recoveredBy) dt.recoveredBy = { seq: f.seq, key: f.key, refFloor: f.ext?.refFloor ?? null, ms: Math.round(t - r.since), discarded: r.discarded };
}

// A frame stream the host reset (it cancelled the frame: GUIDE 2.3 rung 1,
// a frame the client would discard, a failed write) whose header arrived:
// with partial delivery (RESET_STREAM_AT, GUIDE 2.4) the host marks it
// reliable, without it the header may have been read before the reset. The
// frame is lost, as if the host had reported it dropped (its "dropped"
// message comes too, maybe later: a run of discarded frames is reported
// when it ends).
function onFrameReset(buf) {
  if (buf.byteLength < P.FRAME_HEADER_LEN || buf[0] !== P.FRAME_TYPE_VIDEO) return;
  stats.streamResets++;
  const gen = buf[2];
  const seq = new DataView(buf.buffer, buf.byteOffset, buf.byteLength).getUint32(4, true);
  freezeSeen(gen, seq);
  const cfg = video.cfg;
  if (!cfg || gen !== cfg.gen || gen === video.lostGen || seq < video.expectSeq) return;
  video.hostDropped.add(seq);
  if (video.ready) checkGap(); // else drainEarly() gets to it
}

// The host discarded frames it will never send: treat them as lost now.
function onDropped(m) {
  const d = P.parseDropped(m);
  if (!d) return;
  stats.hostDropped += d.count;
  freezeSeen(d.gen, d.from + d.count - 1);
  const cfg = video.cfg;
  if (!cfg || d.gen !== cfg.gen || d.gen === video.lostGen) return; // other generations are discarded anyway
  for (let s = Math.max(d.from, video.expectSeq); s < d.from + d.count; s++) video.hostDropped.add(s);
  if (video.ready) checkGap(); // else drainEarly() gets to it
}

// If the decoder cannot keep up (slow device, software decode), frames queue
// and latency grows without bound. Detect a sustained backlog (chunks in the
// decoder and waiting in front of it), drop it, and restart from a fresh key
// frame (and ask the host to back off). Software decoding chosen only because
// the hardware decoder held frames back goes back to hardware: a frame or
// two held back costs less than a decoder that cannot keep up. That restart
// asks for a key frame only: the client's own choice fell behind (the
// self-test's clip is 640x360), not the device, so the host keeps its
// bitrate and sets no decoder cap, and there is no overload notice.
const overload = { since: 0, warned: false };
function checkDecoderBacklog() {
  const d = video.decoder;
  if (!d || d.state !== 'configured') return false;
  const fps = video.cfg?.fps || 60;
  const backlog = video.inflight.size + video.queue.length;
  if (backlog <= Math.max(4, fps / 10)) { overload.since = 0; return false; }
  const t = now();
  if (!overload.since) overload.since = t;
  if (t - overload.since < 500) return false;
  overload.since = 0;
  try { d.reset(); } catch {}
  video.inflight.clear();
  const toHW = video.softwareFor.delete(video.cfg.family);
  configureDecoder(video.cfg).then(() => drainEarly());
  if (toHW) {
    post('log', { text: 'software decoder fell behind: back to the hardware decoder (it holds frames back)' });
    requestKeyframe('software decoder backlog');
    return true;
  }
  // One message: the host's congestion response lowers the bitrate *and*
  // restarts with a key frame, at once for this reason (other congestion
  // reports restart overlapped while the old generation streams on).
  requestKeyframe('decoder backlog', false);
  transport?.sendControl({ t: 'congestion', delayMs: 0, reason: P.CONGESTION_DECODER });
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
  if (f.key) stats.keyFrames++;
  if (video.recover) {
    if (!P.endsRecovery(f, video.recover.from)) {
      video.recover.discarded++;
      stats.recoveryDiscarded++;
      return;
    }
    endRecovery(f);
  }
  if (dropTest.armed && !f.key) {
    // Never decoded: the next frame references one the decoder has not seen.
    // Under reference recovery the client treats it as lost: it waits for
    // the recovery frame the host makes for it.
    dropTest.armed = false;
    const ref = P.isRefRecovery(P.recoveryOf(video.cfg)) && !video.refRejected.has(video.cfg.codec);
    dropTest.run = {
      gen: f.gen, seq: f.seq, codec: video.cfg?.codec, encoder: video.cfg?.encoder, hw: video.hw, at: now(), decoded: 0, error: null,
      recovery: ref ? video.cfg.recovery : null, recoveredBy: null,
    };
    post('log', { text: `drop test: skipping frame ${f.gen}/${f.seq} (${video.cfg?.codec}, ${video.hw ? 'hardware' : 'software'} decoder${ref ? `, waiting for a recovery frame (${video.cfg.recovery})` : ''})` });
    setTimeout(finishDropTest, DROP_TEST_MS);
    if (ref) awaitRecovery(f.seq, false, 'drop test');
    return;
  }
  if (f.key && video.queue.length) {
    // Decoding the frames before a key frame would only delay it: nothing
    // after it refers to them (never decoded: not acknowledged).
    stats.supersededChunks += video.queue.length;
    video.queue = [];
  }
  video.queue.push(f);
  feedDecoder();
  video.waitingMax = Math.max(video.waitingMax, video.queue.length);
}

// At most MAX_DECODE_QUEUE chunks wait inside the decoder (decodeQueueSize:
// submitted, not yet taken by the codec); later ones wait in video.queue,
// where the client can still act on them (a key frame supersedes them, the
// backlog recovery drops them), until the decoder has room: its 'dequeue'
// event, an output, the next frame or the watchdog (browsers without the
// event).
const MAX_DECODE_QUEUE = 2;

function feedDecoder() {
  const d = video.decoder;
  while (video.queue.length && d?.state === 'configured' && d.decodeQueueSize < MAX_DECODE_QUEUE) {
    const f = video.queue.shift();
    video.inflight.set(f.ptsUs, {
      recv: f.recv, first: f.first, sendUs: f.sendUs, ext: f.ext, seq: f.seq, gen: f.gen, t: now(), n: video.submitted++,
    });
    if (video.inflight.size > 120) video.inflight.delete(video.inflight.keys().next().value);
    try {
      d.decode(new EncodedVideoChunk({ type: f.key ? 'key' : 'delta', timestamp: f.ptsUs, data: f.data }));
      video.queueMax = Math.max(video.queueMax, d.decodeQueueSize);
    } catch (e) {
      onDecodeError(e);
      return;
    }
  }
}

let firstFrame = true;

// Every VideoFrame the worker holds (decoder outputs, probe clones) until it
// is closed. An open frame pins one of the decoder's output buffers, and a
// hardware decoder whose pool runs dry stalls (w3c/webcodecs#680), so each is
// closed as soon as it is drawn (the WebGPU renderer keeps exactly one,
// this.prev, until the next draw). Counted from the frames themselves (a
// closed frame has a coded width of 0), not from the code that closes them:
// frames.max is the most open at once; a frame still open after
// FRAME_LEAK_LIMIT newer ones is a leak: closed here and counted.
const FRAME_LEAK_LIMIT = 16;
const frames = { open: new Set(), max: 0, leaked: 0 };

function openFrames() {
  for (const f of frames.open) if (!f.codedWidth) frames.open.delete(f);
  for (const f of frames.open) {
    if (frames.open.size <= FRAME_LEAK_LIMIT) break;
    frames.open.delete(f);
    f.close();
    if (!frames.leaked++) post('log', { text: 'VideoFrame leak: a frame was never closed (closed now)' });
  }
  return frames.open.size;
}

function trackFrame(f) {
  frames.open.add(f);
  frames.max = Math.max(frames.max, openFrames());
}

// A decoded frame closed unseen (superseded, or stale in Smooth): its HDR
// planes are free again, and it counts as decoded.
function dropFrame(it, why) {
  it.planes?.release();
  it.frame.close();
  if (why === 'superseded') stats.superseded++;
  if (it.meta) ackFrame(it.meta, it.decoded);
}

// Frame pacing (step 4.4, pacing.js; prefs.pacing, applied live). Lowest
// latency draws on decode, one task after the output: outputs already queued
// behind it (a burst after a stall or a backlog, a decoder that releases
// frames together) supersede it, and only the newest is drawn. Smooth draws
// at the next display refresh (the worker's requestAnimationFrame), one frame
// waiting at most, a newer output replacing it. Frames dropped either way are
// closed unseen and acknowledged as decoded (stats.superseded; Smooth's stale
// frames: pacer.counts.stale). The wait is the "hold" stage.
const pacer = new Pacer({
  draw: (it) => drawFrame(it.frame, it.meta, it.decoded, it),
  drop: dropFrame,
  newerComing: () => video.inflight.size + video.queue.length > 0,
  refreshMs: () => pres.refreshMs, // until its ticks show the interval
  // Read on every request (a browser without it in workers: the main thread's ticks).
  raf: () => (typeof self.requestAnimationFrame === 'function' ? (cb) => self.requestAnimationFrame(cb) : null),
  mainTicks: (on) => post('ticks', { on }),
  now,
  log: (text) => post('log', { text }),
});

function onDecoded(frame) {
  trackFrame(frame);
  video.errors.n = 0; // the decoder works
  const meta = video.inflight.get(frame.timestamp);
  video.inflight.delete(frame.timestamp);
  const dt = dropTest.run;
  if (dt && meta && meta.gen === dt.gen && meta.seq > dt.seq) dt.decoded++;
  const decoded = now();
  if (meta) {
    // Output lag: chunks submitted after this one before it came out (a
    // decoder that holds frames back never gets below its hold).
    stats.lagMin = Math.min(stats.lagMin, video.submitted - meta.n - 1);
    stats.decodeSum += decoded - meta.t;
    stats.decodeN++;
  }
  feedDecoder();
  const item = { frame, meta, decoded };
  // HDR10 on the WebGPU renderer: the planes are copied first (hdrPrepare);
  // a frame that cannot take that path withdraws the HDR offer.
  if (video.cfg?.hdr && renderer) {
    const b = renderer.hdrBlocked(hdr.opaqueTest ? null : frame.format, frame.visibleRect);
    if (!b) {
      hdrPrepare(item);
      return;
    }
    hdrWithdraw(b, video.cfg.family);
  }
  pacer.offer(item);
}

// Frame acknowledgement (0x40): one-way delay and decode time of a decoded frame.
function ackFrame(meta, decoded) {
  if (clock.offset === null) return null;
  const owd = meta.recv - hostToLocal(sentUs(meta));
  transport?.sendDatagram(P.frameAck(meta.gen, meta.seq, owd * 1000, (decoded - meta.t) * 1000));
  return owd;
}

// pace: the pacer's item (via: what started the draw; tick: the refresh's start and
// refresh: the refresh interval it judged the frame by, in Smooth).
function drawFrame(frame, meta, decoded, pace) {
  const start = now(); // the hold ends
  // Padding the host announced (VideoConfig crop) is not shown.
  const vr = frame.visibleRect;
  const vis = P.visibleArea(video.cfg, vr ? vr.width : frame.displayWidth, vr ? vr.height : frame.displayHeight, frame.displayWidth, frame.displayHeight);
  const size = `${Math.round(vis.w)}x${Math.round(vis.h)}`;
  if (size !== video.lastSize) {
    video.lastSize = size;
    post('resolution', { w: Math.round(vis.w), h: Math.round(vis.h) });
    const c = video.cfg;
    if (c?.cropRight || c?.cropBottom) {
      post('log', { text: `padded picture: coded ${c.codedWidth}x${c.codedHeight} announced, decoder output ${vr?.width}x${vr?.height} (display ${frame.displayWidth}x${frame.displayHeight}), showing ${size}` });
    }
  }
  if (pres.next) switchRenderer();
  const req = probeStart(frame, meta, vis);
  // HDR planes copied for this renderer (another one took over: drawn as usual).
  let planes = pace?.planes;
  if (planes && planes.renderer !== renderer) {
    planes.release();
    planes = null;
  }
  if (req && planes?.luma) {
    req.luma = Promise.resolve(planes.luma);
    req.method = planes.lumaMethod;
  }
  let drew = true;
  try {
    renderer.draw(frame, req, vis, planes);
  } catch (e) {
    drew = false;
    frame.close();
    renderError(e);
  }
  const presented = now();
  if (req?.clone) trackFrame(req.clone);
  if (req) probeSample(req, presented);
  stats.frames++;
  if (meta) {
    const sent = sentUs(meta);
    const stall = freezeStall(meta, sent, presented);
    Object.assign(freeze, { drawn: presented, sentUs: sent, gen: meta.gen, seq: meta.seq });
    if (stall > STALL_MS) stats.stalls++;
    if (stall > FREEZE_MS) {
      stats.freezes++;
      stats.lastFreeze = stall;
      post('log', { text: `freeze: ${Math.round(stall)} ms longer than the source (until gen ${meta.gen} seq ${meta.seq})` });
    }
  }
  if (firstFrame) {
    firstFrame = false;
    post('firstFrame', { renderer: renderer.name });
  }
  if (pres.announce) announceRenderer();
  drawResult(drew);
  if (!meta || clock.offset === null) return;
  stats.owdSum += ackFrame(meta, decoded);
  stats.owdN++;
  // A failed draw put nothing on screen: no stage record (draw, display, e2e).
  const rec = drew ? recordStages(meta, decoded, start, presented, pace) : null;
  bakeTick(presented, rec);
  if (!rec) return;
  stats.totalSum += rec.e2e;
  stats.sendSum += rec.e2eSend;
  stats.totalN++;
  stats.totalMin = Math.min(stats.totalMin, rec.e2e);
  stats.totalMax = Math.max(stats.totalMax, rec.e2e);
}

// Debug toggle for the decoder checks in docs/VENDOR_NOTES.md (1.4, 3.5): does
// this browser's decoder accept a P-frame after a skipped frame (what recovery
// "skip" does), and under reference recovery the encoder's recovery frame
// after skipped frames? postMessage({type:'dropTest'}) drops the next delta
// frame before the decoder; after DROP_TEST_MS the result ({ decoded, error,
// recovery, recoveredBy, ... }) goes to the main thread
// (window.__recon.dropTest) and the log. Under "skip" the picture stays
// damaged until the encoder heals it (intra refresh) or a key frame; under
// reference recovery the drop is a loss the host is told of ({"t":"lost"}),
// and recoveredBy names the frame that ended it (key: a key frame instead of
// a recovery frame).
const DROP_TEST_MS = 2000;
const dropTest = { armed: false, run: null };

function finishDropTest() {
  const r = dropTest.run;
  if (!r) return;
  dropTest.run = null;
  const result = { ...r, ms: DROP_TEST_MS, ok: !r.error && r.decoded > 0 && (!r.recovery || !!r.recoveredBy) };
  delete result.at;
  const rb = r.recoveredBy;
  const rec = !r.recovery ? '' : !rb ? '; no recovery frame came'
    : rb.key ? `; a key frame (${r.gen}/${rb.seq}) ended the wait after ${rb.ms} ms, not a recovery frame`
      : `; recovery frame ${r.gen}/${rb.seq} (refFloor ${rb.refFloor}) after ${rb.ms} ms, ${rb.discarded} frame(s) discarded`;
  post('log', { text: `drop test: ${result.ok ? 'decoder accepted' : 'decoder did NOT accept'} the frames after the skipped one: ${r.decoded} decoded in ${DROP_TEST_MS} ms${rec}${r.error ? `, error: ${r.error}` : ''}` });
  post('dropTest', { result });
}

// ---------------------------------------------------------------------------
// Per-stage latency. Host stamps (capture, encodeDone: frame header extension;
// send: header) are host-clock µs, converted with the clock sync; the client
// adds first/last byte, decode submit/output, draw start, drawn and displayed
// (estimate: the main thread's next requestAnimationFrame after the draw,
// sampled).
//
//   capture→encodeDone | host queue | network (send→first byte) | transfer |
//   reorder/wait (last byte→submit) | decode | hold (output→draw start: the
//   frame pacing wait, step 4.4) | draw | display (est.)
//
// End-to-end runs from capture (or send, when the host cannot stamp the
// capture) to drawn and is the per-frame sum of the stages in that span; the
// sampled display estimate comes on top. Hosts without the welcome feature
// stage-hold get hold and draw as one draw row (decoder output → drawn).

const STAGES = ['capture', 'queue', 'network', 'transfer', 'wait', 'decode', 'hold', 'draw', 'display'];
const ST = Object.fromEntries(STAGES.map((name, i) => [name, i]));
// Frames of the native encoder helper also carry the game's present and the
// encoder submit time: present->capture (before end-to-end starts), and
// capture->encoded split into capture->submit and encode. Not in the sum.
const DETAIL_STAGES = ['present', 'submit', 'encode'];
const STAGE_WINDOW_MS = 10000;
const DISPLAY_SAMPLE_MS = 50; // display marks: ~20/s keeps the main thread's rAF work small
const lat = { recs: [], pending: null, markId: 0, lastMark: 0, lastReport: now() };

function recordStages(m, decoded, start, drawn, pace) {
  const capUs = m.ext?.captureUs;
  const doneUs = m.ext?.encodeDoneUs;
  const presUs = m.ext?.presentUs;
  const subUs = m.ext?.encodeSubmitUs;
  const d = [
    presUs !== undefined && capUs !== undefined ? (capUs - presUs) / 1000 : null,
    subUs !== undefined && capUs !== undefined ? (subUs - capUs) / 1000 : null,
    subUs !== undefined && doneUs !== undefined ? (doneUs - subUs) / 1000 : null,
  ];
  const sendL = hostToLocal(m.sendUs);
  const s = new Array(STAGES.length).fill(null);
  if (capUs !== undefined && doneUs !== undefined) s[0] = (doneUs - capUs) / 1000;
  if (doneUs !== undefined) s[1] = (m.sendUs - doneUs) / 1000;
  s[2] = m.first - sendL;
  s[3] = m.recv - m.first;
  s[4] = m.t - m.recv;
  s[5] = decoded - m.t;
  // An HDR frame's plane copy (between the decoder's output and the pacer)
  // counts in the draw stage: hold stays the pacing wait.
  const copy = pace?.copied ? pace.copied - decoded : 0;
  s[ST.hold] = start - decoded - copy;
  s[ST.draw] = drawn - start + copy;
  const fromCapture = s[0] !== null;
  const pacing = pace?.via === 'hop' ? 'latency' : 'smooth';
  const up = renderer.upscaled; // FSR drew it: its passes are in the draw stage
  (up ? drawUp.fsr : drawUp.plain).push(s[ST.draw]);
  const rec = {
    t: drawn, s, d, e2e: drawn - (fromCapture ? hostToLocal(capUs) : sendL), e2eSend: drawn - sendL, fromCapture, path: renderer.name, pacing, up,
    raw: {
      presentUs: presUs, captureUs: capUs, encodeSubmitUs: subUs, encodeDoneUs: doneUs, sendUs: m.sendUs, offset: clock.offset,
      first: m.first, last: m.recv, submit: m.t, output: decoded,
      drawStart: start, drawn, pacing, via: pace?.via, tick: pace?.tick ?? null, refresh: pace?.refresh ?? null, upscaled: up,
      copied: pace?.copied ?? null,
    },
  };
  lat.recs.push(rec);
  while (lat.recs.length && lat.recs[0].t < drawn - STAGE_WINDOW_MS) lat.recs.shift();
  // At most one display mark in flight: the main thread answers within a
  // refresh. The presentation bake-off takes one whenever it can.
  if (lat.pending && drawn - lat.pending.t > 250) lat.pending = null; // main thread throttled (hidden)
  if (!lat.pending && drawn - lat.lastMark >= (baking() ? 0 : DISPLAY_SAMPLE_MS)) {
    lat.pending = rec;
    lat.lastMark = drawn;
    rec.mark = ++lat.markId;
    post('drawn', { id: rec.mark, t: performance.timeOrigin + drawn });
  }
  return rec;
}

// Main thread: absolute time of its first requestAnimationFrame after the draw.
function onDisplayed(id, abs) {
  const rec = lat.pending;
  if (!rec || rec.mark !== id) return;
  lat.pending = null;
  const shown = abs - performance.timeOrigin;
  if (shown < rec.t) return;
  rec.raw.displayed = shown;
  rec.s[ST.display] = shown - rec.t;
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
  DETAIL_STAGES.forEach((name, i) => {
    const r = pct(recs.filter((x) => x.d[i] !== null).map((x) => x.d[i]));
    if (r) rows[name] = r;
  });
  let sum = 0;
  let e2eSum = 0;
  for (const r of recs) {
    for (let i = first; i < ST.display; i++) sum += r.s[i];
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

// The frame pacing mode the records were drawn in: latency, smooth, or mixed
// (the setting changed meanwhile); null without records.
function pacingOf(recs) {
  const modes = new Set(recs.map((r) => r.pacing));
  return modes.size > 1 ? 'mixed' : modes.size ? [...modes][0] : null;
}

// Every 10 s the host logs the summary next to its encoder (results per vendor).
function reportStages(sum) {
  const t = now();
  if (!sum || t - lat.lastReport < 10000 || !transport) return;
  lat.lastReport = t;
  // Hosts before step 4.4 take no hold row (at most 9 rows before step 3.1b,
  // 12 since): hold and draw as one draw row there.
  const hold = hostFeatures.includes(P.FEATURE_STAGE_HOLD);
  const stages = hold ? sum.stages : { ...sum.stages, hold: null, draw: pct(lat.recs.map((r) => r.s[ST.hold] + r.s[ST.draw])) };
  const rows = [];
  for (const name of [...STAGES, ...DETAIL_STAGES]) if (stages[name]) rows.push({ name, ...stages[name] });
  rows.push({ name: 'e2e', from: sum.from, ...sum.e2e });
  // The presentation path that drew the window's frames (draw and display
  // depend on it); "bakeoff" for a window with several (the bake-off). The
  // frame pacing mode (hold and display depend on it). Whether FSR upscaled
  // the frames (fsr, off, or mixed; the draw and display rows depend on it).
  const paths = new Set(lat.recs.map((r) => r.path));
  const ups = new Set(lat.recs.map((r) => (r.up ? 'fsr' : 'off')));
  transport.sendControl({
    t: 'stages', stages: rows, renderer: paths.size === 1 ? [...paths][0] : 'bakeoff', pacing: pacingOf(lat.recs),
    upscale: ups.size > 1 ? 'mixed' : [...ups][0],
  });
}

// ---------------------------------------------------------------------------
// Latency probe: the frame barcode (protocol.js) read back from 1 in 30
// decoded frames, an independent check of the stage stamps.
//
//   seq        the host's test pattern carries each frame's seq (welcome
//              feature barcode-seq). A valid barcode that differs from the
//              header's seq means the picture drawn is not the frame the
//              header describes (stale, duplicated or skipped). Latency:
//              the frame's capture stamp -> drawn.
//   wallclock  the user enabled the latency probe and the host shows
//              tools/latency-test/index.html: the barcode is the host's
//              wall-clock ms (low 16 bits) when the page drew it, converted
//              with the host's wall-clock offset (welcome, "clock") and the
//              clock sync. Latency: page drew it -> drawn here, which adds the
//              host's render, present and capture delay to capture -> drawn.
//
// Readback never stalls the decoder: the 2D renderer hands over a clone of
// the frame, read after the draw (VideoFrame.copyTo of the corner, else
// drawImage into a small canvas); WebGL2 and WebGPU render the cells from the
// texture the frame was drawn from into 8x3 texels and read those back
// asynchronously (renderers.js).

const PROBE_EVERY = 30;
const PROBE_RANGE = [-100, 5000]; // ms; outside: implausible (clock or barcode wrong)
const PROBE_MAX_SAMPLES = 36000; // per-sample log for the export (5 h at 2/s)
const probe = {
  mode: 'off', features: [], wallOffsetUs: null, epoch: 0, count: 0, inflight: 0, method: '',
  startedAt: 0, sampled: 0, valid: 0, invalid: 0, mismatched: 0, implausible: 0, skipped: 0, noStamp: 0,
  hist: new Map(), pageToCapture: new Map(), samples: [], lastError: '', lastInvalid: null,
};

function updateProbeMode() {
  const mode = probe.features.includes(P.FEATURE_BARCODE_SEQ) ? 'seq'
    : prefs.latencyProbe && probe.wallOffsetUs !== null ? 'wallclock' : 'off';
  if (mode === probe.mode) return;
  Object.assign(probe, {
    mode, epoch: probe.epoch + 1, count: 0, method: '', startedAt: now(), sampled: 0, valid: 0, invalid: 0,
    mismatched: 0, implausible: 0, skipped: 0, noStamp: 0, hist: new Map(), pageToCapture: new Map(), samples: [], lastInvalid: null,
  });
  post('log', { text: `latency probe: ${mode}${mode === 'off' && prefs.latencyProbe ? ' (host sends no wall-clock offset)' : ''}` });
}

// Decide whether this frame is sampled; returns the request the renderer
// fills. vis: the part of the frame shown (the barcode is in its corner).
function probeStart(frame, meta, vis) {
  if (probe.mode === 'off' || !meta || clock.offset === null) return null;
  if (++probe.count % PROBE_EVERY !== 0) return null;
  if (probe.inflight >= 2) { probe.skipped++; return null; }
  const cell = probe.mode === 'seq' ? P.BARCODE_CELL : vis.w / P.BARCODE_WALLCLOCK_CELLS;
  if (P.BARCODE_COLS * cell > vis.w || P.BARCODE_ROWS * cell > vis.h) { probe.skipped++; return null; }
  return { mode: probe.mode, epoch: probe.epoch, cell, meta, offset: clock.offset, wallOffsetUs: probe.wallOffsetUs, clone: null, luma: null, via: renderer.name };
}

function probeSample(req, drawn) {
  const luma = req.clone ? readCornerLuma(req.clone, req.cell) : req.luma;
  if (!luma) { probe.skipped++; return; }
  if (!req.clone) probe.method = req.method || `${req.via} readback`;
  probe.inflight++;
  luma.then((l) => probeResult(req, drawn, l), (e) => {
    if (e?.message !== probe.lastError) post('log', { text: `latency probe readback failed: ${e?.message}` });
    probe.lastError = e?.message;
    probeResult(req, drawn, null);
  }).finally(() => { probe.inflight--; });
}

// Plane 0 of a VideoFrame format: luma for YUV (bytes per sample, shift to 8
// bits), or RGB channel offsets.
function plane0(fmt) {
  if (fmt === 'RGBA' || fmt === 'RGBX') return { px: 4, r: 0, g: 1, b: 2 };
  if (fmt === 'BGRA' || fmt === 'BGRX') return { px: 4, r: 2, g: 1, b: 0 };
  if (fmt === 'NV12' || /^I4(20|22|44)A?$/.test(fmt || '')) return { px: 1, shift: 0 };
  const m = /^I4(20|22|44)A?P(10|12)$/.exec(fmt || '');
  return m ? { px: 2, shift: +m[2] - 8 } : null;
}

function lumaFromPixels(buf, offset, stride, f, cell) {
  const luma = [];
  for (let k = 0; k < P.BARCODE_BITS; k++) {
    const [x0, y0, x1, y1] = P.barcodeSampleRect(k, cell);
    let sum = 0;
    for (let y = y0; y < y1; y++) {
      let o = offset + y * stride + x0 * f.px;
      for (let x = x0; x < x1; x++, o += f.px) {
        if (f.px === 4) sum += 0.299 * buf[o + f.r] + 0.587 * buf[o + f.g] + 0.114 * buf[o + f.b];
        else if (f.px === 2) sum += (buf[o] | (buf[o + 1] << 8)) >> f.shift;
        else sum += buf[o];
      }
    }
    luma.push(sum / ((x1 - x0) * (y1 - y0)));
  }
  return luma;
}

let probeCanvas = null;
let probeCtx = null;

// Mean luma of the barcode cells of a frame clone (always closed here).
async function readCornerLuma(frame, cell) {
  try {
    const w = Math.min(frame.displayWidth, Math.ceil(P.BARCODE_COLS * cell));
    const h = Math.min(frame.displayHeight, Math.ceil(P.BARCODE_ROWS * cell));
    const f = plane0(frame.format);
    const vr = frame.visibleRect;
    if (f && vr) {
      // Only the corner; 4:2:0 / 4:2:2 planes need an even rectangle.
      const rect = { x: vr.x & ~1, y: vr.y & ~1, width: Math.min(vr.width, (w + 1) & ~1), height: Math.min(vr.height, (h + 1) & ~1) };
      try {
        const buf = new Uint8Array(frame.allocationSize({ rect }));
        const layout = await frame.copyTo(buf, { rect });
        probe.method = `copyTo ${frame.format}`;
        return lumaFromPixels(buf, layout[0].offset, layout[0].stride, f, cell);
      } catch {
        // e.g. a GPU-backed frame this browser cannot copy: draw it instead
      }
    }
    if (!probeCanvas || probeCanvas.width !== w || probeCanvas.height !== h) {
      probeCanvas = new OffscreenCanvas(w, h);
      probeCtx = probeCanvas.getContext('2d', { willReadFrequently: true });
    }
    probeCtx.drawImage(frame, 0, 0, w, h, 0, 0, w, h);
    probe.method = `canvas ${frame.format || 'opaque'}`;
    return lumaFromPixels(probeCtx.getImageData(0, 0, w, h).data, 0, w * 4, { px: 4, r: 0, g: 1, b: 2 }, cell);
  } finally {
    frame.close();
  }
}

const histAdd = (h, ms) => { const b = Math.floor(ms); h.set(b, (h.get(b) || 0) + 1); };

function probeResult(req, drawn, luma) {
  if (req.epoch !== probe.epoch) return; // mode changed meanwhile
  probe.sampled++;
  const v = luma ? P.barcodeDecodeLuma(luma) : null;
  if (v === null) {
    probe.invalid++;
    // For diagnosis: which frame, and the cells as read (null: readback failed).
    probe.lastInvalid = { gen: req.meta.gen, seq: req.meta.seq, luma: luma && luma.map((l) => Math.round(l)) };
    return;
  }
  probe.valid++;
  const m = req.meta;
  const cap = m.ext?.captureUs !== undefined ? m.ext.captureUs / 1000 - req.offset : null; // local ms
  let ms;
  let p2c = null;
  if (req.mode === 'seq') {
    if (v !== m.seq % 65536) { probe.mismatched++; return; }
    if (cap === null) { probe.noStamp++; return; }
    ms = drawn - cap;
  } else {
    // The host's wall clock at our draw; the page's ms is the latest one
    // before it with these low 16 bits (up to 1 s "ahead" allows clock error).
    const wallNow = drawn + req.offset + req.wallOffsetUs / 1000;
    const d = wallNow - v;
    ms = d - 65536 * Math.floor((d + 1000) / 65536) - 0.5; // Date.now() floors: mid-millisecond
    if (cap !== null) p2c = cap - (drawn - ms);
  }
  if (!(ms >= PROBE_RANGE[0] && ms <= PROBE_RANGE[1])) { probe.implausible++; return; }
  histAdd(probe.hist, ms);
  if (p2c !== null) histAdd(probe.pageToCapture, p2c);
  if (probe.samples.length < PROBE_MAX_SAMPLES) {
    probe.samples.push([+((drawn - probe.startedAt) / 1000).toFixed(3), m.gen, m.seq, v, +ms.toFixed(2), p2c === null ? null : +p2c.toFixed(2)]);
  }
}

// Percentiles (1 ms resolution) from a histogram.
function histSummary(h) {
  const keys = [...h.keys()].sort((a, b) => a - b);
  let n = 0;
  let sum = 0;
  for (const k of keys) { n += h.get(k); sum += h.get(k) * (k + 0.5); }
  if (!n) return null;
  const q = (p) => {
    const want = Math.min(n - 1, Math.floor(p * n));
    let c = 0;
    for (const k of keys) { c += h.get(k); if (c > want) return k; }
    return keys[keys.length - 1];
  };
  return { n, p50: q(0.5), p95: q(0.95), p99: q(0.99), min: keys[0], max: keys[keys.length - 1], mean: +(sum / n).toFixed(2) };
}

function probeSummary(full) {
  if (probe.mode === 'off') return { mode: 'off' };
  const out = {
    mode: probe.mode, from: probe.mode === 'seq' ? 'capture' : 'page', every: PROBE_EVERY, method: probe.method,
    durationS: +((now() - probe.startedAt) / 1000).toFixed(1),
    sampled: probe.sampled, valid: probe.valid, invalid: probe.invalid, mismatched: probe.mismatched,
    implausible: probe.implausible, skipped: probe.skipped, noStamp: probe.noStamp,
    latency: histSummary(probe.hist), pageToCapture: histSummary(probe.pageToCapture), lastInvalid: probe.lastInvalid,
    histogram: Object.fromEntries([...probe.hist.entries()].sort((a, b) => a[0] - b[0])),
  };
  if (full) {
    out.pageToCaptureHistogram = Object.fromEntries([...probe.pageToCapture.entries()].sort((a, b) => a[0] - b[0]));
    out.sampleFields = ['t_s', 'gen', 'seq', 'barcode', 'latency_ms', 'page_to_capture_ms'];
    out.samples = probe.samples;
  }
  return out;
}

// Delay-based congestion detection: if one-way delay rises well above its
// recent minimum for a sustained period, the path is queueing. Ask the host to
// back off before latency balloons. Hosts with the rate controller (rate
// reports) judge the delay themselves.
function checkCongestion(owd) {
  if (fb.on || clock.offset === null || !isFinite(owd)) return;
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
  const same = audio.cfg?.enabled && cfg.enabled && audio.cfg.codec === cfg.codec && audio.cfg.sampleRate === cfg.sampleRate &&
    audio.cfg.channels === cfg.channels;
  audio.cfg = cfg;
  post('audio', { cfg });
  // A new frame duration of the running stream (the host follows the RTT,
  // step 4.6; sameStream) keeps the decoder and the sequence: Opus packets
  // carry their duration.
  if (same && cfg.sameStream && (cfg.codec !== 'opus' || audio.decoder?.state === 'configured')) return;
  // Anything else is a new audio stream from the host, also one with the same
  // codec (a codec setting a client without an Opus decoder gets PCM for, a
  // host before step 4.6): its sequence numbers start again from 0.
  audio.lastSeq = -1;
  if (audio.decoder) { try { audio.decoder.close(); } catch {} audio.decoder = null; }
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
  if ((d[1] === P.AUDIO_OPUS) !== (audio.cfg.codec === 'opus')) return; // a packet of the stream before a codec change
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const seq = v.getUint16(2, true);
  const pts = v.getUint32(4, true);
  const payload = d.subarray(8);
  // The packet's own duration (Opus TOC, PCM size): the host changes the
  // Opus frame duration with the RTT (step 4.6).
  const samples = P.audioPacketSamples(d) || 48 * audio.cfg.frameMs;
  stats.audioPackets++;
  fb.audio++;
  if (audio.lastSeq >= 0) {
    const gap = (seq - audio.lastSeq - 1) & 0xffff;
    if (gap > 0 && gap < 4) {
      // Silence for what was lost: up to this packet's pts, which also
      // covers lost packets of another duration.
      const missing = (pts - audio.nextPts) >>> 0;
      stats.audioLost += gap;
      fb.lost += gap;
      silence(missing > 0 && missing <= gap * 960 ? missing : gap * samples);
    } else if (gap >= 0x8000 && ((audio.lastSeq - seq) & 0xffff) < AUDIO_MAX_LATE) return; // late/duplicate
    // The pts moved on by more than the lost packets held: the host's audio
    // source paused (WASAPI loopback sends nothing while nothing plays; the
    // host moves the pts on by a pause of 50 ms or more). The jitter buffer
    // ran dry because the sound ended, not because the network held packets
    // up: the AudioWorklet takes that underrun back (through the page).
    if (gap < 4 && ((pts - audio.nextPts) | 0) > (gap + 1) * 960) post('audioPause');
  }
  audio.lastSeq = seq;
  audio.nextPts = (pts + samples) >>> 0;
  audio.samples = samples;
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
    case 'welcome':
      pageCtl.input = true;
      hostFeatures = m.features || [];
      probe.features = hostFeatures;
      fb.on = hostFeatures.includes(P.FEATURE_RATE_REPORT);
      if (fb.on && !fb.timer) fb.timer = setInterval(sendRateReport, RATE_REPORT_MS);
      probe.wallOffsetUs = m.wallOffsetUs ?? null;
      hdr.hostOffers = hostFeatures.includes(P.FEATURE_HDR);
      updateProbeMode();
      post('welcome', { info: m });
      break;
    case 'clock': if (Number.isFinite(m.wallOffsetUs)) probe.wallOffsetUs = m.wallOffsetUs; updateProbeMode(); break;
    case 'video': onVideoConfig(m); break;
    // The running generation's bitrate or frame rate changed in the encoder
    // (native helper). The config is updated in place: configureDecoder
    // compares it by identity.
    case 'rate':
      if (video.cfg?.gen === m.gen && m.fps > 0) video.cfg.fps = m.fps;
      post('rate', { gen: m.gen, bitrate: m.bitrate, fps: m.fps, maxBitrate: m.maxBitrate });
      break;
    case P.MSG_DROPPED: onDropped(m); break;
    case 'audio': onAudioConfig(m); break;
    case 'cursor': post('cursor', { shape: m }); break;
    case 'notice': post('notice', { level: m.level, msg: m.msg }); break;
    case 'bye':
      byeReason = m.msg || 'Session ended by the host';
      post('notice', { level: 'warn', msg: byeReason });
      // Ending the session tells the host the bye arrived: it waits for that
      // before it closes the connection, which would reset the stream with a
      // bye still in flight.
      transport?.close();
      break;
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
    case P.DG_VIDEO_SHARD: onFecShard(d); break;
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
  const fecFrames = fecRx.stats.frames - fecMode.frames;
  fecMode.frames = fecRx.stats.frames;
  if (fecFrames > 0 || stats.frames > 0) fecMode.on = fecFrames > 0;
  post('stats', {
    stages,
    probe: probeSummary(false),
    fps: stats.frames / dt,
    mbps: (stats.bytes * 8) / dt / 1e6,
    rtt: clock.rtt,
    owd: avg(stats.owdSum, stats.owdN),
    decode: avg(stats.decodeSum, stats.decodeN),
    // In the span the stage summary names (stages.from): capture->draw only
    // if every frame of its 10 s window, which covers this period, had it.
    total: avg(stages?.from === 'send' ? stats.sendSum : stats.totalSum, stats.totalN),
    totalMin: isFinite(stats.totalMin) ? stats.totalMin : null,
    totalMax: stats.totalMax || null,
    dropped: stats.dropped,
    skipped: stats.skipped,
    hostDropped: stats.hostDropped,
    streamResets: stats.streamResets, // reset frame streams whose header arrived (onFrameReset)
    thinned: stats.thinned, // frames the host left out on purpose (temporal SVC thinning), not losses
    keyRequests: stats.keyRequests,
    recovered: stats.recovered,
    recoveredByKey: stats.recoveredByKey,
    recoveryDiscarded: stats.recoveryDiscarded,
    recoveryRejected: stats.recoveryRejected,
    keyFrames: stats.keyFrames, // key frames fed to the decoder (IDRs)
    freezes: stats.freezes,
    stalls: stats.stalls, // stand-stills over STALL_MS beyond the source's
    lastFreeze: stats.lastFreeze || null,
    // Datagram + FEC (fec.js, GUIDE 2.5): frames rebuilt from shards and the
    // shards' counters, this session (null: the host never sent shards).
    fec: fecRx.stats.shards || fecRx.stats.repairs ? fecRx.summary() : null,
    fecNow: fecMode.on, // the video comes as shards now (fecMode)
    audioPackets: stats.audioPackets,
    audioLost: stats.audioLost,
    audioMs,
    rateReports: fb.on ? fb.sent : null, // rate reports sent so far (null: the host does not want them)
    audioFrameMs: audio.samples ? audio.samples / 48 : null, // the last packet's duration
    minRtt: clock.minRtt || null,
    // Decoder hygiene (4.1): decodeQueueSize now and its maximum (bound
    // MAX_DECODE_QUEUE), chunks waiting in front of the decoder, decoded
    // frames closed unseen for a newer one (superseded) and chunks dropped
    // undecoded in front of the decoder for a key frame (supersededChunks),
    // the smallest output lag in this period (frames), the VideoFrames open
    // now / at most / leaked.
    queue: video.decoder ? video.decoder.decodeQueueSize : 0,
    queueMax: video.queueMax,
    waiting: video.queue.length,
    waitingMax: video.waitingMax,
    superseded: stats.superseded,
    supersededChunks: stats.supersededChunks,
    outputLag: isFinite(stats.lagMin) ? stats.lagMin : null,
    videoFrames: { open: openFrames(), max: frames.max, leaked: frames.leaked },
    // Presentation (4.3): the active path, what its context reports, the
    // canvas size (device pixels) and the bake-off's progress or result.
    renderer: renderer ? rendererInfo() : null,
    // HDR10 (4.5): what this client offers the host and why not.
    hdr: { ...hdrPrefs(), decoderInfo: hdr.decoders, withdrawn: hdr.withdrawn, hostOffers: hdr.hostOffers, white: hdr.white, space: hdr.space },
    // Frame pacing (4.4): the mode, where Smooth's refresh ticks come from,
    // the refresh interval it works with (its ticks' or the page-load
    // measurement), the draws per source and Smooth's stale (dropped) and
    // late frames, this session.
    pacing: pacer.info(),
    hw: video.hw,
    synced: clock.offset !== null,
    prio: transport?.prio ? { ...transport.prio } : null, // send priorities (WebTransport only)
  });
  Object.assign(stats, { frames: 0, bytes: 0, decodeSum: 0, decodeN: 0, owdSum: 0, owdN: 0, totalSum: 0, sendSum: 0, totalN: 0, totalMin: Infinity, totalMax: 0, lagMin: Infinity });
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

// Startup decoder self-test (decoder-selftest.js): results for the overlay
// (main thread: decoderTest) and the log; families whose hardware decoder
// holds frames back go to the host as without a hardware decoder (it prefers
// a family the browser decodes in hardware) and decode in software when that
// passed. Each family's decode time on the 1080p timing clip goes to the host
// too (timing), which picks the codec family by it (step 4.2). The hello
// waits for all of it: its duration goes to the overlay and the log. Returns
// the hello's decoders.
async function selfTestDecoders(decoders) {
  let tests = [];
  const t0 = performance.now();
  try {
    tests = await runSelfTests(decoders, prefs.decoder !== 'software');
  } catch (e) {
    post('log', { text: `decoder self-test failed: ${e.message}` });
  }
  const ms = Math.round(performance.now() - t0);
  video.selfTest = tests;
  for (const t of tests) {
    if (t.software) video.softwareFor.add(t.family);
    post('log', { text: `decoder self-test: ${t.text}` });
  }
  post('log', { text: `decoder self-test took ${ms} ms` });
  post('decoderTest', { tests, ms });
  return decoders.map((d) => helloDecoder(d, tests.find((t) => t.family === d.family)));
}

// Control messages from the page (pause and resume when the tab is hidden,
// live settings, key frame requests) wait for the hello: the host takes the
// first control message as the hello and ends the session on anything else
// ("bad hello"), and the page may send them as soon as the transport is up,
// while the hello still waits for the decoder self-test. Held: the last of
// each kind (pause and resume are one kind), sent in that order after it.
// Input waits for the welcome, which comes once the session is the host's
// active one: the host ends a session's input stream on input that arrives
// before (a window blur's key releases while the self-test runs), which left
// the whole session without input. Before the first frame the page sends no
// other input, so it is dropped, not held.
const pageCtl = { open: false, held: [], input: false };
const ctlKind = (c) => (c?.t === 'resume' ? 'pause' : c?.t);

function pageControl(c) {
  if (!pageCtl.open) {
    pageCtl.held = pageCtl.held.filter((h) => ctlKind(h) !== ctlKind(c));
    pageCtl.held.push(c);
    return;
  }
  if (c?.t === 'pause' || c?.t === 'resume') freeze.drawn = 0; // not a freeze
  if (c?.t === 'settings' && c.prefs) c = { ...c, prefs: { ...c.prefs, hdr: hdrPrefs() } }; // what this client can present now
  transport?.sendControl(c);
}

// The hello is out: the page's held control messages follow it.
function openPageControl() {
  pageCtl.open = true;
  for (const c of pageCtl.held.splice(0)) pageControl(c);
}

async function start(msg) {
  prefs = msg.prefs || {};
  pacer.setMode(prefs.pacing);
  if (msg.audioSab) audio.ring = new RingWriter(msg.audioSab);
  if (msg.audioPort) audio.port = msg.audioPort;
  await setupRenderers(msg);
  hdr.mode = prefs.hdr === 'off' ? 'off' : 'auto';
  hdr.display = !!prefs.hdrDisplay;
  hdr.white = hdrWhite(prefs.hdrWhite);
  hdr.space = prefs.gamutP3 ? 'display-p3' : 'srgb';
  hdr.withdrawn = { ...prefs.hdrWithdrawn }; // this page's earlier connections found these families' frames not drawable as HDR
  hdr.canvas = renderer.name !== 'webgpu' ? { ok: false, why: `HDR needs the WebGPU renderer (this connection draws with ${LABELS[renderer.name] || renderer.name})` }
    : pres.mode === 'bakeoff' ? { ok: false, why: 'HDR needs Renderer WebGPU (Auto is measuring the renderers)' } : renderer.hdrCanvasOk;
  const hdrDecs = renderer.name === 'webgpu' ? hdrDecoders() : Promise.resolve([]);
  const decoders = await probeDecoders();
  post('decoders', { decoders });
  const tested = selfTestDecoders(decoders); // while connecting
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
  const helloDecoders = await tested;
  hdr.decoders = (await hdrDecs).filter((d) => !hdr.withdrawn[d.family]);
  applyHdr(false);
  const withdrawn = Object.keys(hdr.withdrawn);
  post('log', { text: `HDR: ${hdrWhy() || 'offered to the host'} (canvas ${hdr.canvas.ok ? 'extended range' : 'SDR'}, 10-bit decoders ${hdr.decoders.map((d) => `${d.family}${d.hw ? ' hw' : ''}`).join(', ') || 'none'}` +
    `${withdrawn.length ? `; withdrawn: ${withdrawn.join(', ')}` : ''})` });
  transport.sendControl({
    t: 'hello', v: P.HELLO_VERSION, ticket: conn.ticket,
    client: msg.client, decoders: helloDecoders, audio: { opus: opusOK, pcm: true }, prefs: { ...msg.hostPrefs, hdr: hdrPrefs() },
    // Video frames as datagram shards (GUIDE 2.5): WebTransport only.
    ...(transport.kind === 'webtransport' && prefs.fec !== 'off' ? { fec: P.HELLO_FEC_VERSION } : {}),
  });
  post('hello', { decoders: helloDecoders }); // what the host chose the codec from (overlay, tests)
  openPageControl();
  for (let i = 0; i < 5; i++) setTimeout(sendPing, i * 60);
  const pingTimer = setInterval(sendPing, 1000);
  const watchdogTimer = setInterval(videoWatchdog, 250);
  const statsTimer = setInterval(postStats, 500);
  const reason = await transport.run({ control: onControl, datagram: onDatagram, frame: onFrameBytes, frameReset: onFrameReset });
  clearInterval(pingTimer);
  clearInterval(statsTimer);
  clearInterval(watchdogTimer);
  clearInterval(fb.timer);
  fb.timer = 0;
  clearTimeout(fecTimer.id);
  fecTimer.at = Infinity;
  transport = null;
  // A deliberate "bye" (e.g. another device took over) must not trigger an
  // automatic reconnect, or two clients would keep stealing the session.
  post('closed', { reason: byeReason || reason, retry: !byeReason });
}

self.onmessage = (ev) => {
  const m = ev.data;
  switch (m.type) {
    case 'start': start(m).catch((e) => post('closed', { reason: e.message, retry: true })); break;
    case 'in': if (pageCtl.input) transport?.sendInput(m.b); break;
    case 'dg': if (pageCtl.input) transport?.sendInputDatagram(m.b); break;
    case 'ctl': pageControl(m.m); break;
    case 'prefs':
      prefs = { ...prefs, ...m.prefs };
      updateProbeMode();
      pacer.setMode(prefs.pacing); // live
      if (['upscale', 'sharpness', 'fsrDenoise', 'fsrInput'].some((k) => k in m.prefs)) applyUpscale(); // live
      if (['hdr', 'hdrWhite', 'hdrDisplay'].some((k) => k in m.prefs)) {
        // Live: an HDR stream is tone-mapped at once when HDR is no longer
        // wanted (the main thread's settings message moves the host to SDR).
        hdr.mode = prefs.hdr === 'off' ? 'off' : 'auto';
        hdr.display = !!prefs.hdrDisplay;
        hdr.white = hdrWhite(prefs.hdrWhite);
        applyHdr();
      }
      break;
    case 'hdrCheck': hdr.check = { points: m.points }; break; // test hook: the next HDR frame's canvas pixels and codes
    case 'hdrOpaque': hdr.opaqueTest = true; break; // test hook: HDR frames count as format null (Chrome's hardware 10-bit frames)
    case 'tick': pacer.tick(m.t - performance.timeOrigin, 'main'); break; // the main thread's animation frame (absolute ms)
    case 'probeDump': post('probeDump', { probe: probeSummary(true), stages: stageSummary() }); break;
    case 'displayed': onDisplayed(m.id, m.t); break;
    case 'resize': onResize(m.w, m.h); break;
    case 'dropTest': if (!dropTest.run) dropTest.armed = true; break;
    case 'loseContext': renderer?.loseContext(); break; // test hook: the active path's GPU context is lost
    case 'stageDump': post('stageDump', { recs: lat.recs.map((r) => ({ ...r.raw, stages: r.s, e2e: r.e2e, fromCapture: r.fromCapture })) }); break;
    case 'close':
      if (transport) {
        if (pageCtl.open) transport.sendControl({ t: 'bye' }); // (not in place of the hello)
        setTimeout(() => transport?.close(), 50);
      }
      break;
  }
};
