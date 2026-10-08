// Frame pacing (guide step 4.4): when the stream worker draws a decoded frame.
//
//   latency  "Lowest latency" (the default): draw on decode, one task after
//            the decoder's output; outputs already waiting by then supersede
//            each other and only the newest is drawn (step 4.1)
//   smooth   "Smooth": the frame waits for the next display refresh and is
//            drawn there, so the screen gets at most one new frame per
//            refresh, drawn as the refresh starts (an even cadence; no
//            mid-refresh update of a desynchronized canvas). Costs up to one
//            refresh of latency. At most one frame waits: a newer output
//            replaces it (the older one is dropped). A frame older than one
//            refresh when its refresh comes (the refresh came late or was
//            skipped) is dropped when a newer frame is already in the decoder
//            (that one takes the next refresh); otherwise it is drawn late, as
//            the newest picture there is (the last frame before a still
//            picture must not be lost), and never two drops in a row (a
//            refresh source that is always late must not starve the screen).
//
// Refresh ticks: the worker's requestAnimationFrame (its timestamp is the
// start of the refresh); without one in the worker, the main thread's
// animation frames (mainTicks(true): it posts their times); and a watchdog:
// a frame that waited watchdogMs() without a tick is handled from a timer
// (logged when such draws start), so a browser that never runs the callbacks
// still shows frames. It is long enough not to override the browser's own
// back-pressure in ordinary cases: a compositor or GPU that is behind delays
// the callbacks (the emulated GPU of the test machine while WebGPU presents:
// from 60 ms to seconds).
//
// The wait (decoder output -> the draw starts) is the "hold" stage of the
// per-stage latency (stream-worker.js), in both modes.

export const PACING = ['latency', 'smooth'];
export const PACING_LABELS = { latency: 'Lowest latency', smooth: 'Smooth' };

// "Older than one refresh": a frame that waited more than STALE_REFRESHES
// refreshes from its decode to the start of the refresh that would draw it.
// The quarter refresh of slack covers a frame decoded just as a refresh
// started (its tick request was too late for that one).
export const STALE_REFRESHES = 1.25;
export const WATCHDOG = { refreshes: 3, minMs: 100 };
export const watchdogMs = (refreshMs) => Math.max(WATCHDOG.minMs, WATCHDOG.refreshes * refreshMs);

// What a refresh starting at ts does with a frame decoded at `decoded`:
// 'draw'; 'stale' (older than one refresh, dropped: a newer frame is in the
// decoder and the previous refresh did not drop one already); 'late' (older
// than one refresh, drawn all the same).
export function paceFate(ts, decoded, refreshMs, newerComing, droppedLast = false) {
  if (ts - decoded <= STALE_REFRESHES * refreshMs) return 'draw';
  return newerComing && !droppedLast ? 'stale' : 'late';
}

// The pacer between the decoder's output and the renderer. o: draw(item)
// (item: { frame, meta, decoded }, plus via and tick set here), drop(item,
// why: 'superseded' | 'stale'), newerComing() (chunks in or in front of the
// decoder), refreshMs(), raf() (the worker's requestAnimationFrame, or null),
// mainTicks(on), now(), log(text); for tests also soon(fn) (one task later;
// default a MessageChannel) and timer(fn, ms) -> cancel (default setTimeout).
export class Pacer {
  constructor(o) {
    this.o = o;
    this.mode = 'latency';
    this.item = null; // the frame waiting to be drawn
    this.hop = false; // latency: the one-task hop is posted
    this.raf = 0; // smooth: the pending requestAnimationFrame's number (0: none)
    this.rafSeq = 0;
    this.main = false; // smooth: the main thread posts its animation frames
    this.cancelWatchdog = null;
    this.droppedLast = false;
    this.via = ''; // what started the last draw: hop | raf | main | timer
    this.counts = { hop: 0, raf: 0, main: 0, timer: 0, stale: 0, late: 0 };
    if (o.soon) {
      this.soon = () => o.soon(() => this.onHop());
    } else {
      const ch = new MessageChannel();
      ch.port1.onmessage = () => this.onHop();
      this.soon = () => ch.port2.postMessage(null);
    }
    this.timer = o.timer || ((fn, ms) => { const t = setTimeout(fn, ms); return () => clearTimeout(t); });
  }

  setMode(mode) {
    mode = PACING.includes(mode) ? mode : 'latency';
    if (mode === this.mode) return;
    this.mode = mode;
    this.droppedLast = false;
    if (mode === 'latency') {
      this.stopWatchdog();
      if (this.main) { this.main = false; this.o.mainTicks(false); }
    }
    if (this.item) this.schedule(); // the waiting frame goes the new way
  }

  // A decoded frame: it waits for its turn; one already waiting is dropped.
  offer(item) {
    const old = this.item;
    this.item = item;
    if (old) this.o.drop(old, 'superseded');
    this.schedule();
  }

  schedule() {
    if (this.mode === 'latency') {
      if (!this.hop) { this.hop = true; this.soon(); }
      return;
    }
    if (!this.cancelWatchdog) {
      this.cancelWatchdog = this.timer(() => {
        this.cancelWatchdog = null;
        this.raf = 0; // a request that never ran: the next frame asks again
        this.tick(this.o.now(), 'timer');
      }, watchdogMs(this.o.refreshMs()));
    }
    if (this.raf || this.main) return;
    const raf = this.o.raf();
    if (raf) {
      const n = ++this.rafSeq;
      this.raf = n;
      raf((ts) => {
        if (this.raf === n) this.raf = 0;
        this.tick(ts, 'raf');
      });
    } else {
      this.main = true;
      this.o.mainTicks(true);
    }
  }

  stopWatchdog() {
    if (this.cancelWatchdog) this.cancelWatchdog();
    this.cancelWatchdog = null;
  }

  onHop() {
    this.hop = false;
    if (this.mode !== 'latency' || !this.item) return; // switched to smooth meanwhile: setMode scheduled it
    const it = this.item;
    this.item = null;
    this.draw(it, 'hop', null);
  }

  // A display refresh started at ts (the worker's clock): the waiting frame
  // is drawn, or dropped when it is stale (paceFate).
  tick(ts, via) {
    if (this.mode !== 'smooth' || !this.item) return;
    this.stopWatchdog();
    const it = this.item;
    this.item = null;
    const fate = paceFate(ts, it.decoded, this.o.refreshMs(), this.o.newerComing(), this.droppedLast);
    this.droppedLast = fate === 'stale';
    if (fate === 'stale') {
      this.counts.stale++;
      this.o.drop(it, 'stale');
      return;
    }
    if (fate === 'late') this.counts.late++;
    if (via === 'timer' && this.via !== 'timer') { // once per run of watchdog draws
      this.o.log(`frame pacing: no display refresh within ${Math.round(watchdogMs(this.o.refreshMs()))} ms of a decoded frame; drawing from a timer`);
    }
    this.draw(it, via, ts);
  }

  draw(it, via, ts) {
    this.counts[via]++;
    this.via = via;
    it.via = via;
    it.tick = ts; // the refresh's start (smooth), null on decode
    this.o.draw(it);
  }

  // ticks: where Smooth's refresh ticks come from now (null in Lowest latency).
  info() {
    const ticks = this.mode !== 'smooth' ? null : this.main || !this.o.raf() ? 'main' : 'raf';
    return { mode: this.mode, via: this.via, ticks, counts: { ...this.counts } };
  }
}
