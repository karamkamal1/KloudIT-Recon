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
//
// Then (step 4.2) every family is timed on a 1920x1080 clip
// (decoder-timing-clips.js) with the decoder the stream would use: the
// median time from decode() to the output of its P frames, fed one at a time
// like the stream's. The hello carries the times (decoders[].timing) and the
// host picks the codec family by them. The 640x360 clip above cannot do this:
// it mostly measures the fixed cost of a decode call, where a software
// decoder beats a hardware decoder's round trip to the GPU process.

import { CLIPS } from './decoder-selftest-clips.js';

export { CLIPS };

// The timing clips (226 kB) load when the self-test starts; a failed load is
// tried again by the next one.
let timingClips = null;
const loadTimingClips = () => (timingClips ??= import('./decoder-timing-clips.js').then((m) => m.TIMING_CLIPS)
  .catch((e) => { timingClips = null; throw e; }));

// The first output may wait for the decoder to start (a hardware decoder's
// set-up); a decoder that only answers later is reported as a slow start,
// not as holding frames back, if it then keeps up frame by frame. Each later
// chunk waits NEXT_OUTPUT_MS for its output; a decoder slower than that lags
// too, so after the last chunk the test waits up to FIRST_OUTPUT_MS more
// without new input: a slow decoder delivers the outputs still due, one that
// holds frames back outputs nothing more (only that counts as holding).
export const FIRST_OUTPUT_MS = 1000;
export const NEXT_OUTPUT_MS = 100; // a 640x360 frame
// Timing: each 1920x1080 frame may take this long before the next goes in
// (one at a time, as on the stream; a decoder slower than that is timed on
// overlapping frames, which only makes it look slower). At least
// MIN_TIMED frames must come out for a time.
export const TIMING_NEXT_OUTPUT_MS = 250;
export const MIN_TIMED = 4;
const FRAME_US = 16667;

function b64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/**
 * Decodes the clip of a family (opts.clip, else its hygiene clip) with the
 * first supported of `accels` (hardwareAcceleration values). Result:
 *   supported   false: none of accels is supported (nothing else is set)
 *   accel       the hardwareAcceleration decoded with
 *   firstAfter  chunks submitted when the first output arrived (1: at once; null: no output)
 *   held        frames the decoder still held back when the test stopped (chunks - outputs,
 *               after up to FIRST_OUTPUT_MS without new input)
 *   outputs     frames output, of frames submitted (sent)
 *   ok          first output after one chunk, nothing held back, no error
 *   error       the decoder's error message, or null
 *   firstMs     configure -> first output
 *   decodeMs    mean submit -> output of the later frames
 *   decodeP50   median of the same, over `timed` frames
 * Decoder and Chunk replace VideoDecoder and EncodedVideoChunk (tests).
 */
export async function selfTestDecoder(family, accels, opts = {}) {
  const { Decoder = globalThis.VideoDecoder, Chunk = globalThis.EncodedVideoChunk, firstOutputMs = FIRST_OUTPUT_MS, nextOutputMs = NEXT_OUTPUT_MS } = opts;
  const clip = opts.clip || CLIPS[family];
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
    sent: 0, outputs: 0, firstAfter: null, held: 0, ok: false, error: null, firstMs: null, decodeMs: null, decodeP50: null, timed: 0,
  };
  const now = () => performance.now();
  const sentAt = [];
  const times = [];
  let wake = null;
  const t0 = now();
  const dec = new Decoder({
    output: (f) => {
      const i = Math.round(f.timestamp / FRAME_US);
      f.close();
      const t = now();
      if (!r.outputs) { r.firstAfter = sentAt.length; r.firstMs = t - t0; }
      else if (sentAt[i] !== undefined) times.push(t - sentAt[i]);
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
    await waitFor(sentAt.length, firstOutputMs);
  } catch (e) {
    r.error = e?.message || String(e);
  } finally {
    // Not flush(): the remaining outputs do not matter, and the test must
    // not hold up the session start any longer.
    try { dec.close(); } catch {}
  }
  r.sent = sentAt.length;
  r.held = r.sent - r.outputs;
  r.timed = times.length;
  if (times.length) {
    r.decodeMs = +(times.reduce((a, b) => a + b, 0) / times.length).toFixed(2);
    const sorted = [...times].sort((a, b) => a - b);
    const m = sorted.length >> 1;
    r.decodeP50 = +(sorted.length % 2 ? sorted[m] : (sorted[m - 1] + sorted[m]) / 2).toFixed(2);
  }
  r.firstMs = r.firstMs === null ? null : +r.firstMs.toFixed(1);
  r.ok = !r.error && r.firstAfter === 1 && r.held === 0;
  return r;
}

/** The decoder demonstrably holds frames back (not merely slow to start, nor failing). */
export const holdsFrames = (r) => !!r?.supported && !r.error && r.outputs > 0 && r.held > 0;

/**
 * Times a family's decoder (step 4.2): its 1920x1080 timing clip decoded
 * with `accel`, one frame at a time. Returns the hello's timing
 * { ms, w, h, n, accel } (ms: median decode() -> output of the n P frames
 * that came out), or null when the decoder failed or too few frames came out.
 */
export async function timeDecoder(family, accel, opts = {}) {
  const clips = opts.timingClips || (await loadTimingClips());
  const clip = clips?.[family];
  if (!clip) return null;
  const r = await selfTestDecoder(family, [accel], { ...opts, clip, nextOutputMs: opts.timingNextOutputMs ?? TIMING_NEXT_OUTPUT_MS });
  if (!r.supported || r.error || r.timed < MIN_TIMED) return null;
  return { ms: r.decodeP50, w: clip.width, h: clip.height, n: r.timed, accel: r.accel };
}

// The result the stream's decoder of a family corresponds to, or null when
// it would decode with a hardware decoder that holds frames back (its time
// per frame is then the hold, not decoding work).
function streamResult(t) {
  if (t.software) return t.sw;
  const r = t.hw || t.sw;
  return r?.supported && !r.error && r.outputs > 0 && !holdsFrames(r) ? r : null;
}

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
 *   timing    the hello's timing (timeDecoder) with the decoder the stream would use, or null
 *   text      one line for the overlay and the log
 * The families' hygiene tests run in parallel: a decoder that holds frames back
 * costs about 2 x FIRST_OUTPUT_MS + 3 x NEXT_OUTPUT_MS, the others a few frame
 * decodes. The timing runs follow one family at a time, so no two decoders
 * compete for the GPU's decode engine or the CPU while timed (eight frames
 * each); the hygiene tests just warmed every decoder up.
 */
export async function runSelfTests(decoders, preferHW, opts = {}) {
  if (!opts.timingClips) loadTimingClips().catch(() => null); // fetch while the hygiene tests run
  const tests = await Promise.all(decoders.map(async (d) => {
    const t = { family: d.family };
    if (preferHW && d.hw) {
      t.hw = await selfTestDecoder(d.family, ['prefer-hardware'], opts);
      if (holdsFrames(t.hw)) t.sw = await selfTestDecoder(d.family, ['prefer-software'], opts);
    } else {
      t.sw = await selfTestDecoder(d.family, preferHW ? ['no-preference'] : ['prefer-software', 'no-preference'], opts);
    }
    t.software = holdsFrames(t.hw) && !!t.sw?.ok;
    t.reportHW = !!d.hw && !holdsFrames(t.hw);
    return t;
  }));
  for (const t of tests) {
    const r = streamResult(t);
    t.timing = r ? await timeDecoder(t.family, r.accel, opts).catch(() => null) : null;
    t.text = describe(t);
  }
  return tests;
}

/** The hello's decoder entry of a family: the probe's, with the self-test's hw flag and timing. */
export function helloDecoder(d, t) {
  if (!t) return d;
  const out = { ...d, hw: t.reportHW };
  if (t.timing) out.timing = t.timing;
  return out;
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
  const timing = t.timing ? ` · timed ${t.timing.h}p: ${t.timing.ms} ms/frame` : '';
  return `${name} ${parts.join(', ')}${t.software ? ' → decoding in software' : ''}${timing}`;
}
