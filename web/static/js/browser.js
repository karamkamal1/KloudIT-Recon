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
// false) is listed in the overlay, not offered as a reason to update.

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
      'the 2D canvas cannot say whether it got low-latency (desynchronized) presentation, which Renderer Auto weighs', true);
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
 * The notice for an old Chromium-based browser, or for any browser with
 * gaps the tested Chromium does not have ('' when none is due): what it
 * lacks, and the way out (update it; another engine: Chrome or Edge).
 */
export function browserNotice(info, gaps) {
  const old = info.chromium > 0 && info.chromium < TESTED.major;
  const fix = gaps.filter((x) => x.tested);
  if (!old && !fix.length) return '';
  const head = old ? `${info.label} is ${TESTED.major - info.chromium} versions older than ${TESTED.name} ${TESTED.major}, which this client is tested with` : info.label;
  const lacks = fix.length ? `${old ? '; it' : ''} lacks ${fix.map((x) => x.name).join(', ')}` : '';
  const way = info.chromium > 0 ? `Update ${info.name === 'Browser' ? 'the browser' : info.name} for lower latency.`
    : `Chrome or Edge (${TESTED.name} ${TESTED.major} or newer) has ${fix.length > 1 ? 'them' : 'it'}.`;
  return `${head}${lacks}. ${way}`;
}
