// Low-latency audio playout. Samples arrive from the stream worker through a
// lock-free SharedArrayBuffer ring (or a MessagePort when the page is not
// cross-origin isolated). A small adaptive jitter buffer absorbs network jitter;
// if the buffer grows (clock drift, bursts) it is trimmed back to the target so
// audio never drifts behind video.

class ReconAudio extends AudioWorkletProcessor {
  constructor(options) {
    super();
    this.target = Math.round(sampleRate * ((options.processorOptions?.targetMs ?? 30) / 1000));
    this.ring = null;
    this.queue = [];
    this.queued = 0;
    this.buffering = true;
    this.underruns = 0;
    if (options.processorOptions?.sab) this.setRing(options.processorOptions.sab);
    this.port.onmessage = (ev) => {
      const m = ev.data;
      if (m && m.port) {
        m.port.onmessage = (e) => { this.queue.push(e.data); this.queued += e.data.length / 2; };
      } else if (m && m.sab) {
        this.setRing(m.sab);
      } else if (m && m.targetMs) {
        this.target = Math.round(sampleRate * (m.targetMs / 1000));
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

  process(_inputs, outputs) {
    const out = outputs[0];
    const L = out[0];
    const R = out[1] || out[0];
    const n = L.length;
    const avail = this.available();
    if (this.buffering) {
      if (avail >= this.target) this.buffering = false;
      else return true; // output silence while filling
    }
    if (avail < n) {
      this.underruns++;
      this.buffering = true;
      return true;
    }
    const high = this.target + Math.max(this.target, Math.round(sampleRate * 0.04));
    if (avail > high) this.skip(avail - this.target);
    this.read(L, R, n);
    return true;
  }
}

registerProcessor('recon-audio', ReconAudio);
