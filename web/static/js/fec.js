// "Datagram + FEC" video mode, client side (GUIDE 2.5; the host's half is
// internal/host/fec.go, the format internal/proto/fec.go).
//
// Where a path's round trip makes a lost packet's retransmission a visible
// stall, the host sends a video frame not on its own stream but as datagrams:
// the frame's bytes (header, extension, payload: what a frame stream carries)
// cut into n data shards of one size (the last one shorter), grouped into
// blocks of at most MAX_BLOCK_DATA data shards, each block a systematic
// Reed-Solomon code over GF(2^8) with parity shards. Any k of a block's
// shards rebuild its k data shards. FecReceiver collects the shards, rebuilds
// what is missing and hands the frame on exactly as a frame stream would
// (onFrameBytes); shards a frame still lacks once its shards stop coming it
// asks for (DG_FEC_NACK; the host answers with fresh parity), while the frame
// has time; then it gives the frame up (lost: the loss-recovery path).
//
// The code is github.com/klauspost/reedsolomon's default: the Vandermonde
// matrix V[r][c] = r^c (0^0 = 1) made systematic by the inverse of its top
// k x k square; parity row r of a block is row k + r of V x inv(top), the same
// whatever the number of parity rows, which is what lets a repair send rows
// the frame did not. GF(2^8) with the polynomial 0x11d, generator 2.

import { DG_VIDEO_SHARD, DG_FEC_NACK } from './protocol.js';

export const SHARD_HEADER_LEN = 18;
export const MAX_BLOCK_DATA = 64;
export const MAX_BLOCK_SHARDS = 256;
export const SHARD_PARITY = 1;
export const SHARD_REPAIR = 2;
const MAX_NACK_BLOCKS = 64;

// ---------------------------------------------------------------------------
// GF(2^8)

const EXP = new Uint8Array(512);
const LOG = new Uint8Array(256);
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    EXP[i] = x;
    LOG[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
  for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255];
}
export const gfMul = (a, b) => (a && b ? EXP[LOG[a] + LOG[b]] : 0);
const gfInv = (a) => EXP[255 - LOG[a]];
const gfExp = (a, n) => (n === 0 ? 1 : a === 0 ? 0 : EXP[(LOG[a] * n) % 255]);

let MUL = null; // MUL[c * 256 + x] = c * x, built on first use (64 KB)
function mulTable() {
  if (!MUL) {
    MUL = new Uint8Array(65536);
    for (let c = 1; c < 256; c++) for (let x = 1; x < 256; x++) MUL[c * 256 + x] = EXP[LOG[c] + LOG[x]];
  }
  return MUL;
}

/** dst ^= c * src (byte-wise, GF(2^8)), over dst's length. */
function mulAdd(dst, src, c) {
  if (!c) return;
  const n = dst.length;
  if (c === 1) {
    for (let i = 0; i < n; i++) dst[i] ^= src[i];
    return;
  }
  const t = mulTable();
  const o = c * 256;
  for (let i = 0; i < n; i++) dst[i] ^= t[o + src[i]];
}

/** Inverse of an n x n matrix (array of Uint8Array rows), or null if singular. */
function invert(m, n) {
  const a = m.map((r, i) => {
    const row = new Uint8Array(2 * n);
    row.set(r.subarray(0, n));
    row[n + i] = 1;
    return row;
  });
  for (let c = 0; c < n; c++) {
    let p = c;
    while (p < n && !a[p][c]) p++;
    if (p === n) return null;
    if (p !== c) [a[c], a[p]] = [a[p], a[c]];
    const inv = gfInv(a[c][c]);
    if (inv !== 1) for (let j = 0; j < 2 * n; j++) a[c][j] = gfMul(a[c][j], inv);
    for (let r = 0; r < n; r++) {
      const f = a[r][c];
      if (r === c || !f) continue;
      for (let j = 0; j < 2 * n; j++) a[r][j] ^= gfMul(f, a[c][j]);
    }
  }
  return a.map((r) => r.slice(n));
}

const topInv = new Map(); // k -> inv(top k x k of V)
const rows = new Map(); // k * 256 + r -> parity row r of a block of k (Uint8Array k)

/** Parity row r (0-based) of a block of k data shards: row k + r of V x inv(top). */
export function parityRow(k, r) {
  const key = k * 256 + r;
  let row = rows.get(key);
  if (row) return row;
  let ti = topInv.get(k);
  if (!ti) {
    const top = [];
    for (let i = 0; i < k; i++) top.push(Uint8Array.from({ length: k }, (_, c) => gfExp(i, c)));
    ti = invert(top, k);
    topInv.set(k, ti);
  }
  const v = Uint8Array.from({ length: k }, (_, c) => gfExp(k + r, c));
  row = new Uint8Array(k);
  for (let c = 0; c < k; c++) {
    let s = 0;
    for (let j = 0; j < k; j++) s ^= gfMul(v[j], ti[j][c]);
    row[c] = s;
  }
  if (rows.size > 8192) rows.clear();
  rows.set(key, row);
  return row;
}

/** Parity rows from..from+count-1 of k equal-size data shards (the host's encoder; tests). */
export function encodeParity(k, data, from, count) {
  const size = data[0].length;
  const out = [];
  for (let r = from; r < from + count; r++) {
    const p = new Uint8Array(size);
    const row = parityRow(k, r);
    for (let c = 0; c < k; c++) mulAdd(p, data[c], row[c]);
    out.push(p);
  }
  return out;
}

/**
 * Rebuild the missing data shards of a block of k: have maps shard index
 * (0..k-1 data, k.. parity row index - k) to its bytes (size each, data
 * shards zero-padded). Returns the k data shards (the present ones as given)
 * or null with fewer than k shards. Only the missing ones are computed: an
 * e x e system over e parity rows, e the number missing.
 */
export function reconstruct(k, size, have) {
  const data = new Array(k);
  const missing = [];
  for (let i = 0; i < k; i++) {
    const d = have.get(i);
    if (d) data[i] = d;
    else missing.push(i);
  }
  const e = missing.length;
  if (!e) return data;
  const par = [...have.keys()].filter((i) => i >= k).sort((a, b) => a - b).slice(0, e);
  if (par.length < e) return null;
  const prow = par.map((i) => parityRow(k, i - k));
  const A = invert(prow.map((row) => Uint8Array.from(missing, (c) => row[c])), e);
  if (!A) return null;
  const rhs = par.map((pi, i) => {
    const b = new Uint8Array(size);
    b.set(have.get(pi));
    for (let c = 0; c < k; c++) if (data[c]) mulAdd(b, data[c], prow[i][c]);
    return b;
  });
  missing.forEach((c, j) => {
    const d = new Uint8Array(size);
    for (let i = 0; i < e; i++) mulAdd(d, rhs[i], A[j][i]);
    data[c] = d;
  });
  return data;
}

// ---------------------------------------------------------------------------
// Shards

/** The blocks of a frame of n data shards: [{ base, k }] (as equal as they divide). */
export function blockLayout(n) {
  const nb = Math.ceil(n / MAX_BLOCK_DATA);
  const out = [];
  for (let i = 0; i < nb; i++) {
    const lo = Math.floor((i * n) / nb);
    out.push({ base: lo, k: Math.floor(((i + 1) * n) / nb) - lo });
  }
  return out;
}

/** Parse a DG_VIDEO_SHARD datagram (data aliases d), or null if malformed (mirror of proto.ParseVideoShard). */
export function parseShard(d) {
  if (d.length < SHARD_HEADER_LEN || d[0] !== DG_VIDEO_SHARD) return null;
  const v = new DataView(d.buffer, d.byteOffset, d.byteLength);
  const s = {
    flags: d[1], gen: d[2], index: d[3], seq: v.getUint32(4, true), len: v.getUint32(8, true),
    size: v.getUint16(12, true), base: v.getUint16(14, true), k: d[16], m: d[17], data: d.subarray(SHARD_HEADER_LEN),
  };
  if (!s.size || !s.len || s.len > 32 * 1024 * 1024 || !s.k || s.k > MAX_BLOCK_DATA) return null;
  const n = Math.ceil(s.len / s.size);
  if (s.base + s.k > n) return null;
  const want = s.index < s.k ? Math.min(s.size, s.len - (s.base + s.index) * s.size) : s.size;
  return s.data.length === want ? s : null;
}

/** A DG_FEC_NACK datagram: frame (gen, seq) needs `need` more shards of each listed block ([] : the whole frame). */
export function fecNack(gen, seq, blocks) {
  const k = Math.min(blocks.length, MAX_NACK_BLOCKS);
  const b = new Uint8Array(8 + 4 * k);
  const v = new DataView(b.buffer);
  b[0] = DG_FEC_NACK;
  b[1] = gen & 0xff;
  b[2] = k;
  v.setUint32(4, seq >>> 0, true);
  for (let i = 0; i < k; i++) {
    v.setUint16(8 + 4 * i, blocks[i].base, true);
    b[10 + 4 * i] = Math.max(0, Math.min(255, blocks[i].need));
  }
  return b;
}

/**
 * The shards of a frame as the host sends them (internal/fec Cut): each
 * block's data shards, then `parity(k)` parity shards. For tests and tools;
 * the client only receives.
 */
export function cutFrame(frame, gen, seq, maxShard, parity) {
  const size = Math.ceil(frame.length / Math.ceil(frame.length / maxShard));
  const out = [];
  for (const { base, k } of blockLayout(Math.ceil(frame.length / size))) {
    const m = Math.min(parity(k), MAX_BLOCK_SHARDS - k);
    const data = [];
    for (let i = 0; i < k; i++) {
      const d = new Uint8Array(size);
      d.set(frame.subarray((base + i) * size, Math.min(frame.length, (base + i + 1) * size)));
      data.push(d);
    }
    const par = encodeParity(k, data, 0, m);
    for (let i = 0; i < k + m; i++) {
      const payload = i < k ? data[i].subarray(0, Math.min(size, frame.length - (base + i) * size)) : par[i - k];
      out.push(shardDatagram({ flags: i < k ? 0 : SHARD_PARITY, gen, index: i, seq, len: frame.length, size, base, k, m }, payload));
    }
  }
  return out;
}

/** A DG_VIDEO_SHARD datagram (tests and tools). */
export function shardDatagram(h, payload) {
  const b = new Uint8Array(SHARD_HEADER_LEN + payload.length);
  const v = new DataView(b.buffer);
  b[0] = DG_VIDEO_SHARD;
  b[1] = h.flags;
  b[2] = h.gen;
  b[3] = h.index;
  v.setUint32(4, h.seq >>> 0, true);
  v.setUint32(8, h.len >>> 0, true);
  v.setUint16(12, h.size, true);
  v.setUint16(14, h.base, true);
  b[16] = h.k;
  b[17] = h.m;
  b.set(payload, SHARD_HEADER_LEN);
  return b;
}

// ---------------------------------------------------------------------------
// Receiver

// Timing (ms). A frame's shards come back to back (paced: a frame takes up
// to about a frame interval, a large key frame on a slow path longer), in
// order: each block's data, then its parity. Once its first transmission
// is through (its last block's last parity shard or a shard of a newer frame
// arrived: sentAt) and GRACE has passed (reordering), or none of its shards
// came for QUIET, the frame has stalled: the shards it lacks are lost. It
// NACKs them at once, again every RETRY while the repairs do not come, and
// gives the frame up GIVE_UP after the stall (lost: the stream's loss
// handling takes over, a recovery frame or a key frame). GIVE_UP leaves room
// for two NACK rounds and stays below the reorder buffer's gap timeout
// (max(250, 4 x RTT)), which would otherwise decide.
const GRACE_MS = 3;
const quietMs = (interval) => Math.max(20, 2 * interval);
const retryMs = (rtt) => Math.max(25, 1.5 * rtt);
const giveUpMs = (rtt) => Math.max(120, 2.5 * rtt + 30);
const FORGET_MS = 2000; // a frame's bookkeeping (late shards, the loss count) lasts this long
const MAX_PLACEHOLDERS = 8; // frames with no shard at all, NACKed whole, per gap

const frameKey = (gen, seq) => gen * 4294967296 + seq;

/**
 * Collects video shards into frames. h: { deliver(buf, recv, first,
 * repaired): a complete frame (its bytes, when its last needed shard arrived,
 * when its first did, whether it needed a NACK), lost(gen, seq): a frame
 * given up, nack(datagram), rtt(): ms,
 * interval(): the frame interval, ms }. Every method takes the time t (ms);
 * the caller runs tick(t) at nextDue() while that is finite.
 */
export class FecReceiver {
  constructor(h) {
    this.h = h;
    this.frames = new Map(); // frameKey -> entry, oldest first
    this.open = new Set(); // entries neither complete nor given up
    this.top = new Map(); // gen -> newest seq seen
    this.stats = {
      frames: 0, shards: 0, parity: 0, repairs: 0, shardsLost: 0, rebuilt: 0, repaired: 0, nacks: 0, wholeNacks: 0,
      lost: 0, unused: 0, bad: 0, bytes: 0, frameBytes: 0,
    };
  }

  /** A DG_VIDEO_SHARD datagram d arrived at t. */
  shard(d, t) {
    const s = parseShard(d);
    const st = this.stats;
    if (!s) { st.bad++; return; }
    st.bytes += d.length;
    if (s.flags & SHARD_REPAIR) st.repairs++;
    else st.shards++;
    if (s.index >= s.k) st.parity++;
    this.newer(s.gen, s.seq, t);
    const key = frameKey(s.gen, s.seq);
    let e = this.frames.get(key);
    if (e?.ph) { // a placeholder: the frame's first shard
      this.frames.delete(key);
      this.open.delete(e);
      e = null;
    }
    e ||= this.entry(s, t);
    const b = e.blocks?.find((x) => x.base === s.base);
    if (!b || b.k !== s.k || s.len !== e.len || s.size !== e.size) { st.bad++; return; }
    if (b.seen.has(s.index)) return;
    b.seen.add(s.index);
    if (b.m < 0) b.m = s.m;
    e.last = t;
    // The frame's last shard of its first transmission (or a repair): every
    // shard it sent has had its chance to arrive.
    if (!e.sentAt && (s.index >= s.k + s.m - 1) && b === e.blocks[e.blocks.length - 1]) e.sentAt = t;
    if (e.done || e.lost) { st.unused++; return; } // parity after its frame completed (normal), or after it was given up
    if (b.done) return;
    if (s.index < b.k) {
      e.out.set(s.data, (b.base + s.index) * e.size);
      b.data[s.index] = 1;
    } else {
      const p = new Uint8Array(e.size);
      p.set(s.data);
      b.parity.set(s.index, p);
    }
    if (++b.have < b.k) return;
    this.rebuild(e, b);
    if (e.blocks.every((x) => x.done)) this.complete(e, t);
  }

  entry(s, t) {
    const n = Math.ceil(s.len / s.size);
    const e = {
      gen: s.gen, seq: s.seq, len: s.len, size: s.size, first: t, last: t, sentAt: 0, stallAt: 0, nackAt: 0, nacks: 0,
      done: false, lost: false, rebuilt: false, out: new Uint8Array(n * s.size),
      blocks: blockLayout(n).map(({ base, k }) => ({ base, k, m: -1, seen: new Set(), data: new Uint8Array(k), parity: new Map(), have: 0, done: false })),
    };
    this.frames.set(frameKey(s.gen, s.seq), e);
    this.open.add(e);
    return e;
  }

  // A shard of frame (gen, seq): the older open frames of the generation
  // have seen their shards come and go; frames skipped entirely get a
  // placeholder, NACKed whole (they may also have gone on a stream, or been
  // dropped by the host, which then ignores the NACK; a placeholder is never
  // given up as lost).
  newer(gen, seq, t) {
    const top = this.top.get(gen);
    if (top !== undefined && seq <= top) return;
    if (top !== undefined) {
      for (let q = Math.max(top + 1, seq - MAX_PLACEHOLDERS); q < seq; q++) {
        const key = frameKey(gen, q);
        if (this.frames.has(key)) continue;
        const ph = { ph: true, gen, seq: q, first: t, last: t, sentAt: t, stallAt: 0, nackAt: 0, nacks: 0 };
        this.frames.set(key, ph);
        this.open.add(ph);
      }
    }
    this.top.delete(gen);
    this.top.set(gen, seq);
    if (this.top.size > 4) this.top.delete(this.top.keys().next().value);
    for (const e of this.open) if (e.gen === gen && e.seq < seq && !e.sentAt) e.sentAt = t;
  }

  // Block b has k shards: fill in its missing data shards.
  rebuild(e, b) {
    b.done = true;
    const missing = [];
    for (let i = 0; i < b.k; i++) if (!b.data[i]) missing.push(i);
    if (missing.length) {
      const have = new Map();
      for (let i = 0; i < b.k; i++) if (b.data[i]) have.set(i, e.out.subarray((b.base + i) * e.size, (b.base + i + 1) * e.size));
      for (const [i, p] of b.parity) have.set(i, p);
      const data = reconstruct(b.k, e.size, have);
      for (const i of missing) e.out.set(data[i], (b.base + i) * e.size);
      e.rebuilt = true;
    }
    b.parity.clear();
  }

  complete(e, t) {
    e.done = true;
    this.open.delete(e);
    const st = this.stats;
    st.frames++;
    st.frameBytes += e.len;
    if (e.rebuilt) st.rebuilt++;
    if (e.nacks) st.repaired++;
    const out = e.out.subarray(0, e.len);
    e.out = null;
    this.h.deliver(out, t, e.first, e.nacks > 0);
  }

  /** Runs the NACKs and give-ups due at t. */
  tick(t) {
    const rtt = this.h.rtt() || 50;
    const interval = this.h.interval() || 1000 / 60;
    // The same sums as nextDue (t - a >= b and t >= a + b can differ in
    // floating point: a timer due at a + b must find the frame due).
    for (const e of [...this.open]) {
      if (!e.stallAt) {
        if (!((e.sentAt && t >= e.sentAt + GRACE_MS) || t >= e.last + quietMs(interval))) continue;
        e.stallAt = t;
      }
      if (t >= e.stallAt + giveUpMs(rtt)) {
        this.open.delete(e);
        if (e.ph) { this.frames.delete(frameKey(e.gen, e.seq)); continue; }
        e.lost = true;
        e.out = null;
        this.stats.lost++;
        this.h.lost(e.gen, e.seq);
        continue;
      }
      if (!e.nackAt || t >= e.nackAt + retryMs(rtt)) this.nack(e, t);
    }
    // Forget frames long done with, counting the shards they never got.
    for (const [key, e] of this.frames) {
      if (t < e.first + FORGET_MS) break;
      if (this.open.has(e)) continue;
      this.frames.delete(key);
      if (!e.ph) this.account(e);
    }
  }

  // The shards of frame e's first transmission (data and parity sent with
  // it) that never arrived: a block none of whose shards came counts its
  // data shards.
  account(e) {
    for (const b of e.blocks) {
      if (b.m < 0) { this.stats.shardsLost += b.k; continue; }
      let got = 0;
      for (const i of b.seen) if (i < b.k + b.m) got++;
      this.stats.shardsLost += b.k + b.m - got;
    }
  }

  nack(e, t) {
    const blocks = [];
    if (!e.ph) {
      for (const b of e.blocks) {
        // A second ask for a block: one spare (the first answer was lost too).
        if (!b.done) blocks.push({ base: b.base, need: b.k - b.have + (e.nacks ? 1 : 0) });
      }
      if (!blocks.length) return;
    } else {
      this.stats.wholeNacks++;
    }
    e.nackAt = t;
    e.nacks++;
    this.stats.nacks++;
    this.h.nack(fecNack(e.gen, e.seq, blocks));
  }

  /** When tick must run next (ms, absolute), or Infinity: nothing to do. */
  nextDue() {
    const rtt = this.h.rtt() || 50;
    const interval = this.h.interval() || 1000 / 60;
    let due = Infinity;
    for (const e of this.open) {
      if (!e.stallAt) {
        due = Math.min(due, e.last + quietMs(interval), e.sentAt ? e.sentAt + GRACE_MS : Infinity);
      } else {
        due = Math.min(due, e.stallAt + giveUpMs(rtt), e.nackAt + retryMs(rtt));
      }
    }
    if (due === Infinity && this.frames.size) due = this.frames.values().next().value.first + FORGET_MS;
    return due;
  }

  /** The counters (cumulative) and the frames still open. */
  summary() { return { ...this.stats, open: this.open.size }; }
}
