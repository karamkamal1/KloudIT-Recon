// Wire protocol constants and encoders. Mirrors internal/proto (Go).
// All integers are little-endian.

export const KIND_CONTROL = 0x43; // 'C'
export const KIND_INPUT = 0x49; // 'I'

export const WS_CONTROL = 0;
export const WS_INPUT = 1;
export const WS_FRAME = 2;
export const WS_DATAGRAM = 3;

export const DG_AUDIO = 0x10;
export const DG_CURSOR_POS = 0x11;
export const DG_MOUSE_REL = 0x20;
export const DG_MOUSE_ABS = 0x21;
export const DG_GAMEPAD = 0x22;
export const DG_RUMBLE = 0x23;
export const DG_PING = 0x30;
export const DG_PONG = 0x31;
export const DG_FRAME_ACK = 0x40;

export const IN_KEY = 1;
export const IN_MOUSE_BUTTON = 2;
export const IN_WHEEL = 3;
export const IN_RELEASE_ALL = 4;
export const IN_TEXT = 5;

export const FRAME_HEADER_LEN = 24;
export const FRAME_FLAG_KEY = 1;

export const AUDIO_OPUS = 1;
export const AUDIO_PCM = 2;

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

export function ping(id, t0) {
  const b = new Uint8Array(16);
  const v = new DataView(b.buffer);
  b[0] = DG_PING;
  v.setUint32(4, id >>> 0, true);
  v.setFloat64(8, t0, true);
  return b;
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

/** Parse a 24-byte frame header. */
export function parseFrameHeader(buf) {
  const v = new DataView(buf.buffer, buf.byteOffset, FRAME_HEADER_LEN);
  return {
    type: buf[0],
    key: (buf[1] & FRAME_FLAG_KEY) !== 0,
    gen: buf[2],
    seq: v.getUint32(4, true),
    ptsUs: Number(v.getBigUint64(8, true)),
    sendUs: Number(v.getBigUint64(16, true)),
  };
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
