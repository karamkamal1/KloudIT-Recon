// This browser, and the features the client uses that it lacks (second
// hardware run: the owner streamed from Chrome 122, February 2024). Shown on
// the dashboard ("This browser"), on the stream page's splash and in its
// overlay, and reported to the host, which logs it (the input latency line).
//
// Detected from the APIs themselves, not from version numbers. TESTED is the
// Chromium the end-to-end test runs (test/e2e/browser.mjs, Playwright's
// build): a gap it does not have is one an update closes, verified (Chrome
// for Testing 122 against it; docs/VENDOR_NOTES.md, "Second hardware run:
// browser presentation and input latency"). A gap it has too (tested:
// false) is listed in the overlay, not offered as a reason to update. A gap
// is marked latency only where going without it costs latency by itself
// (TCP's head-of-line blocking, mouse motion held to the next frame), not
// where it only hides a report.

export const TESTED = { name: 'Chromium', major: 141 };

/** { name, major, chromium (the Chromium version, 0 if not Chromium), label } from the user agent. */
export function browserInfo(ua = globalThis.navigator?.userAgent || '') {
  const chromium = +(/(?:Chrome|Chromium)\/(\d+)/.exec(ua) || [])[1] || 0;
  for (const [name, re] of [['Edge', /Edg\/(\d+)/], ['Opera', /OPR\/(\d+)/], ['Firefox', /Firefox\/(\d+)/], ['Chrome', /(?:Headless)?Chrome\/(\d+)/], ['Safari', /Version\/(\d+).*Safari/]]) {
    const m = re.exec(ua);
    if (m) return { name, major: +m[1], chromium: name === 'Firefox' || name === 'Safari' ? 0 : chromium, label: `${name} ${m[1]}` };
  }
  return { name: 'Browser', major: 0, chromium, label: 'Browser' };
}

const proto = (g, iface, member) => typeof g[iface] === 'function' && member in g[iface].prototype;

/**
 * The features the client uses that this browser lacks: [{ id, name, why
 * (what the client cannot do without it), latency (it costs latency),
 * tested (the tested Chromium has it) }]. g: the global scope (a test
 * passes a stand-in).
 */
export function browserGaps(g = globalThis) {
  const gaps = [];
  const add = (id, name, why, latency, tested = true) => gaps.push({ id, name, why, latency, tested });
  if (typeof g.WebTransport === 'undefined') add('webtransport', 'WebTransport', 'WebSocket over TCP only, where one lost packet holds up everything behind it', true);
  if (typeof g.VideoDecoder === 'undefined') add('webcodecs', 'WebCodecs', 'no video', true);
  if (!('onpointerrawupdate' in g)) add('pointerrawupdate', 'raw mouse events (pointerrawupdate)', 'mouse motion waits for the next animation frame, up to a display refresh', true);
  if (typeof g.OffscreenCanvasRenderingContext2D === 'function' && !proto(g, 'OffscreenCanvasRenderingContext2D', 'getContextAttributes')) {
    add('canvas2d-attributes', "the 2D canvas's low-latency report (getContextAttributes in the worker)",
      'the 2D canvas cannot say whether it got low-latency (desynchronized) presentation; Renderer Auto ranks it after a context that reports it granted', false);
  }
  if (!g.navigator?.gpu) add('webgpu', 'WebGPU', 'no zero-copy renderer, no FSR upscaling, no HDR10', false);
  else if (typeof g.GPUCanvasContext === 'function' && !proto(g, 'GPUCanvasContext', 'getConfiguration')) {
    add('hdr-canvas', 'HDR canvas output (GPUCanvasContext.getConfiguration)', 'no HDR10 presentation', false);
  }
  if (!g.navigator?.keyboard?.lock) add('keyboard-lock', 'Keyboard Lock', 'Esc, Alt+Tab and the Windows key stay with this computer in fullscreen', false);
  if (typeof g.WebTransport !== 'undefined' && !proto(g, 'WebTransportSendStream', 'sendOrder')) {
    add('send-order', 'WebTransport sendOrder', 'input is not sent ahead of the other streams', true, false);
  }
  return gaps;
}

/**
 * The notice for this browser ({ text, warn }, or null when none is due):
 * for a Chromium-based browser older than the tested one that lacks what the
 * tested one has, what an update brings; for another engine that lacks it,
 * that Chrome or Edge has it. Lower latency is promised (and warn set) only
 * where a gap costs latency by itself. A current browser's own gaps (Keyboard
 * Lock on Android, what the tested Chromium lacks too) are no reason to
 * update: the overlay's Browser row lists them.
 */
export function browserNotice(info, gaps) {
  const fix = gaps.filter((x) => x.tested);
  const old = info.chromium > 0 && info.chromium < TESTED.major;
  if (!fix.length || (info.chromium > 0 && !old)) return null;
  const warn = fix.some((x) => x.latency);
  const lacks = `lacks ${fix.map((x) => x.name).join(', ')}`;
  const them = fix.length > 1 ? 'them' : 'it';
  if (!old) return { text: `${info.label} ${lacks}. Chrome or Edge (${TESTED.name} ${TESTED.major} or newer) has ${them}.`, warn };
  const k = TESTED.major - info.chromium;
  const name = info.name === 'Browser' ? 'the browser' : info.name;
  return {
    text: `${info.label} is ${k} version${k === 1 ? '' : 's'} older than ${TESTED.name} ${TESTED.major}, which this client is tested with; it ${lacks}. ` +
      (warn ? `Update ${name} for lower latency.` : `Updating ${name} brings ${them}.`),
    warn,
  };
}
