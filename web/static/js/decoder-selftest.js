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
// like the stream's, the families interleaved frame by frame. The hello
// carries the times (decoders[].timing) and the host picks the codec family
// by them. The 640x360 clip above cannot do this: it mostly measures the
// fixed cost of a decode call, where a software decoder beats a hardware
// decoder's round trip to the GPU process.

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
// Timing: each 1920x1080 P frame may take this long; a family whose frame is
// not out by then gets no more (one frame in flight at a time, as on the
// stream). At least MIN_TIMED P frames must come out for a time.
export const TIMING_NEXT_OUTPUT_MS = 250;
export const MIN_TIMED = 4;
// The hello waits for the self-test on every connection (and the host waits
// 10 s for the hello), so the timing has a budget: TIMING_BUDGET_MS in all,
// the wait for the clips included, and TIMING_FAMILY_MS per family (its
// frames' decode() -> output, the key frame's decoder start included). A
// family not timed within them goes to the host without a time; the host then
// keeps its default order for it.
export const TIMING_BUDGET_MS = 1500;
export const TIMING_FAMILY_MS = 500;
const FRAME_US = 16667;

function b64(s) {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function median(xs) {
  const sorted = [...xs].sort((a, b) => a - b);
  const m = sorted.length >> 1;
  return +(sorted.length % 2 ? sorted[m] : (sorted[m - 1] + sorted[m]) / 2).toFixed(2);
}

// p, or a rejection after ms.
function within(p, ms) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`no answer in ${Math.round(ms)} ms`)), Math.max(0, ms));
    Promise.resolve(p).then((v) => { clearTimeout(timer); resolve(v); }, (e) => { clearTimeout(timer); reject(e); });
  });
}

/**
 * Decodes the hygiene clip of a family with the first supported of `accels`
 * (hardwareAcceleration values). Result:
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
    r.decodeP50 = median(times);
  }
  r.firstMs = r.firstMs === null ? null : +r.firstMs.toFixed(1);
  r.ok = !r.error && r.firstAfter === 1 && r.held === 0;
  return r;
}

/** The decoder demonstrably holds frames back (not merely slow to start, nor failing). */
export const holdsFrames = (r) => !!r?.supported && !r.error && r.outputs > 0 && r.held > 0;

/**
 * Times the decoders of several families (step 4.2): entries [{ family, accel }],
 * each on its 1920x1080 timing clip with `accel` (hardwareAcceleration). The
 * families are interleaved one frame at a time: every family's key frame, then
 * P frame 1 of every family, then P frame 2, ..., each frame going in once the
 * previous one is out. So no two decodes overlap (as on a stream; nor do two
 * decoders compete for the GPU's decode engine or the CPU), and a change of
 * load on the client during the pass falls on every family alike instead of
 * on whichever was timed then. Returns { [family]: timing }, the hello's
 * { ms, w, h, n, accel } (ms: median decode() -> output of the n P frames
 * that came out); a family is missing when its config is not supported, its
 * decoder failed, or fewer than MIN_TIMED P frames came out within its
 * budget (TIMING_FAMILY_MS, TIMING_BUDGET_MS) or before one was slower than
 * TIMING_NEXT_OUTPUT_MS.
 */
export async function timeDecoders(entries, opts = {}) {
  const { Decoder = globalThis.VideoDecoder, Chunk = globalThis.EncodedVideoChunk } = opts;
  const familyMs = opts.timingFamilyMs ?? TIMING_FAMILY_MS;
  const nextMs = opts.timingNextOutputMs ?? TIMING_NEXT_OUTPUT_MS;
  const now = () => performance.now();
  const end = now() + (opts.timingBudgetMs ?? TIMING_BUDGET_MS);
  const out = {};
  if (!Decoder || !entries.length) return out;
  const clips = await within(opts.timingClips ?? loadTimingClips(), end - now()).catch(() => null);
  const runs = [];
  for (const { family, accel } of entries) {
    const clip = clips?.[family];
    if (!clip) continue;
    const config = { codec: clip.codec, codedWidth: clip.width, codedHeight: clip.height, optimizeForLatency: true, hardwareAcceleration: accel };
    const s = await within(Promise.resolve().then(() => Decoder.isConfigSupported(config)), end - now()).catch(() => null);
    if (s?.supported) runs.push({ family, accel, clip, config, data: clip.frames.map(b64), sentAt: [], times: [], outputs: 0, error: null, used: 0, stopped: false });
  }
  let wake = null;
  // Resolves when r has n frames out, on its error, or after ms.
  const waitFor = (r, n, ms) => new Promise((resolve) => {
    if (r.outputs >= n || r.error) { resolve(); return; }
    const done = () => { clearTimeout(timer); wake = null; resolve(); };
    const timer = setTimeout(done, ms);
    wake = () => { if (r.outputs >= n || r.error) done(); };
  });
  try {
    for (const r of runs) {
      try {
        r.dec = new Decoder({
          output: (f) => {
            const i = Math.round(f.timestamp / FRAME_US);
            f.close();
            if (i > 0 && r.sentAt[i] !== undefined) r.times.push(now() - r.sentAt[i]);
            r.outputs++;
            wake?.();
          },
          error: (e) => { r.error = e?.message || String(e); wake?.(); },
        });
        r.dec.configure(r.config);
      } catch (e) {
        r.error = e?.message || String(e);
      }
    }
    const frames = Math.max(0, ...runs.map((r) => r.data.length));
    for (let i = 0; i < frames; i++) {
      for (const r of runs) {
        if (r.error || r.stopped || i >= r.data.length) continue;
        const left = Math.min(end - now(), familyMs - r.used);
        if (left <= 0) { r.stopped = true; continue; }
        const t = now();
        r.sentAt[i] = t;
        try {
          r.dec.decode(new Chunk({ type: i ? 'delta' : 'key', timestamp: i * FRAME_US, data: r.data[i] }));
        } catch (e) {
          r.error = e?.message || String(e);
          continue;
        }
        await waitFor(r, i + 1, Math.min(left, i ? nextMs : FIRST_OUTPUT_MS));
        r.used += now() - t;
        if (r.outputs < i + 1) r.stopped = true; // still decoding: the next frame would overlap it
      }
    }
  } finally {
    // Not flush(): a frame still in flight does not matter.
    for (const r of runs) { try { r.dec?.close(); } catch {} }
  }
  for (const r of runs) {
    if (!r.error && r.times.length >= MIN_TIMED) out[r.family] = { ms: median(r.times), w: r.clip.width, h: r.clip.height, n: r.times.length, accel: r.accel };
  }
  return out;
}

/** timeDecoders for one family: its timing, or null. */
export async function timeDecoder(family, accel, opts = {}) {
  return (await timeDecoders([{ family, accel }], opts))[family] ?? null;
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
 *   noSoftware  !preferHW, and the family has no software decoder (Chrome:
 *             HEVC decodes only in hardware): the stream decodes it with the
 *             browser's other decoder (no-preference)
 *   reportHW  the hello's hw flag: a hardware decoder that does not hold
 *             frames back, which the stream uses (never with !preferHW: the
 *             host then picks for a client that decodes in software)
 *   timing    the hello's timing (timeDecoders) with the decoder the stream
 *             would use, or null (also for noSoftware: a hardware decoder's
 *             time would win the host's choice for a family the user's
 *             setting avoids)
 *   text      one line for the overlay and the log
 * The families' hygiene tests run in parallel: a decoder that holds frames back
 * costs about 2 x FIRST_OUTPUT_MS + 3 x NEXT_OUTPUT_MS, the others a few frame
 * decodes. Then one timing pass for all of them (interleaved frame by frame, no
 * two decodes at once; eight frames each, TIMING_BUDGET_MS at most); the
 * hygiene tests just warmed every decoder up.
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
    t.noSoftware = !preferHW && !!t.sw?.supported && t.sw.accel !== 'prefer-software';
    t.reportHW = preferHW && !!d.hw && !holdsFrames(t.hw);
    return t;
  }));
  const entries = tests.filter((t) => !t.noSoftware).map((t) => ({ family: t.family, accel: streamResult(t)?.accel })).filter((e) => e.accel);
  const timings = await timeDecoders(entries, opts).catch(() => ({}));
  for (const t of tests) {
    t.timing = timings[t.family] ?? null;
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
  const how = t.software ? ' → decoding in software' : t.noSoftware ? ' → no software decoder (not timed)' : '';
  return `${name} ${parts.join(', ')}${how}${timing}`;
}
