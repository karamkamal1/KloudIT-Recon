// Wire protocol constants and encoders. Mirrors internal/proto (Go).
// All integers are little-endian.

export const KIND_CONTROL = 0x43; // 'C'
export const KIND_INPUT = 0x49; // 'I'

// WebTransport session close codes (mirror of internal/transport Code*).
export const CLOSE_AUTH = 4; // the host refused the hello's ticket (direct path, UDP relay)

export const WS_CONTROL = 0;
export const WS_INPUT = 1;
export const WS_FRAME = 2;
export const WS_DATAGRAM = 3;

export const DG_AUDIO = 0x10;
export const DG_CURSOR_POS = 0x11;
export const DG_VIDEO_SHARD = 0x12; // a shard of a video frame, "datagram + FEC" mode (fec.js)
export const DG_MOUSE_REL = 0x20;
export const DG_MOUSE_ABS = 0x21;
export const DG_GAMEPAD = 0x22;
export const DG_RUMBLE = 0x23;
export const DG_PING = 0x30;
export const DG_PONG = 0x31;
export const DG_FRAME_ACK = 0x40;
export const DG_RATE_REPORT = 0x41;
export const DG_FEC_NACK = 0x42; // the shards a video frame still needs (fec.js)

export const IN_KEY = 1;
export const IN_MOUSE_BUTTON = 2;
export const IN_WHEEL = 3;
export const IN_RELEASE_ALL = 4;
export const IN_TEXT = 5;

export const FRAME_HEADER_LEN = 24;
export const FRAME_TYPE_VIDEO = 1;
export const FRAME_FLAG_KEY = 1;
export const FRAME_FLAG_EXT = 0x80; // TLV extension block after the header

// Frame header extension tags. Timestamps are host-clock µs (same clock as sendUs).
export const EXT_TAGS = {
  1: 'presentUs', 2: 'captureUs', 3: 'encodeSubmitUs', 4: 'encodeDoneUs',
  5: 'refFloor', 6: 'ltrSlot', 7: 'temporalLayer', 8: 'thinned',
};

// Hello version: 2 asks the host for extended frame headers, 3 says the client
// handles reference recovery (VideoConfig.recovery "ltr" / "invalidate"), 4
// that it reads the "thinned" tag (the host may then leave out discardable
// frames under congestion: temporal SVC thinning).
export const HELLO_VERSION = 4;
export const FEATURE_FRAME_EXT = 'frame-ext';
// Hosts with the delay-based rate controller (GUIDE 2.2) list this feature:
// the client sends a rate report (DG_RATE_REPORT) every 20-50 ms and no
// delay-based "congestion" messages (the decoder's still go).
export const FEATURE_RATE_REPORT = 'rate-report';
export const RATE_REPORT_OWD = 1; // owdP50Us / owdMaxUs valid
export const RATE_REPORT_FRAME = 2; // gen / lastSeq name a received frame
export const RATE_REPORT_SHARDS = 4; // shards / shardsLost follow (48 bytes)
// "Datagram + FEC" video mode (GUIDE 2.5, fec.js; mirror of
// internal/proto/fec.go): clients on WebTransport send hello.fec =
// HELLO_FEC_VERSION, and a host that may send them video frames as shards
// (DG_VIDEO_SHARD) lists FEATURE_VIDEO_FEC.
export const HELLO_FEC_VERSION = 1;
export const FEATURE_VIDEO_FEC = 'video-fec';
// The host takes the "hold" row (frame pacing wait) in the client's stage
// report; without it the client reports hold and draw as one draw row.
export const FEATURE_STAGE_HOLD = 'stage-hold';
// Hosts whose configuration allows HDR10 streams ("hdr": "auto"; step 4.5).
// The client offers HDR in its prefs (hello / settings prefs.hdr: { mode,
// display, canvas, decoders, why }); an HDR10 video config carries hdr,
// bitDepth 10, colorSpace (VideoColorSpaceInit) and hdrMetadata, and hdrNote
// says why a generation is not HDR to clients that offered it.
export const FEATURE_HDR = 'hdr10';

export const AUDIO_OPUS = 1;
export const AUDIO_PCM = 2;

// Loss recovery (control messages, mirror of internal/proto/control.go).
// {"t":"dropped","gen":g,"fromSeq":s,"count":n}: the host discarded n frames
// of generation g from seq s (queue overflow, failed stream); they never
// arrive. VideoConfig.recovery: what to do about a confirmed loss.
export const MSG_DROPPED = 'dropped';
export const RECOVERY_SKIP = 'skip'; // the encoder heals itself (intra refresh): skip the frame
export const RECOVERY_KEYFRAME = 'keyframe'; // request a key frame
// Reference recovery (native helper, GUIDE 3.5): the encoder answers a loss
// with a recovery frame (frame extension refFloor < the lost seq) coded from
// frames the client decoded before the loss: an acknowledged long-term
// reference (ltr, AMF) or after invalidating the lost frames (invalidate,
// NVENC). Decode nothing from the lost frame on until that frame (or a key
// frame); report losses the host did not ({"t":"lost","gen","fromSeq"}).
export const RECOVERY_LTR = 'ltr';
export const RECOVERY_INVALIDATE = 'invalidate';
export const MSG_LOST = 'lost';
// {"t":"congestion","reason":"decoder"}: the decoder fell behind and was flushed (restart at once).
export const CONGESTION_DECODER = 'decoder';
const MAX_DROPPED = 1024;

/** The frames a "dropped" message reports: { gen, from, count } (count 1..1024), or null if malformed. */
export function parseDropped(m) {
  if (m?.t !== MSG_DROPPED || !Number.isInteger(m.gen) || !Number.isInteger(m.fromSeq) || m.fromSeq < 0) return null;
  const count = Number.isInteger(m.count) && m.count > 0 ? Math.min(m.count, MAX_DROPPED) : 1;
  return { gen: m.gen, from: m.fromSeq, count };
}

const RECOVERY_MODES = [RECOVERY_SKIP, RECOVERY_LTR, RECOVERY_INVALIDATE];

/** A VideoConfig's recovery mode; hosts before the field (or unknown values) mean keyframe. */
export const recoveryOf = (cfg) => (RECOVERY_MODES.includes(cfg?.recovery) ? cfg.recovery : RECOVERY_KEYFRAME);

/** Whether a recovery mode waits for the encoder's recovery frame (ltr, invalidate). */
export const isRefRecovery = (mode) => mode === RECOVERY_LTR || mode === RECOVERY_INVALIDATE;

/**
 * Whether a frame (parsed header) ends the wait after a loss at seq `lostFrom`
 * under reference recovery: a key frame, or a recovery frame whose refFloor
 * (the newest earlier frame it or a later frame may reference) is older than
 * the lost frame.
 */
export const endsRecovery = (h, lostFrom) => h.key || (h.ext?.refFloor !== undefined && h.ext.refFloor < lostFrom);

/**
 * The seqs of the frames the host left out on purpose (temporal SVC
 * thinning: frames no other frame references) among the 32 before a frame
 * (parsed header): its "thinned" mask, bit i = seq - 1 - i. Not losses: skip
 * them without waiting or recovering. Oldest first.
 */
export function thinnedSeqs(h) {
  const m = h.ext?.thinned;
  const out = [];
  if (!m) return out;
  for (let i = 31; i >= 0; i--) if ((m >>> i) & 1 && h.seq - 1 - i >= 0) out.push(h.seq - 1 - i);
  return out;
}

const enc = new TextEncoder();

/** Length-prefix a message (u32 LE + payload). */
export function frameMsg(payload) {
  const out = new Uint8Array(4 + payload.byteLength);
  new DataView(out.buffer).setUint32(0, payload.byteLength, true);
  out.set(payload, 4);
  return out;
}

export function jsonBytes(obj) {
  return enc.encode(JSON.stringify(obj));
}

export function keyEvent(sc, ext, down) {
  const b = new Uint8Array(4);
  b[0] = IN_KEY;
  b[1] = sc & 0xff;
  b[2] = sc >> 8;
  b[3] = (down ? 1 : 0) | (ext ? 2 : 0);
  return b;
}

export function buttonEvent(button, down) {
  return new Uint8Array([IN_MOUSE_BUTTON, button, down ? 1 : 0]);
}

export function wheelEvent(dy, dx) {
  const b = new Uint8Array(6);
  const v = new DataView(b.buffer);
  b[0] = IN_WHEEL;
  v.setInt16(2, dy, true);
  v.setInt16(4, dx, true);
  return b;
}

export function releaseAllEvent() {
  return new Uint8Array([IN_RELEASE_ALL]);
}

export function textEvent(s) {
  const t = enc.encode(s);
  const b = new Uint8Array(1 + t.length);
  b[0] = IN_TEXT;
  b.set(t, 1);
  return b;
}

export function mouseRel(seq, cx, cy) {
  const b = new Uint8Array(16);
  const v = new DataView(b.buffer);
  b[0] = DG_MOUSE_REL;
  v.setUint32(4, seq >>> 0, true);
  v.setInt32(8, cx | 0, true);
  v.setInt32(12, cy | 0, true);
  return b;
}

export function mouseAbs(seq, x, y) {
  const b = new Uint8Array(12);
  const v = new DataView(b.buffer);
  b[0] = DG_MOUSE_ABS;
  v.setUint32(4, seq >>> 0, true);
  v.setUint16(8, x, true);
  v.setUint16(10, y, true);
  return b;
}

export function gamepadState(idx, connected, seq, s) {
  const b = new Uint8Array(20);
  const v = new DataView(b.buffer);
  b[0] = DG_GAMEPAD;
  b[1] = idx;
  b[2] = connected ? 1 : 0;
  v.setUint32(4, seq >>> 0, true);
  v.setUint16(8, s.buttons, true);
  b[10] = s.lt;
  b[11] = s.rt;
  v.setInt16(12, s.lx, true);
  v.setInt16(14, s.ly, true);
  v.setInt16(16, s.rx, true);
  v.setInt16(18, s.ry, true);
  return b;
}

/**
 * A clock-sync ping. Since step 4.6 it carries minRttMs, the smallest RTT of
 * the recent pings (0: none yet), as u32 µs after t0: the host picks the Opus
 * frame duration by it (5 ms on a LAN, 10 ms over a WAN). Hosts before it
 * ignore the extra bytes.
 */
export function ping(id, t0, minRttMs = 0) {
  const b = new Uint8Array(20);
  const v = new DataView(b.buffer);
  b[0] = DG_PING;
  v.setUint32(4, id >>> 0, true);
  v.setFloat64(8, t0, true);
  v.setUint32(16, Math.max(0, Math.min(4294967295, Math.round(minRttMs * 1000))), true);
  return b;
}

// Opus frame durations in 48 kHz samples by TOC config (RFC 6716 3.1): SILK
// 10/20/40/60 ms, Hybrid 10/20 ms, CELT 2.5/5/10/20 ms.
const OPUS_FRAME = [480, 960, 1920, 2880, 480, 960, 1920, 2880, 480, 960, 1920, 2880, 480, 960, 480, 960,
  120, 240, 480, 960, 120, 240, 480, 960, 120, 240, 480, 960, 120, 240, 480, 960];

/**
 * The samples per channel (48 kHz) an audio datagram carries: from the Opus
 * packet's TOC byte (its frame duration and count) or the PCM payload size;
 * 0 when malformed. Opus packets carry their duration, so the host can change
 * the frame duration without a new configuration (step 4.6).
 */
export function audioPacketSamples(d) {
  if (d.length < 9) return 0;
  if (d[1] === AUDIO_PCM) return (d.length - 8) >> 2;
  if (d[1] !== AUDIO_OPUS) return 0;
  const toc = d[8];
  const code = toc & 3;
  const frames = code === 0 ? 1 : code < 3 ? 2 : d.length > 9 ? d[9] & 0x3f : 0;
  return OPUS_FRAME[toc >> 3] * frames;
}

export function frameAck(gen, seq, owdUs, decodeUs) {
  const b = new Uint8Array(16);
  const v = new DataView(b.buffer);
  b[0] = DG_FRAME_ACK;
  b[1] = gen;
  v.setUint32(4, seq >>> 0, true);
  v.setInt32(8, Math.max(-2147483648, Math.min(2147483647, Math.round(owdUs))), true);
  v.setUint32(12, Math.max(0, Math.min(4294967295, Math.round(decodeUs))), true);
  return b;
}

/**
 * The rate report datagram (mirror of proto.RateReport, 40 bytes; 48 with
 * RATE_REPORT_SHARDS: the video shards received and lost): the counters
 * (frames, bytes, lost, audio, shards, shardsLost) are cumulative and wrap at
 * 2^32; the one-way delays (µs) are those of the frames since the previous
 * report.
 */
export function rateReport(r) {
  const shards = (r.flags & RATE_REPORT_SHARDS) !== 0;
  const b = new Uint8Array(shards ? 48 : 40);
  const v = new DataView(b.buffer);
  const u32 = (x) => Math.floor(x) >>> 0;
  const i32 = (x) => Math.max(-2147483648, Math.min(2147483647, Math.round(x)));
  b[0] = DG_RATE_REPORT;
  b[1] = r.flags & 0xff;
  b[2] = r.gen & 0xff;
  v.setUint32(4, u32(r.timeMs), true);
  v.setUint32(8, u32(r.lastSeq), true);
  v.setUint32(12, u32(r.frames), true);
  v.setUint32(16, u32(r.bytes % 4294967296), true);
  v.setInt32(20, i32(r.owdP50Us), true);
  v.setInt32(24, i32(r.owdMaxUs), true);
  v.setUint32(28, u32(r.lost), true);
  v.setUint32(32, u32(r.audio), true);
  v.setUint16(36, Math.max(0, Math.min(65535, r.decodeQueue | 0)), true);
  if (shards) {
    v.setUint32(40, u32((r.shards || 0) % 4294967296), true);
    v.setUint32(44, u32((r.shardsLost || 0) % 4294967296), true);
  }
  return b;
}

/**
 * Parse a frame header: 24 fixed bytes, then (FRAME_FLAG_EXT) u16 extLen and
 * entries "u8 tag | u8 len | value LE". Unknown tags are skipped. Returns null
 * for a malformed frame; the payload starts at headerLen.
 */
export function parseFrameHeader(buf) {
  if (buf.byteLength < FRAME_HEADER_LEN) return null;
  const v = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  const h = {
    type: buf[0],
    key: (buf[1] & FRAME_FLAG_KEY) !== 0,
    gen: buf[2],
    seq: v.getUint32(4, true),
    ptsUs: Number(v.getBigUint64(8, true)),
    sendUs: Number(v.getBigUint64(16, true)),
    ext: null,
    headerLen: FRAME_HEADER_LEN,
  };
  if (!(buf[1] & FRAME_FLAG_EXT)) return h;
  if (buf.byteLength < FRAME_HEADER_LEN + 2) return null;
  const end = FRAME_HEADER_LEN + 2 + v.getUint16(FRAME_HEADER_LEN, true);
  if (end > buf.byteLength) return null;
  const ext = {};
  for (let p = FRAME_HEADER_LEN + 2; p < end;) {
    if (p + 2 > end || p + 2 + buf[p + 1] > end) return null;
    const tag = buf[p];
    const len = buf[p + 1];
    const name = EXT_TAGS[tag];
    if (name) {
      if (len < 1 || len > 8) return null;
      let x = 0;
      for (let i = len - 1; i >= 0; i--) x = x * 256 + buf[p + 2 + i];
      ext[name] = x;
    }
    p += 2 + len;
  }
  h.ext = ext;
  h.headerLen = end;
  return h;
}

// ---------------------------------------------------------------------------
// Video config crop (mirror of proto.VideoConfig codedWidth, codedHeight,
// cropRight, cropBottom; omitted when there is no padding). An encoder that
// codes in blocks pads the picture at the right and bottom (AV1 on RDNA3 codes
// 1920x1080 as 1920x1082) and AV1 has no cropping window, so the decoder
// outputs the padding; the host announces it and the client shows only the
// top-left width x height pixels.

/**
 * The part of a decoded frame to show. frameW x frameH is the decoded picture
 * (VideoFrame.visibleRect), displayW x displayH its display size. Returns
 * { w, h } in display units (drawImage source coordinates) and { fx, fy }, the
 * visible share of the frame's width and height (texture coordinate scale).
 * Only what the decoder still outputs is cropped: a frame already at the
 * visible size is shown whole.
 */
export function visibleArea(cfg, frameW, frameH, displayW, displayH) {
  let fx = 1;
  let fy = 1;
  if (cfg && (cfg.cropRight > 0 || cfg.cropBottom > 0) && cfg.width > 0 && cfg.height > 0 && frameW > 0 && frameH > 0) {
    fx = Math.min(1, cfg.width / frameW);
    fy = Math.min(1, cfg.height / frameH);
  }
  return { w: displayW * fx, h: displayH * fy, fx, fy };
}

// ---------------------------------------------------------------------------
// Frame barcode (mirror of internal/proto/barcode.go): a 16-bit value and its
// CRC-8 drawn as an 8x3 grid of black/white cells in the picture's top-left
// corner, row-major, most significant bit first (cell k shows bit 23-k of
// value << 8 | crc; white = 1). The test pattern draws each frame's seq with
// 16 px cells; tools/latency-test/index.html draws the host's wall-clock ms
// with cells of 1/96 of the picture width.

export const BARCODE_COLS = 8;
export const BARCODE_ROWS = 3;
export const BARCODE_BITS = BARCODE_COLS * BARCODE_ROWS;
export const BARCODE_CELL = 16;
export const BARCODE_WALLCLOCK_CELLS = 96;
export const BARCODE_DARK = 96; // mean luma below: 0; above BARCODE_LIGHT: 1; between: no barcode
export const BARCODE_LIGHT = 160;
export const FEATURE_BARCODE_SEQ = 'barcode-seq';

/** CRC-8 (poly 0x07, init 0, xorout 0x55) over the value's two bytes, high first. */
export function barcodeCRC(v) {
  let crc = 0;
  for (const b of [(v >> 8) & 0xff, v & 0xff]) {
    crc ^= b;
    for (let i = 0; i < 8; i++) crc = crc & 0x80 ? ((crc << 1) ^ 0x07) & 0xff : (crc << 1) & 0xff;
  }
  return crc ^ 0x55;
}

/** The 24 bits drawn for value v. */
export const barcodeWord = (v) => ((v & 0xffff) * 256) + barcodeCRC(v & 0xffff);

/** Value of a word whose CRC checks out, else null. */
export function barcodeDecode(word) {
  if (word < 0 || word >= 1 << BARCODE_BITS) return null;
  const v = word >>> 8;
  return (word & 0xff) === barcodeCRC(v) ? v : null;
}

/** Decode the cells' mean luma (row-major, 0-255); null unless every cell is clearly dark or light and the CRC holds. */
export function barcodeDecodeLuma(luma) {
  if (luma.length !== BARCODE_BITS) return null;
  let word = 0;
  for (let k = 0; k < BARCODE_BITS; k++) {
    const l = luma[k];
    if (l > BARCODE_LIGHT) word += 2 ** (BARCODE_BITS - 1 - k);
    else if (!(l < BARCODE_DARK)) return null;
  }
  return barcodeDecode(word);
}

/** The inner half of cell k (what a reader averages) for cells of `cell` px: [x0, y0, x1, y1), x1/y1 exclusive. */
export function barcodeSampleRect(k, cell) {
  const c = k % BARCODE_COLS;
  const r = Math.floor(k / BARCODE_COLS);
  const x0 = Math.round((c + 0.25) * cell);
  const y0 = Math.round((r + 0.25) * cell);
  return [x0, y0, Math.max(Math.round((c + 0.75) * cell), x0 + 1), Math.max(Math.round((r + 0.75) * cell), y0 + 1)];
}

/** XInput button bits used by the host's virtual controller. */
export const XUSB = {
  UP: 0x0001, DOWN: 0x0002, LEFT: 0x0004, RIGHT: 0x0008,
  START: 0x0010, BACK: 0x0020, LTHUMB: 0x0040, RTHUMB: 0x0080,
  LB: 0x0100, RB: 0x0200, GUIDE: 0x0400,
  A: 0x1000, B: 0x2000, X: 0x4000, Y: 0x8000,
};

/** Convert a standard-mapping Gamepad into an XInput-style state. */
export function gamepadToXusb(gp) {
  const b = gp.buttons;
  const pressed = (i) => (b[i] ? b[i].pressed || b[i].value > 0.5 : false);
  let buttons = 0;
  const map = [
    [0, XUSB.A], [1, XUSB.B], [2, XUSB.X], [3, XUSB.Y],
    [4, XUSB.LB], [5, XUSB.RB], [8, XUSB.BACK], [9, XUSB.START],
    [10, XUSB.LTHUMB], [11, XUSB.RTHUMB], [12, XUSB.UP], [13, XUSB.DOWN],
    [14, XUSB.LEFT], [15, XUSB.RIGHT], [16, XUSB.GUIDE],
  ];
  for (const [i, bit] of map) if (pressed(i)) buttons |= bit;
  const axis = (i, invert) => {
    let v = gp.axes[i] || 0;
    if (Math.abs(v) < 0.04) v = 0; // tiny deadzone; games apply their own
    if (invert) v = -v;
    return Math.max(-32768, Math.min(32767, Math.round(v * 32767)));
  };
  const trig = (i) => Math.round(Math.max(0, Math.min(1, b[i] ? b[i].value : 0)) * 255);
  return {
    buttons, lt: trig(6), rt: trig(7),
    lx: axis(0, false), ly: axis(1, true), rx: axis(2, false), ry: axis(3, true),
  };
}
