// Decoder self-test (guide step 4.1), run by the stream worker while it connects.
//
// Every codec family the browser decodes gets a ten-frame clip
// (decoder-selftest-clips.js: a key frame, then P frames only, nothing to
// reorder) fed one chunk at a time, each chunk waiting for its own output
// before the next goes in. A decoder fit for streaming outputs every frame
// after its own chunk. One that holds frames back (it waits for more input
// before it outputs, e.g. sized for B-frame reordering) shows it at the first
// chunk already, and would add that many frame intervals to every frame of
// the stream. Such decoders are avoided: a hardware decoder that holds frames
// back is reported to the host as no hardware decoder (the host prefers a
// family the browser decodes in hardware), and the stream decodes in software
// when that family's software decoder passes.

import { CLIPS } from './decoder-selftest-clips.js';

export { CLIPS };

// The first output may wait for the decoder to start (a hardware decoder's
// set-up); a decoder that only answers later is reported as a slow start,
// not as holding frames back, if it then keeps up frame by frame.
export const FIRST_OUTPUT_MS = 1000;
export const NEXT_OUTPUT_MS = 100; // a 640x360 frame
const FRAME_US = 16667;

function b64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/**
 * Decodes the clip of a family with the first supported of `accels`
 * (hardwareAcceleration values). Result:
 *   supported   false: none of accels is supported (nothing else is set)
 *   accel       the hardwareAcceleration decoded with
 *   firstAfter  chunks submitted when the first output arrived (1: at once; null: no output)
 *   held        frames the decoder still held back when the test stopped (chunks - outputs)
 *   outputs     frames output, of frames submitted (sent)
 *   ok          first output after one chunk, nothing held back, no error
 *   error       the decoder's error message, or null
 *   firstMs     configure -> first output
 *   decodeMs    mean submit -> output of the later frames
 * Decoder and Chunk replace VideoDecoder and EncodedVideoChunk (tests).
 */
export async function selfTestDecoder(family, accels, opts = {}) {
  const { Decoder = globalThis.VideoDecoder, Chunk = globalThis.EncodedVideoChunk, firstOutputMs = FIRST_OUTPUT_MS, nextOutputMs = NEXT_OUTPUT_MS } = opts;
  const clip = CLIPS[family];
  if (!clip || !Decoder) return { family, supported: false };
  let config = null;
  for (const hardwareAcceleration of accels) {
    const c = { codec: clip.codec, codedWidth: clip.width, codedHeight: clip.height, optimizeForLatency: true, hardwareAcceleration };
    const s = await Promise.resolve().then(() => Decoder.isConfigSupported(c)).catch(() => ({ supported: false }));
    if (s?.supported) { config = c; break; }
  }
  if (!config) return { family, supported: false };
  const data = clip.frames.map(b64);
  const r = {
    family, supported: true, accel: config.hardwareAcceleration, codec: clip.codec, frames: data.length,
    sent: 0, outputs: 0, firstAfter: null, held: 0, ok: false, error: null, firstMs: null, decodeMs: null,
  };
  const now = () => performance.now();
  const sentAt = [];
  let decodeSum = 0;
  let decodeN = 0;
  let wake = null;
  const t0 = now();
  const dec = new Decoder({
    output: (f) => {
      const i = Math.round(f.timestamp / FRAME_US);
      f.close();
      const t = now();
      if (!r.outputs) { r.firstAfter = sentAt.length; r.firstMs = t - t0; }
      else if (sentAt[i] !== undefined) { decodeSum += t - sentAt[i]; decodeN++; }
      r.outputs++;
      wake?.();
    },
    error: (e) => { r.error = e?.message || String(e); wake?.(); },
  });
  // Resolves when n frames are out, on an error, or after ms.
  const waitFor = (n, ms) => new Promise((resolve) => {
    if (r.outputs >= n || r.error) { resolve(); return; }
    const done = () => { clearTimeout(timer); wake = null; resolve(); };
    const timer = setTimeout(done, ms);
    wake = () => { if (r.outputs >= n || r.error) done(); };
  });
  try {
    dec.configure(config);
    let lastLag = 0;
    let same = 0;
    for (let i = 0; i < data.length && !r.error; i++) {
      sentAt.push(now());
      dec.decode(new Chunk({ type: i ? 'delta' : 'key', timestamp: i * FRAME_US, data: data[i] }));
      await waitFor(i + 1, i ? nextOutputMs : firstOutputMs);
      // A decoder that holds frames back lags by the same count after every
      // chunk once it has started: three in a row settle it.
      const lag = i + 1 - r.outputs;
      same = lag > 0 && r.outputs > 0 && lag === lastLag ? same + 1 : 0;
      lastLag = lag;
      if (same >= 3) break;
    }
  } catch (e) {
    r.error = e?.message || String(e);
  } finally {
    // Not flush(): the remaining outputs do not matter, and the test must
    // not hold up the session start any longer.
    try { dec.close(); } catch {}
  }
  r.sent = sentAt.length;
  r.held = r.sent - r.outputs;
  r.decodeMs = decodeN ? +(decodeSum / decodeN).toFixed(2) : null;
  r.firstMs = r.firstMs === null ? null : +r.firstMs.toFixed(1);
  r.ok = !r.error && r.firstAfter === 1 && r.held === 0;
  return r;
}

/** The decoder demonstrably holds frames back (not merely slow to start, nor failing). */
export const holdsFrames = (r) => !!r?.supported && !r.error && r.outputs > 0 && r.held > 0;

/**
 * Tests the families the browser decodes (decoders: [{ family, hw }], hw =
 * isConfigSupported with prefer-hardware) as the stream would decode them
 * (preferHW: the user's decoder setting) and decides per family:
 *   hw        the hardware decoder's result (preferHW and a hardware decoder)
 *   sw        the software (or any) decoder's result: without hardware, or
 *             when the hardware decoder held frames back
 *   software  decode this family in software: its hardware decoder held
 *             frames back and the software decoder passed
 *   reportHW  the hello's hw flag: a hardware decoder that does not hold frames back
 *   text      one line for the overlay and the log
 * The families run in parallel: a decoder that holds frames back costs about
 * FIRST_OUTPUT_MS + 3 x NEXT_OUTPUT_MS, the others a few frame decodes.
 */
export async function runSelfTests(decoders, preferHW, opts = {}) {
  return Promise.all(decoders.map(async (d) => {
    const t = { family: d.family };
    if (preferHW && d.hw) {
      t.hw = await selfTestDecoder(d.family, ['prefer-hardware'], opts);
      if (holdsFrames(t.hw)) t.sw = await selfTestDecoder(d.family, ['prefer-software'], opts);
    } else {
      t.sw = await selfTestDecoder(d.family, preferHW ? ['no-preference'] : ['prefer-software', 'no-preference'], opts);
    }
    t.software = holdsFrames(t.hw) && !!t.sw?.ok;
    t.reportHW = !!d.hw && !holdsFrames(t.hw);
    t.text = describe(t);
    return t;
  }));
}

const NAMES = { h264: 'H.264', hevc: 'HEVC', av1: 'AV1' };
const ACCEL = { 'prefer-hardware': 'HW', 'prefer-software': 'SW', 'no-preference': 'any' };

function verdict(r) {
  if (!r?.supported) return 'not supported';
  if (r.error) return `error (${r.error})`;
  if (r.ok) return r.decodeMs === null ? '✓' : `✓ ${r.decodeMs} ms/frame`;
  if (holdsFrames(r)) return `holds ${r.held} frame${r.held > 1 ? 's' : ''} back`;
  if (!r.outputs) return 'no output';
  return `first output after ${r.firstAfter} chunks (slow start)`;
}

function describe(t) {
  const name = NAMES[t.family] || t.family;
  const parts = [];
  if (t.hw) parts.push(`${ACCEL[t.hw.accel] || 'HW'} ${verdict(t.hw)}`);
  if (t.sw) parts.push(`${t.sw.supported ? ACCEL[t.sw.accel] : 'SW'} ${verdict(t.sw)}`);
  return `${name} ${parts.join(', ')}${t.software ? ' → decoding in software' : ''}`;
}
