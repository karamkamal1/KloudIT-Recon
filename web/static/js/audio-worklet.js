// Low-latency audio playout. Samples arrive from the stream worker through a
// lock-free SharedArrayBuffer ring (or a MessagePort when the page is not
// cross-origin isolated). A jitter buffer absorbs network jitter: it holds
// about `target` of audio, refills to it (Auto: a little above it,
// refillLevel) after an underrun, and drops what
// grows above it (clock drift, bursts) so audio never drifts behind video.
//
// Target (step 4.6). Adaptive (default): 20 ms at the start, then what the
// last 10 s needed, between 10 ms and 60 ms: the deepest drop of the buffer
// level below its mean in a 250 ms window (packet size, network jitter, the
// audio device's render bursts; a delay spike shows as one deep drop), the
// largest of the last 10 s, plus a 2.5 ms margin, plus a bias that each
// underrun raises by 10 ms and that decays by 1 ms per second without one. On
// a clean LAN (5 ms packets) that is 10-20 ms; a jittery link moves it up to
// 60 ms. Fixed: the target set in the settings.
// A pause of the host's audio source is not jitter: WASAPI loopback sends
// nothing while nothing plays, so the buffer runs dry at the end of every
// sound. The host moves the pts on by such a pause, and the stream worker
// reports the first packet after one ({pause: true}, through the page): the
// underrun that ended the sound is then taken back (its count and its bias),
// so sounds with gaps keep the target where continuous audio has it.
// The level follows the target: a 250 ms window whose mean level is above it
// drops the excess, at most 5 ms per window, in one render quantum with a
// crossfade (the jump of a hard cut would click); far above it (a burst after
// a stall) the excess goes at once. Statistics go to the page once a second.

const WINDOW = 0.25; // s
const MIN_MS = 10;
const MAX_MS = 60;
const START_MS = 20;
const MARGIN_MS = 2.5;
const HISTORY_WINDOWS = 40; // 10 s
const UNDERRUN_BIAS_MS = 10;
const BIAS_DECAY_MS = 1; // per second without an underrun
const MAX_SKIP_MS = 5;

class ReconAudio extends AudioWorkletProcessor {
  constructor(options) {
    super();
    const o = options.processorOptions || {};
    this.ms = sampleRate / 1000;
    this.auto = o.auto !== false;
    this.fixedMs = o.targetMs ?? 30;
    this.target = Math.round(this.ms * (this.auto ? START_MS : this.fixedMs));
    this.ring = null;
    this.queue = [];
    this.queued = 0;
    this.buffering = true;
    this.underruns = 0;
    this.skippedMs = 0;
    this.bias = 0; // ms
    this.drops = []; // per window: mean level - lowest level, samples
    this.win = { n: 0, min: Infinity, sum: 0, frames: 0 };
    this.sinceUnderrun = 0; // samples played
    this.pauses = 0; // underruns taken back: the host's source paused
    this.undo = null; // the last underrun's state before it, until a pause can take it back
    this.dry = 0; // samples played since samples last arrived
    this.left = 0; // samples in the buffer after the last render quantum
    this.sinceReport = 0;
    this.tL = new Float32Array(128 + Math.ceil(MAX_SKIP_MS * this.ms)); // a render quantum + the most one window drops
    this.tR = new Float32Array(this.tL.length);
    if (o.sab) this.setRing(o.sab);
    this.port.onmessage = (ev) => {
      const m = ev.data;
      if (m && m.port) {
        m.port.onmessage = (e) => { this.queue.push(e.data); this.queued += e.data.length / 2; };
      } else if (m && m.sab) {
        this.setRing(m.sab);
      } else if (m && m.pause) {
        this.sourcePaused();
      } else if (m && (m.targetMs || m.auto !== undefined)) {
        if (m.targetMs) this.fixedMs = m.targetMs;
        if (m.auto !== undefined) this.auto = !!m.auto;
        this.retarget();
      }
    };
  }

  setRing(sab) {
    this.ring = { idx: new Int32Array(sab, 0, 2), data: new Float32Array(sab, 8) };
    this.ring.cap = this.ring.data.length / 2;
    this.buffering = true;
  }

  available() {
    if (this.ring) {
      const w = Atomics.load(this.ring.idx, 0);
      const r = Atomics.load(this.ring.idx, 1);
      return (w - r + this.ring.cap) % this.ring.cap;
    }
    return this.queued;
  }

  skip(n) {
    if (this.ring) {
      const r = Atomics.load(this.ring.idx, 1);
      Atomics.store(this.ring.idx, 1, (r + n) % this.ring.cap);
      return;
    }
    while (n > 0 && this.queue.length) {
      const head = this.queue[0];
      const frames = head.length / 2;
      if (frames <= n) { this.queue.shift(); this.queued -= frames; n -= frames; }
      else { this.queue[0] = head.subarray(n * 2); this.queued -= n; n = 0; }
    }
  }

  read(L, R, n) {
    if (this.ring) {
      let r = Atomics.load(this.ring.idx, 1);
      const d = this.ring.data;
      for (let i = 0; i < n; i++) {
        L[i] = d[2 * r];
        R[i] = d[2 * r + 1];
        if (++r === this.ring.cap) r = 0;
      }
      Atomics.store(this.ring.idx, 1, r);
      return;
    }
    let i = 0;
    while (i < n && this.queue.length) {
      const head = this.queue[0];
      const frames = head.length / 2;
      const take = Math.min(frames, n - i);
      for (let k = 0; k < take; k++, i++) { L[i] = head[2 * k]; R[i] = head[2 * k + 1]; }
      if (take === frames) this.queue.shift();
      else this.queue[0] = head.subarray(take * 2);
      this.queued -= take;
    }
  }

  // readSkipping plays n samples and drops k more: the n samples after the k
  // fade in over the n samples before them.
  readSkipping(L, R, n, k) {
    if (this.tL.length < n + k) { this.tL = new Float32Array(n + k); this.tR = new Float32Array(n + k); }
    const { tL, tR } = this;
    this.read(tL, tR, n + k);
    for (let i = 0; i < n; i++) {
      const w = (i + 1) / n;
      L[i] = tL[i] * (1 - w) + tL[i + k] * w;
      R[i] = tR[i] * (1 - w) + tR[i + k] * w;
    }
    this.skippedMs += k / this.ms;
  }

  // retarget sets the target from the measurements (adaptive) or the setting.
  retarget() {
    let ms = this.fixedMs;
    if (this.auto) {
      ms = START_MS;
      if (this.drops.length) ms = MARGIN_MS + Math.max(...this.drops) / this.ms;
      ms = Math.min(MAX_MS, Math.max(MIN_MS, ms + this.bias));
    }
    this.target = Math.round(ms * this.ms);
  }

  // sourcePaused takes back the last underrun: it was the host's source
  // pausing (the stream worker saw the pts move on by the pause), not the
  // network. The notice comes before the buffer has refilled, or soon after
  // (the excess of the higher target is then trimmed); one later than a
  // quarter of a second of playing after the underrun is about another one.
  sourcePaused() {
    const u = this.undo;
    if (!u) return;
    this.undo = null;
    this.underruns--;
    this.pauses++;
    this.bias = u.bias;
    this.sinceUnderrun += u.since;
    // The level's drop as the buffer ran dry is not jitter either: forget
    // the windows it fell in.
    this.drops = u.drops;
    this.win = { n: 0, min: Infinity, sum: 0, frames: this.win.frames };
    this.retarget();
  }

  // refillLevel is the level playback starts at after an underrun (or at the
  // start): Auto refills to the top of the band the level is kept in (the
  // trimming below leaves up to max(3 ms, a quarter of the target) above the
  // target), where continuous playback sits; a refill to the target itself
  // ran dry in the audio device's next render burst whenever the target was
  // about the burst's size (10 ms in Windows shared mode). Fixed: the target.
  refillLevel() {
    return this.auto ? this.target + Math.max(3 * this.ms, this.target / 4) : this.target;
  }

  // endWindow adapts the target to the window's level and returns how many
  // samples to drop now (the mean level's excess over the target).
  endWindow() {
    const w = this.win;
    const mean = w.sum / w.n;
    this.drops.push(mean - w.min);
    if (this.drops.length > HISTORY_WINDOWS) this.drops.shift();
    if (this.auto && this.sinceUnderrun >= sampleRate) {
      this.bias = Math.max(0, this.bias - BIAS_DECAY_MS * WINDOW);
    }
    this.retarget();
    this.win = { n: 0, min: Infinity, sum: 0, frames: 0 };
    const excess = mean - this.target;
    if (excess <= Math.max(3 * this.ms, this.target / 4)) return 0;
    return Math.min(Math.round(excess), Math.round(MAX_SKIP_MS * this.ms), Math.max(0, w.min - 128));
  }

  report(n) {
    this.sinceReport += n;
    if (this.sinceReport < sampleRate) return;
    this.sinceReport = 0;
    this.port.postMessage({
      t: 'jitter', auto: this.auto, targetMs: this.target / this.ms, levelMs: this.available() / this.ms,
      underruns: this.underruns, pauses: this.pauses, skippedMs: this.skippedMs, biasMs: this.bias,
    });
  }

  process(_inputs, outputs) {
    const out = outputs[0];
    const L = out[0];
    const R = out[1] || out[0];
    const n = L.length;
    this.report(n);
    const avail = this.available();
    if (avail > this.left) this.dry = 0;
    this.left = avail;
    if (this.buffering) {
      if (avail >= this.refillLevel()) this.buffering = false;
      else return true; // output silence while filling
    }
    if (avail < n) {
      this.underruns++;
      // The windows the level ran dry in, for a pause to forget: this one,
      // and the one before when the drain began in it.
      const drops = this.win.frames < this.dry ? this.drops.slice(0, -1) : this.drops.slice();
      this.undo = { bias: this.bias, since: this.sinceUnderrun, drops };
      this.sinceUnderrun = 0;
      if (this.auto) this.bias = Math.min(MAX_MS, this.bias + UNDERRUN_BIAS_MS);
      this.retarget();
      this.buffering = true;
      return true;
    }
    this.sinceUnderrun += n;
    this.dry += n;
    if (this.undo && this.sinceUnderrun > sampleRate / 4) this.undo = null;
    const high = this.target + Math.max(this.target, Math.round(sampleRate * 0.04));
    if (avail > high) {
      this.skip(avail - this.target);
      this.read(L, R, n);
      this.left = this.available();
      return true;
    }
    const w = this.win;
    w.n++;
    w.sum += avail;
    if (avail < w.min) w.min = avail;
    w.frames += n;
    const k = w.frames >= WINDOW * sampleRate ? this.endWindow() : 0;
    if (k > 0 && avail >= n + k) this.readSkipping(L, R, n, k);
    else this.read(L, R, n);
    this.left = this.available();
    return true;
  }
}

registerProcessor('recon-audio', ReconAudio);
