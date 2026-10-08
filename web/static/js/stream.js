// Stream page (main thread): UI, input capture and audio graph. Networking,
// decoding and rendering live in stream-worker.js.

import { api, me, el, toast, capabilities } from './api.js';
import * as P from './protocol.js';
import { codeToScancode } from './keymap.js';
import { PATHS, LABELS } from './renderers.js';
import { PACING, PACING_LABELS } from './pacing.js';
import { FSR, UPSCALE, UPSCALE_LABELS, upscaleSettings } from './fsr1.js';

const $ = (id) => document.getElementById(id);
const hostId = new URLSearchParams(location.search).get('host');

const ICONS = {
  mouseDesk: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M5 3l12 6-5 1.6L9.4 16z"/><path d="M12 10.6L18 18"/></svg>',
  mouseGame: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><circle cx="12" cy="12" r="7"/><path d="M12 2v5M12 17v5M2 12h5M17 12h5"/></svg>',
  fullscreen: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5"/></svg>',
  stats: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 19V10M10 19V5M16 19v-7M22 19H2"/></svg>',
  paste: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="7" width="18" height="12" rx="2"/><path d="M7 11h.01M11 11h.01M15 11h.01M7 15h10"/></svg>',
  settings: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12M20 18h0"/><circle cx="16" cy="6" r="2"/><circle cx="10" cy="12" r="2"/><circle cx="18" cy="18" r="2"/></svg>',
  disconnect: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M12 3v9"/><path d="M6.3 6.3a8 8 0 1 0 11.4 0"/></svg>',
};

// ---------------------------------------------------------------------------
// Preferences (persisted per browser)

const DEFAULTS = {
  codec: 'auto', bitrate: 30, fps: 60, resolution: 'native', quality: 'balanced', monitor: 0,
  audio: true, audioCodec: 'opus', volume: 100, jitterMs: 30,
  renderer: 'auto', pacing: 'latency', decoder: 'hardware', path: 'auto', transport: 'auto',
  upscale: 'auto', sharpness: FSR.sharpness, fsrDenoise: false,
  mouse: 'desktop', cursor: 'local', stats: false, adaptive: true, autoFullscreen: false, latencyProbe: false,
};
const PREF_KEY = 'recon.prefs.v1';
let prefs = { ...DEFAULTS };
try {
  const saved = JSON.parse(localStorage.getItem(PREF_KEY) || '{}');
  // Before step 4.3 every saved set held the then default renderer
  // "canvas2d"; that default is now "auto" (an explicit WebGPU choice stays).
  if (!saved.rendererV && saved.renderer === 'canvas2d') delete saved.renderer;
  Object.assign(prefs, saved, { rendererV: 2 });
} catch {}
if (prefs.renderer !== 'auto' && !PATHS.includes(prefs.renderer)) prefs.renderer = 'auto';
if (!PACING.includes(prefs.pacing)) prefs.pacing = 'latency';
if (!UPSCALE.includes(prefs.upscale)) prefs.upscale = 'auto';
prefs.sharpness = upscaleSettings({ sharpness: prefs.sharpness }).sharpness;
const savePrefs = () => { try { localStorage.setItem(PREF_KEY, JSON.stringify(prefs)); } catch {} };

const RESOLUTIONS = {
  native: [0, 0], '2160': [3840, 2160], '1440': [2560, 1440], '1080': [1920, 1080], '900': [1600, 900], '720': [1280, 720],
};

function hostPrefs() {
  let [w, h] = RESOLUTIONS[prefs.resolution] || [0, 0];
  if (prefs.resolution === 'client') {
    w = Math.round(screen.width * devicePixelRatio) & ~1;
    h = Math.round(screen.height * devicePixelRatio) & ~1;
  }
  return {
    codec: prefs.codec, bitrate: Math.round(prefs.bitrate * 1000), fps: +prefs.fps, width: w, height: h,
    monitor: +prefs.monitor, audio: !!prefs.audio, audioCodec: prefs.audioCodec, cursor: prefs.cursor, quality: prefs.quality,
    adaptive: prefs.adaptive !== false,
  };
}

// ---------------------------------------------------------------------------
// State

const S = {
  worker: null,
  surface: $('stage'), // input target (pointer lock, focus, events) holding the canvases
  canvases: {}, // presentation path -> its canvas (only the active one is shown)
  box: null, // the canvas box in device pixels
  present: null, // presentPlan() of this connection
  renderer: null, // the worker's active renderer (info)
  bakeoff: null, // this session's presentation bake-off result
  present2D: false, // Auto's path failed while drawing: the 2D canvas for this page
  connected: false,
  streaming: false,
  userClosed: false,
  attempts: 0,
  udpRelayFailedAt: -Infinity, // the UDP relay's ports did not answer: try the splice relay first for a while
  video: { w: 0, h: 0 },
  videoCfg: null,
  audioCfg: null,
  conn: null,
  welcome: null,
  keys: new Map(),
  buttons: new Set(),
  rel: { seq: 0, x: 0, y: 0, settle: [] },
  abs: { seq: 0, last: null, settle: 0 },
  wheel: { y: 0, x: 0 },
  cursor: { cache: new Map(), current: null, visible: true, pos: null },
  hz: 60,
  history: [],
  lastStats: null,
  logs: [],
};
const audio = { ctx: null, node: null, gain: null };

const post = (m, transfer) => S.worker?.postMessage(m, transfer || []);
const sendIn = (b) => post({ type: 'in', b });
const sendDg = (b) => post({ type: 'dg', b });
const sendCtl = (m) => post({ type: 'ctl', m });

// ---------------------------------------------------------------------------
// Splash / lifecycle

function splash(title, sub, { button = null, spinner = false } = {}) {
  $('splash').classList.remove('hidden');
  $('splash-title').textContent = title;
  $('splash-sub').textContent = sub;
  $('btn-start').classList.toggle('hidden', !button);
  if (button) $('btn-start').innerHTML = button;
  $('spinner').classList.toggle('hidden', !spinner);
}

// The display's refresh rate: the median interval of 30 animation frames, so
// frames the browser skips while the page loads do not count (frame pacing,
// step 4.4, takes its refresh interval from this).
async function measureHz() {
  return new Promise((res) => {
    const d = [];
    let last = 0;
    const tick = (t) => {
      if (last) d.push(t - last);
      last = t;
      if (d.length < 30) { requestAnimationFrame(tick); return; }
      d.sort((a, b) => a - b);
      res(Math.round(1000 / d[d.length >> 1]));
    };
    requestAnimationFrame(tick);
  });
}

function ensureAudioContext() {
  if (audio.ctx) {
    audio.ctx.resume().catch(() => {});
    return;
  }
  try {
    audio.ctx = new AudioContext({ latencyHint: 'interactive', sampleRate: 48000 });
  } catch {
    audio.ctx = new AudioContext({ latencyHint: 'interactive' });
  }
}

async function audioChannel() {
  const ctx = audio.ctx;
  if (!ctx) return {};
  if (!audio.node) {
    await ctx.audioWorklet.addModule('/js/audio-worklet.js');
    audio.node = new AudioWorkletNode(ctx, 'recon-audio', {
      numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [2], processorOptions: { targetMs: prefs.jitterMs },
    });
    audio.gain = ctx.createGain();
    audio.gain.gain.value = prefs.volume / 100;
    audio.node.connect(audio.gain).connect(ctx.destination);
  }
  if (self.crossOriginIsolated && typeof SharedArrayBuffer !== 'undefined') {
    const sab = new SharedArrayBuffer(8 + 48000 * 2 * 4);
    audio.node.port.postMessage({ sab });
    return { sab };
  }
  const ch = new MessageChannel();
  audio.node.port.postMessage({ port: ch.port1 }, [ch.port1]);
  return { port: ch.port2 };
}

// ---------------------------------------------------------------------------
// Presentation (guide step 4.3). Renderer "auto": the first connection
// without a stored result gives the worker a canvas per path and it runs the
// bake-off on the live stream (stream-worker.js; the pick is a heuristic,
// renderers.js pickPath: the 2D default unless another path is clearly
// better); the pick and the numbers are stored here (localStorage, per
// browser major version and OS: a browser update measures again). A path
// chosen in the settings is used as is.

const PRESENT_KEY = 'recon.present.v2';
try { localStorage.removeItem('recon.present.v1'); } catch {} // picked by a noisier rule

function deviceKey() {
  const ua = navigator.userAgent;
  let browser = 'Browser';
  for (const [name, re] of [['Edge', /Edg\/(\d+)/], ['Opera', /OPR\/(\d+)/], ['Firefox', /Firefox\/(\d+)/], ['Chrome', /Chrome\/(\d+)/], ['Safari', /Version\/(\d+).*Safari/]]) {
    const m = re.exec(ua);
    if (m) { browser = `${name} ${m[1]}`; break; }
  }
  const os = navigator.userAgentData?.platform || (/Windows|Android|iPhone|iPad|Mac OS X|CrOS|Linux/.exec(ua) || ['unknown OS'])[0];
  return `${browser} · ${os}`;
}

function storedPresent() {
  try {
    const s = JSON.parse(localStorage.getItem(PRESENT_KEY) || 'null');
    return s && s.key === deviceKey() && PATHS.includes(s.winner) ? s : null;
  } catch {
    return null;
  }
}

function storePresent(rec) {
  try {
    if (rec) localStorage.setItem(PRESENT_KEY, JSON.stringify(rec));
    else localStorage.removeItem(PRESENT_KEY);
  } catch {}
}

function presentPlan() {
  if (PATHS.includes(prefs.renderer)) return { mode: 'setting', paths: [prefs.renderer] };
  if (S.present2D) return { mode: 'auto', paths: ['canvas2d'] };
  const s = storedPresent();
  if (s) return { mode: 'auto', paths: [s.winner] };
  return { mode: 'bakeoff', paths: [...PATHS] };
}

// One canvas per path (a canvas is transferred to the worker once and keeps
// its context type), the first one shown. Returns them for the worker.
function stageCanvases(paths) {
  for (const c of Object.values(S.canvases)) c.remove();
  S.canvases = {};
  const out = {};
  for (const p of paths) {
    const c = el('canvas', { 'data-path': p });
    S.surface.insertBefore(c, $('remote-cursor'));
    S.canvases[p] = c;
    out[p] = c.transferControlToOffscreen();
  }
  showCanvas(paths[0]);
  return out;
}

function showCanvas(slot) {
  for (const [p, c] of Object.entries(S.canvases)) {
    c.hidden = p !== slot;
    if (p === slot) c.id = 'screen';
    else c.removeAttribute('id');
  }
}

function onRenderer(info) {
  S.renderer = info;
  if (S.conn) S.conn.renderer = info.name;
  showCanvas(info.slot);
  const hint = $('upscale-hint');
  if (hint) hint.textContent = upscaleHint();
  // The stored winner no longer works here: measure again next time.
  if (info.mode === 'auto' && info.name !== info.slot) storePresent(null);
}

function onCanvasGone(slot) {
  S.canvases[slot]?.remove();
  delete S.canvases[slot];
}

function onBakeoff(result) {
  S.bakeoff = result;
  streamHints();
  if (!result.winner) return;
  const v = S.videoCfg;
  storePresent({
    key: deviceKey(), winner: result.winner, why: result.why, at: new Date().toISOString(), results: result.results, pacing: result.pacing,
    video: v ? `${v.width}x${v.height} ${v.fps} fps ${v.codec}` : '', hz: S.hz, dpr: devicePixelRatio,
  });
  toast(`Renderer: ${LABELS[result.winner]}, Auto's pick (${result.why}). Settings → Pipeline.`, 'info', 4000);
}

// Auto's path failed draw after draw (the worker gave up on it): forget it
// and reconnect with the 2D canvas (a canvas keeps its context type).
function onPresentFailed(m) {
  storePresent(null);
  S.present2D = true;
  toast(`Renderer: ${LABELS[m.path] || m.path} stopped drawing (${m.reason}); reconnecting with the 2D canvas.`, 'warn', 5000);
  teardown();
  S.attempts = 0;
  connect();
}

// The canvas box in device pixels: the renderers size their canvas to it, so
// the compositor never scales the picture. devicePixelContentBoxSize where
// the browser has it (exact, snapped) and it agrees with the CSS size times
// devicePixelRatio within a pixel (DevTools device emulation reports CSS
// pixels there), else the CSS size times devicePixelRatio.
function stageBoxNow() {
  const r = S.surface.getBoundingClientRect();
  return { w: Math.round(r.width * devicePixelRatio), h: Math.round(r.height * devicePixelRatio) };
}

const stageObserver = new ResizeObserver((entries) => {
  const e = entries[entries.length - 1];
  const d = e.devicePixelContentBoxSize?.[0];
  const w = Math.round(e.contentRect.width * devicePixelRatio);
  const h = Math.round(e.contentRect.height * devicePixelRatio);
  S.box = d && Math.abs(d.inlineSize - w) <= 1 && Math.abs(d.blockSize - h) <= 1 ? { w: d.inlineSize, h: d.blockSize } : { w, h };
  post({ type: 'resize', ...S.box });
});
try {
  stageObserver.observe(S.surface, { box: 'device-pixel-content-box' });
} catch {
  stageObserver.observe(S.surface);
}

async function connect() {
  S.userClosed = false;
  splash('Connecting…', 'Requesting a secure session', { spinner: true });
  let ep;
  try {
    ep = await api('POST', `/api/hosts/${encodeURIComponent(hostId)}/connect`, {});
  } catch (e) {
    return onClosed(e.message, true);
  }
  $('host-name').textContent = ep.host;
  document.title = `${ep.host} · KloudIT Recon`;
  const { sab, port } = await audioChannel().catch(() => ({}));
  S.present = presentPlan();
  S.renderer = null;
  S.bakeoff = null;
  const canvases = stageCanvases(S.present.paths);
  const w = new Worker('/js/stream-worker.js', { type: 'module', name: 'recon-stream' });
  S.worker = w;
  // A worker being torn down (reconnect) still posts its last messages.
  w.onmessage = (ev) => { if (S.worker === w) onWorker(ev.data); };
  w.onerror = (e) => { if (S.worker === w) onClosed(`worker error: ${e.message}`, true); };
  const transfer = Object.values(canvases);
  if (port) transfer.push(port);
  w.postMessage({
    type: 'start', canvases, present: { mode: S.present.mode }, box: S.box || stageBoxNow(), endpoints: ep,
    prefs: {
      decoder: prefs.decoder, path: prefs.path, transport: prefs.transport, adaptive: prefs.adaptive, latencyProbe: !!prefs.latencyProbe, pacing: prefs.pacing,
      skipUdpRelay: performance.now() - S.udpRelayFailedAt < 10 * 60 * 1000,
      ...upscalePrefs(), fsrInput: prefs.fsrInput, // fsrInput: diagnostics only (localStorage), see fsr1.js FSR.input
    },
    hostPrefs: hostPrefs(),
    client: { ua: navigator.userAgent, w: Math.round(screen.width * devicePixelRatio), h: Math.round(screen.height * devicePixelRatio), dpr: devicePixelRatio, hz: S.hz },
    audioSab: sab, audioPort: port,
  }, transfer);
}

function teardown() {
  releaseAll();
  onTicks(false);
  if (S.worker) {
    const w = S.worker;
    w.postMessage({ type: 'close' });
    setTimeout(() => w.terminate(), 200);
    S.worker = null;
  }
  S.connected = false;
  S.streaming = false;
  if (document.pointerLockElement) document.exitPointerLock();
}

function onClosed(reason, retry) {
  const wasStreaming = S.streaming;
  teardown();
  if (S.userClosed) return;
  if (retry && S.attempts < 6) {
    S.attempts++;
    const delay = Math.min(8000, 800 * S.attempts);
    splash(wasStreaming ? 'Connection lost' : 'Could not connect', `${reason} — retrying in ${Math.round(delay / 1000)} s…`, { spinner: true });
    setTimeout(() => { if (!S.userClosed && !S.worker) connect(); }, delay);
    return;
  }
  splash('Disconnected', reason, { button: '↻&nbsp; Reconnect' });
}

function disconnect() {
  S.userClosed = true;
  teardown();
  location.href = '/';
}

// ---------------------------------------------------------------------------
// Worker messages

function onWorker(m) {
  switch (m.type) {
    case 'status': $('splash-sub').textContent = m.text; break;
    case 'log':
      console.log('[recon]', m.text);
      S.logs.push(`${new Date().toISOString()} ${m.text}`);
      if (S.logs.length > 200) S.logs.shift();
      break;
    case 'udpRelayFailed': S.udpRelayFailedAt = performance.now(); break;
    case 'connected':
      S.conn = m;
      S.connected = true;
      $('splash-sub').textContent = `Connected via ${m.transport === 'webtransport' ? 'WebTransport' : 'WebSocket'} (${m.path}) — waiting for the first frame…`;
      break;
    case 'welcome': S.welcome = m.info; buildDrawer(); break;
    case 'video': S.videoCfg = m.cfg; break;
    case 'rate':
      if (S.videoCfg?.gen === m.gen) S.videoCfg = { ...S.videoCfg, bitrate: m.bitrate, fps: m.fps || S.videoCfg.fps, maxBitrate: m.maxBitrate ?? S.videoCfg.maxBitrate };
      break;
    case 'audio': S.audioCfg = m.cfg; break;
    case 'resolution': S.video = { w: m.w, h: m.h }; break;
    case 'firstFrame':
      S.streaming = true;
      S.attempts = 0;
      $('splash').classList.add('hidden');
      S.surface.focus();
      // During the bake-off nothing covers the canvas (the toolbar has a
      // backdrop filter), so every path is measured alike: the start-up
      // toolbar and hint come with its result.
      if (S.present?.mode !== 'bakeoff') streamHints();
      break;
    case 'cursor': onCursorShape(m.shape); break;
    case 'cursorPos': onCursorPos(m); break;
    case 'notice': toast(m.msg, m.level === 'error' ? 'error' : m.level === 'warn' ? 'warn' : 'info', 6000); break;
    case 'stats': onStats(m); break;
    case 'renderer': onRenderer(m.info); break;
    case 'gone': onCanvasGone(m.slot); break;
    case 'bakeoff': onBakeoff(m.result); break;
    case 'presentFailed': onPresentFailed(m); break;
    case 'drawn': onDrawnMark(m); break;
    case 'ticks': onTicks(m.on); break;
    case 'stageDump': S.stageDump = m.recs; break;
    case 'dropTest': S.dropTest = m.result; break;
    case 'decoderTest': S.decoderTest = m.tests; S.decoderTestMs = m.ms; break;
    case 'hello': S.helloDecoders = m.decoders; break;
    case 'probeDump': for (const done of probeDumpWait.splice(0)) done(m); break;
    case 'rumble': rumble(m); break;
    case 'closed': onClosed(m.reason, m.retry); break;
  }
}

// Shown once the stream is up: the toolbar for a moment, the game-mode hint.
function streamHints() {
  showToolbar(3000);
  if (prefs.mouse === 'game') toast('Game mode: click the screen to capture the mouse (Esc releases it).', 'info', 5000);
}

// Display estimate for the worker's per-stage latency: the first animation
// frame that starts after a (sampled) draw. Times are absolute (timeOrigin +
// now) because the worker's performance clock has a different origin.
let drawnMark = null;
function onDrawnMark(m) {
  const waiting = drawnMark !== null;
  drawnMark = m; // a newer mark replaces one the (throttled) page never answered
  if (waiting) return;
  const tick = (ts) => {
    const abs = performance.timeOrigin + ts;
    if (abs < drawnMark.t) { requestAnimationFrame(tick); return; }
    post({ type: 'displayed', id: drawnMark.id, t: abs });
    drawnMark = null;
  };
  requestAnimationFrame(tick);
}

// Frame pacing "Smooth" in a browser whose workers have no
// requestAnimationFrame (pacing.js): the worker asks for this page's
// animation frames instead; each one's start goes to it as an absolute time.
// One loop at a time (gen: a loop of an earlier worker stops).
const ticks = { on: false, gen: 0 };
function onTicks(on) {
  if (!!on === ticks.on) return;
  ticks.on = !!on;
  const gen = ++ticks.gen;
  if (!on) return;
  const tick = (ts) => {
    if (ticks.gen !== gen) return;
    post({ type: 'tick', t: performance.timeOrigin + ts });
    requestAnimationFrame(tick);
  };
  requestAnimationFrame(tick);
}

// ---------------------------------------------------------------------------
// Keyboard

const isHotkey = (e) => e.ctrlKey && e.altKey && e.shiftKey;

function uiFocused(e) {
  const t = e.target;
  return t && (t.closest?.('.drawer') || t.closest?.('.modal-bg') || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName));
}

window.addEventListener('keydown', (e) => {
  if (!S.streaming || uiFocused(e)) return;
  if (isHotkey(e)) {
    const actions = { KeyM: toggleMouseMode, KeyF: toggleFullscreen, KeyS: toggleStats, KeyO: toggleDrawer, KeyQ: disconnect, KeyV: pasteDialog };
    if (actions[e.code]) {
      e.preventDefault();
      releaseAll();
      actions[e.code]();
      return;
    }
  }
  const m = codeToScancode(e.code);
  if (!m) return;
  e.preventDefault();
  e.stopPropagation();
  S.keys.set(e.code, m);
  sendIn(P.keyEvent(m[0], m[1], true));
}, true);

window.addEventListener('keyup', (e) => {
  if (!S.streaming) return;
  const m = S.keys.get(e.code) || codeToScancode(e.code);
  if (!m) return;
  if (!S.keys.has(e.code) && uiFocused(e)) return;
  e.preventDefault();
  S.keys.delete(e.code);
  sendIn(P.keyEvent(m[0], m[1], false));
}, true);

function releaseAll() {
  for (const [, m] of S.keys) sendIn(P.keyEvent(m[0], m[1], false));
  S.keys.clear();
  for (const b of S.buttons) sendIn(P.buttonEvent(b, false));
  S.buttons.clear();
  sendIn(P.releaseAllEvent());
}

window.addEventListener('blur', releaseAll);
document.addEventListener('visibilitychange', () => {
  if (!S.connected) return;
  if (document.hidden) {
    releaseAll();
    sendCtl({ t: 'pause' }); // stop encoding while nobody is watching
  } else {
    sendCtl({ t: 'resume' });
  }
});

// ---------------------------------------------------------------------------
// Mouse

function contentRect() {
  const r = S.surface.getBoundingClientRect();
  const vw = S.video.w || 16;
  const vh = S.video.h || 9;
  const scale = Math.min(r.width / vw, r.height / vh);
  const w = vw * scale;
  const h = vh * scale;
  return { x: r.left + (r.width - w) / 2, y: r.top + (r.height - h) / 2, w, h, scale };
}

function sendAbs(clientX, clientY) {
  const c = contentRect();
  const nx = Math.max(0, Math.min(1, (clientX - c.x) / c.w));
  const ny = Math.max(0, Math.min(1, (clientY - c.y) / c.h));
  const x = Math.round(nx * 65535);
  const y = Math.round(ny * 65535);
  if (S.abs.last && S.abs.last[0] === x && S.abs.last[1] === y) return;
  S.abs.last = [x, y];
  const pkt = P.mouseAbs(++S.abs.seq, x, y);
  sendDg(pkt);
  clearTimeout(S.abs.settle);
  S.abs.settle = setTimeout(() => sendDg(P.mouseAbs(++S.abs.seq, x, y)), 40); // loss insurance
}

function sendRel(dx, dy) {
  const r = S.rel;
  r.x += dx;
  r.y += dy;
  const cx = Math.round(r.x);
  const cy = Math.round(r.y);
  sendDg(P.mouseRel(++r.seq, cx, cy));
  // Totals are cumulative, so re-sending the latest one after motion stops
  // recovers any datagram that was lost.
  for (const t of r.settle) clearTimeout(t);
  r.settle = [40, 150].map((d) => setTimeout(() => sendDg(P.mouseRel(++r.seq, cx, cy)), d));
}

const locked = () => document.pointerLockElement === S.surface;

async function lockPointer() {
  try {
    await S.surface.requestPointerLock({ unadjustedMovement: true });
  } catch {
    try { await S.surface.requestPointerLock(); } catch {}
  }
}

// The toolbar shows when the pointer reaches the top edge (no element over
// the canvas for that: anything on top of it can cost the browser its
// direct presentation path).
let atTop = false;

const moveEvent = 'onpointerrawupdate' in window ? 'pointerrawupdate' : 'pointermove';
document.addEventListener(moveEvent, (e) => {
  const top = e.clientY < 6 && !locked();
  if (top && !atTop) showToolbar();
  atTop = top;
  if (!S.streaming) return;
  if (locked()) {
    if (e.movementX || e.movementY) sendRel(e.movementX, e.movementY);
    return;
  }
  if (prefs.mouse !== 'desktop') return;
  if (!S.surface.contains(e.target) && !S.buttons.size) return;
  sendAbs(e.clientX, e.clientY);
}, { passive: true });

// Input goes to the stage that holds the canvases (one per presentation path
// during the bake-off), so pointer lock and focus survive a renderer switch.
function bindSurface(c) {
  c.addEventListener('pointerdown', (e) => {
    if (!S.streaming) return;
    e.preventDefault();
    c.focus();
    if (prefs.mouse === 'game' && !locked()) {
      lockPointer();
      return;
    }
    if (!locked()) {
      c.setPointerCapture?.(e.pointerId);
      sendAbs(e.clientX, e.clientY);
    }
    if (e.button >= 0 && e.button <= 4 && !S.buttons.has(e.button)) {
      S.buttons.add(e.button);
      sendIn(P.buttonEvent(e.button, true));
    }
  });
  c.addEventListener('pointerup', (e) => {
    if (!S.buttons.has(e.button)) return;
    S.buttons.delete(e.button);
    sendIn(P.buttonEvent(e.button, false));
  });
  c.addEventListener('contextmenu', (e) => e.preventDefault());
  c.addEventListener('auxclick', (e) => e.preventDefault());
  c.addEventListener('wheel', (e) => {
    if (!S.streaming) return;
    e.preventDefault();
    const f = e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? 120 * 3 : 1.2;
    S.wheel.y += -e.deltaY * f;
    S.wheel.x += e.deltaX * f;
    const dy = Math.trunc(S.wheel.y);
    const dx = Math.trunc(S.wheel.x);
    if (dy || dx) {
      S.wheel.y -= dy;
      S.wheel.x -= dx;
      sendIn(P.wheelEvent(Math.max(-32768, Math.min(32767, dy)), Math.max(-32768, Math.min(32767, dx))));
    }
  }, { passive: false });
}

document.addEventListener('pointerlockchange', () => {
  for (const b of S.buttons) sendIn(P.buttonEvent(b, false));
  S.buttons.clear();
  $('remote-cursor').style.display = 'none';
  updateToolbarState();
  if (locked()) hideToolbar();
});

function toggleMouseMode() {
  prefs.mouse = prefs.mouse === 'game' ? 'desktop' : 'game';
  savePrefs();
  if (prefs.mouse === 'game') lockPointer();
  else if (locked()) document.exitPointerLock();
  toast(prefs.mouse === 'game' ? 'Game mode: raw relative mouse (click to capture)' : 'Desktop mode: absolute pointer with local cursor', 'info', 2500);
  updateToolbarState();
}

// ---------------------------------------------------------------------------
// Cursor rendered locally (zero-latency pointer)

function onCursorShape(shape) {
  const cur = S.cursor;
  if (shape.png && /^[A-Za-z0-9+/]+=*$/.test(shape.png) && shape.png.length < 400000) cur.cache.set(shape.id, { url: `data:image/png;base64,${shape.png}`, hotX: shape.hotX, hotY: shape.hotY, w: shape.w, h: shape.h });
  cur.visible = !shape.hidden;
  if (!shape.hidden) cur.current = cur.cache.get(shape.id) || cur.current;
  applyCursor();
}

function applyCursor() {
  const cur = S.cursor;
  const c = S.surface; // the canvases inherit the cursor
  if (prefs.cursor !== 'local' || !S.welcome?.features?.includes('cursor')) {
    c.style.cursor = prefs.cursor === 'video' ? 'none' : 'default';
    return;
  }
  if (!cur.visible) c.style.cursor = 'none';
  else if (cur.current) c.style.cursor = `url(${cur.current.url}) ${cur.current.hotX} ${cur.current.hotY}, default`;
  else c.style.cursor = 'default';
  const img = $('remote-cursor');
  if (cur.current) img.src = cur.current.url;
}

function onCursorPos(m) {
  // In game mode the OS cursor is hidden; if the game still shows a cursor
  // (menus), draw the host's cursor at the host position.
  const img = $('remote-cursor');
  const cur = S.cursor;
  if (!locked() || !m.visible || !cur.current || prefs.cursor !== 'local') {
    img.style.display = 'none';
    return;
  }
  const c = contentRect();
  const s = c.scale;
  img.style.display = 'block';
  img.style.width = `${cur.current.w * s}px`;
  img.style.height = `${cur.current.h * s}px`;
  img.style.left = `${c.x + (m.x / 65535) * c.w - cur.current.hotX * s}px`;
  img.style.top = `${c.y + (m.y / 65535) * c.h - cur.current.hotY * s}px`;
}

// ---------------------------------------------------------------------------
// Gamepads (polled at 250 Hz, sent as loss-tolerant full-state datagrams)

const pads = { timer: 0, state: [] };

function padLoop() {
  const gps = navigator.getGamepads ? navigator.getGamepads() : [];
  const t = performance.now();
  let any = false;
  for (let i = 0; i < 4; i++) {
    const gp = gps[i];
    const st = pads.state[i];
    if (gp && gp.connected) {
      any = true;
      const s = P.gamepadToXusb(gp);
      const key = `${s.buttons},${s.lt},${s.rt},${s.lx},${s.ly},${s.rx},${s.ry}`;
      if (!st || st.key !== key || t - st.sent > 100) {
        const seq = (st?.seq || 0) + 1;
        pads.state[i] = { key, sent: t, seq };
        if (S.streaming) sendDg(P.gamepadState(i, true, seq, s));
      }
    } else if (st) {
      if (S.streaming) sendDg(P.gamepadState(i, false, st.seq + 1, { buttons: 0, lt: 0, rt: 0, lx: 0, ly: 0, rx: 0, ry: 0 }));
      pads.state[i] = null;
    }
  }
  if (!any && !pads.state.some(Boolean)) {
    clearInterval(pads.timer);
    pads.timer = 0;
  }
}

window.addEventListener('gamepadconnected', (e) => {
  toast(`Controller connected: ${e.gamepad.id.slice(0, 48)}`, 'ok', 3000);
  if (!pads.timer) pads.timer = setInterval(padLoop, 4);
});

function rumble(m) {
  const gp = navigator.getGamepads?.()[m.idx];
  gp?.vibrationActuator?.playEffect?.('dual-rumble', { duration: 200, strongMagnitude: m.large / 255, weakMagnitude: m.small / 255 }).catch(() => {});
}

// ---------------------------------------------------------------------------
// Toolbar, fullscreen, stats

let toolbarTimer = 0;
function showToolbar(ms = 2500) {
  if (locked()) return;
  $('toolbar').classList.remove('hide');
  clearTimeout(toolbarTimer);
  toolbarTimer = setTimeout(() => {
    if (!$('toolbar').matches(':hover')) hideToolbar();
  }, ms);
}
function hideToolbar() { $('toolbar').classList.add('hide'); }
$('toolbar').addEventListener('pointerleave', () => showToolbar(1200));

function updateToolbarState() {
  $('btn-mouse').innerHTML = prefs.mouse === 'game' ? ICONS.mouseGame : ICONS.mouseDesk;
  $('btn-mouse').classList.toggle('on', prefs.mouse === 'game');
  $('btn-stats').classList.toggle('on', !!prefs.stats);
  $('btn-fullscreen').classList.toggle('on', !!document.fullscreenElement);
}

async function toggleFullscreen() {
  if (document.fullscreenElement) {
    navigator.keyboard?.unlock?.();
    await document.exitFullscreen().catch(() => {});
    return;
  }
  // Element fullscreen of the player (canvas stage + stream UI), browser UI hidden.
  await $('player').requestFullscreen({ navigationUI: 'hide' }).catch((e) => S.logs.push(`${new Date().toISOString()} fullscreen refused: ${e.message}`));
}

document.addEventListener('fullscreenchange', async () => {
  if (document.fullscreenElement) {
    // Keyboard lock: Esc, Alt+Tab, Win, Ctrl+W... go to the host (hold Esc to exit).
    await navigator.keyboard?.lock?.().catch(() => {});
    if (prefs.mouse === 'game') lockPointer();
  }
  updateToolbarState();
});

function toggleStats() {
  prefs.stats = !prefs.stats;
  savePrefs();
  $('stats').classList.toggle('hidden', !prefs.stats);
  updateToolbarState();
}

// present, submit and encode: frames of the native encoder helper only.
const STAGE_LABELS = [
  ['present', 'game present→capture'], ['capture', 'capture→encoded'], ['submit', '  capture→encoder'], ['encode', '  encode'],
  ['queue', 'host queue'], ['network', 'network'], ['transfer', 'transfer'],
  ['wait', 'reorder/wait'], ['decode', 'decode'], ['hold', 'hold (frame pacing)'], ['draw', 'draw'], ['display', '+ display (est.)'],
];
const fmt = (v, d = 1, unit = ' ms') => (v === null || v === undefined || !isFinite(v) ? '—' : `${v.toFixed(d)}${unit}`);
const cls = (v, a, b) => (v === null || v === undefined ? '' : v < a ? 'good' : v < b ? 'warn' : 'bad');

function onStats(st) {
  S.lastStats = st;
  S.probe = st.probe;
  const pill = $('latency');
  if (st.total !== null && st.total !== undefined) {
    pill.textContent = `${Math.round(st.total)} ms`;
    pill.className = `pill ${st.total < 25 ? '' : st.total < 50 ? 'mid' : 'high'}`;
    // The worker gives st.total in the span st.stages.from names: capture->draw
    // needs the host's capture stamps (frame-ext) on every frame of the stage
    // window; otherwise the number starts when the frame leaves the host.
    pill.title = st.stages?.from === 'capture'
      ? 'End-to-end latency (capture→draw): from capture on the host until the frame is drawn on this screen'
      : 'Stream latency (send→draw): from the frame leaving the host until it is drawn on this screen';
    S.history.push(st.total);
    if (S.history.length > 120) S.history.shift();
  }
  if (!prefs.stats) return;
  const v = S.videoCfg || {};
  const row = (k, val, c = '') => el('div', { class: 'row' }, el('span', { class: 'k' }, k), el('span', { class: `v ${c}` }, val));
  const spark = el('canvas', { class: 'spark', width: '240', height: '34' });
  const lat = st.stages;
  const pcts = (r) => (r ? `${fmt(r.p50, 1, '')} / ${fmt(r.p95, 1, '')} / ${fmt(r.p99, 1, '')}` : '—');
  const latencyRows = lat ? [
    row('Latency (ms)', 'p50 / p95 / p99'),
    row(lat.from === 'capture' ? 'End-to-end (capture→draw)' : 'Stream latency (send→draw)', pcts(lat.e2e), cls(lat.e2e?.p50, 25, 50)),
    ...STAGE_LABELS.filter(([k]) => lat.stages[k]).map(([k, label]) => row(`  ${label}`, pcts(lat.stages[k]))),
    row('  round trip (avg)', fmt(st.rtt, 1, ''), cls(st.rtt, 15, 40)),
  ] : [
    row('Stream latency (send→draw)', `${fmt(st.total)}  (${fmt(st.totalMin, 0, '')}–${fmt(st.totalMax, 0, '')})`, cls(st.total, 25, 50)),
    row('  round trip', fmt(st.rtt), cls(st.rtt, 15, 40)),
  ];
  $('stats-body').replaceChildren(...[
    ...latencyRows,
    ...probeRows(st.probe, row, pcts),
    spark,
    el('hr'),
    row('Frame rate', `${st.fps.toFixed(1)} fps`),
    row('Bitrate', `${st.mbps.toFixed(1)} Mbps`),
    targetRow(v, row),
    row('Video', `${S.video.w}×${S.video.h} ${v.family ? v.family.toUpperCase() : ''}${v.cropRight || v.cropBottom ? ` (coded ${v.codedWidth}×${v.codedHeight}, cropped)` : ''}`),
    row('Codec', `${v.codec || '—'} ${st.hw ? '(HW)' : '(SW)'}`),
    row('Encoder', `${v.encoder || '—'} · ${v.capture || ''}`),
    row('Loss recovery', recoveryText(v.recovery, st)),
    row('Transport', S.conn ? `${S.conn.transport} · ${S.conn.path}` : '—'),
    ...presentRows(st, row),
    ...upscaleRows(st.renderer, row),
    pacingRow(st.pacing, row),
    row('Audio', S.audioCfg?.enabled ? `${S.audioCfg.codec} · buf ${fmt(st.audioMs, 0)} · lost ${st.audioLost}` : 'off'),
    ...decoderRows(st, row),
    row('Frames dropped', `${st.dropped} (host dropped ${st.hostDropped}) · skipped ${st.skipped} · superseded ${st.superseded ?? 0} (+${st.supersededChunks ?? 0} undecoded) · key req ${st.keyRequests}`, st.dropped ? 'warn' : ''),
    st.recovered || st.recoveredByKey ? row('  recovered', `${st.recovered} by recovery frame · ${st.recoveredByKey} by key frame · ${st.recoveryDiscarded} frames waited out`) : null,
    row('Freezes > 100 ms', st.freezes ? `${st.freezes} (last ${fmt(st.lastFreeze, 0)})` : '0', st.freezes ? 'warn' : ''),
    st.synced ? null : row('Clock', 'syncing…', 'warn'),
  ].filter(Boolean));
  drawSpark(spark);
}

// What a lost frame costs in this generation (VideoConfig.recovery): reference
// recovery waits for the encoder's recovery frame (GUIDE 3.5), unless this
// browser's decoder rejected one (then key frames).
function recoveryText(mode, st) {
  switch (mode) {
    case 'skip': return 'skip frame (intra refresh)';
    case 'ltr':
    case 'invalidate': {
      const how = mode === 'ltr' ? 'LTR' : 'reference invalidation';
      return st.recoveryRejected ? `key frame (decoder rejected ${how} recovery)` : `recovery frame (${how})`;
    }
    default: return 'key frame';
  }
}

// The encoder's bitrate target: below the setting while a congestion
// back-off lasts, raised step by step as the network stays quiet (hosts
// before maxBitrate: the target only).
function targetRow(v, row) {
  if (!v.bitrate) return null;
  const mb = (kbps) => (kbps / 1000).toFixed(1);
  const backedOff = v.maxBitrate > v.bitrate;
  return row('  target', backedOff ? `${mb(v.bitrate)} of ${mb(v.maxBitrate)} Mbps (backed off)` : `${mb(v.bitrate)} Mbps`, backedOff ? 'warn' : '');
}

// Presentation (step 4.3): the active path, what its context reports
// (getContextAttributes().desynchronized) and the canvas size in device
// pixels, then the bake-off's draw and display stages per path and why Auto
// picked its path (this session's, or the result stored for this browser).
const desyncText = (r) => (r.desynchronized === true ? 'desynchronized ✓' : r.desynchronized === false ? 'desynchronized ✗ (not granted)'
  : r.name === 'webgpu' ? 'no low-latency mode' : 'desynchronized not reported');

function presentRows(st, row) {
  const r = st.renderer;
  if (!r) return [row('Renderer', S.conn ? S.conn.renderer : '—')];
  const b = r.bake;
  const how = r.mode === 'setting' ? 'setting' : b && !b.done ? `auto: measuring ${b.warming ? '(warm-up)' : `${b.slot}/${b.slots}`}` : 'auto';
  const rows = [
    row('Renderer', `${LABELS[r.name] || r.name}${r.name !== r.slot ? ` (${LABELS[r.slot] || r.slot} failed)` : ''} · ${how}`),
    row('  context', `${desyncText(r)} · canvas ${r.canvas[0]}×${r.canvas[1]}${r.drawErrors ? ` · ${r.drawErrors} errors` : ''}`, r.desynchronized === false || r.drawErrors ? 'warn' : ''),
  ];
  const res = b?.done ? b : storedPresent();
  if (res?.results) {
    rows.push(row(`  bake-off${res.at ? ` (${res.at.slice(0, 10)})` : ''}`, `draw p50/p95 · display p50 · fps${res.pacing ? ` (pacing: ${res.pacing})` : ''}`));
    for (const p of PATHS) {
      const x = res.results[p];
      if (!x) continue;
      const v = x.error ? 'unavailable' : `${x.draw.p50 ?? '—'}/${x.draw.p95 ?? '—'} · ${x.display.p50 ?? '—'} · ${x.fps ?? '—'}${x.out ? ` · ${x.out}` : ''}`;
      rows.push(row(`  ${p === res.winner ? '★ ' : ''}${LABELS[p]}`, v, p === res.winner ? 'good' : x.out ? 'warn' : ''));
    }
    if (res.why) rows.push(row('  pick', res.why));
  }
  return rows;
}

// Client-side upscaling (Phase 5, fsr1.js), with the WebGPU renderer (other
// paths: only when FSR 1 was chosen, as a warning; the setting's hint says
// FSR needs WebGPU): FSR 1 drawing (input -> output in device pixels, scale,
// sharpness) or why the bilinear path draws, and once FSR has drawn the GPU
// cost of the draw passes: by timestamp-query (FSR's passes, the copy among
// them, and the plain pass where it drew this session), else the draw stage
// with and without FSR (CPU side: encoding and submitting the passes).
function upscaleRows(r, row) {
  const u = r?.upscale;
  if (!u || (r.name !== 'webgpu' && u.mode !== 'fsr')) return [];
  const size = (a) => (a ? `${a[0]}×${a[1]}` : '—');
  const ms = (x, k = 'mean') => (x ? `${x[k].toFixed(2)} ms` : '—');
  const head = u.active
    ? `FSR 1 · ${size(u.in)} → ${size(u.out)} (${u.scale}×) · sharpness ${u.sharpness}${u.denoise ? ' · denoise' : ''}`
    : `bilinear${u.in ? ` · ${size(u.in)} → ${size(u.out)}` : ''}${u.why && u.why !== 'off' ? ` · ${u.why}` : ''}`;
  const rows = [row('Upscaling', `${UPSCALE_LABELS[u.mode] || u.mode}: ${head}`, u.mode === 'fsr' && !u.active && u.why !== 'not enlarged' ? 'warn' : '')];
  const g = u.gpu;
  if (g?.fsr) {
    rows.push(row('  GPU (timestamp-query, mean)', `FSR ${ms(g.fsr)}${g.copy ? ` (copy ${ms(g.copy)})` : ''} · plain ${ms(g.plain)} (n ${g.fsr.n})`));
  } else if (!g && u.cpu?.fsr) {
    const d = u.cpu.plain ? u.cpu.fsr.p50 - u.cpu.plain.p50 : null;
    const diff = d === null ? '' : ` · ${d >= 0 ? '+' : ''}${d.toFixed(2)} ms`;
    rows.push(row('  draw stage (CPU, p50)', `FSR ${ms(u.cpu.fsr, 'p50')} · plain ${ms(u.cpu.plain, 'p50')}${diff}`));
  }
  return rows;
}

// Frame pacing (step 4.4): the mode; for Smooth where its refresh ticks come
// from (the worker's requestAnimationFrame, else this page's), the refresh
// interval it works with (the one its ticks show), and this session's frames
// dropped stale (older than one refresh, a newer one on its way), drawn late,
// and drawn from the watchdog timer (no refresh tick came: warn).
const TICKS = { raf: 'worker rAF', main: 'page rAF' };
function pacingRow(pc, row) {
  if (!pc) return null;
  if (pc.mode !== 'smooth') return row('Frame pacing', `${PACING_LABELS.latency} · draw on decode`);
  const c = pc.counts;
  return row('Frame pacing', `${PACING_LABELS.smooth} · each refresh (${TICKS[pc.ticks] || pc.ticks}, ${pc.refreshMs} ms) · stale ${c.stale} · late ${c.late}` +
    `${c.timer ? ` · watchdog ${c.timer}` : ''}`, c.timer ? 'warn' : '');
}

// Decoder hygiene (step 4.1): the queue in the decoder (bound 2) and in front
// of it, the output lag (frames the decoder holds back on this stream), the
// VideoFrames open, and the startup self-test per codec family with how long
// it took in all (the hello waits for it: hygiene and step 4.2's timing).
function decoderRows(st, row) {
  const vf = st.videoFrames;
  return [
    row('Decoder queue', `${st.queue} (max ${st.queueMax ?? '—'}) · waiting ${st.waiting ?? 0} (max ${st.waitingMax ?? '—'})`, st.queueMax > 2 ? 'bad' : ''),
    row('Decoder output lag', st.outputLag === null || st.outputLag === undefined ? '—' : `${st.outputLag} frame${st.outputLag === 1 ? '' : 's'}`, st.outputLag > 0 ? 'warn' : ''),
    vf ? row('VideoFrames open', `${vf.open} (max ${vf.max})${vf.leaked ? ` · ${vf.leaked} leaked` : ''}`, vf.leaked ? 'bad' : '') : null,
    ...(S.decoderTest || []).map((t, i) => row(i ? '' : `Decoder self-test${S.decoderTestMs !== undefined ? ` (${S.decoderTestMs} ms)` : ''}`, t.text,
      t.software || (t.hw && !t.hw.ok) ? 'warn' : '')),
  ];
}

// Latency probe (frame barcode) rows: sample counts and capture->drawn
// measured from the picture (seq: test pattern; wallclock: host test page).
function probeRows(pr, row, pcts) {
  if (!pr || pr.mode === 'off') return [];
  const share = (n) => (pr.sampled ? `${Math.round((100 * n) / pr.sampled)}%` : '—');
  const label = pr.mode === 'seq' ? 'capture→drawn (barcode)' : 'host screen→drawn (barcode)';
  const bad = pr.mode === 'seq' ? `mismatched ${pr.mismatched}` : `implausible ${pr.implausible}`;
  return [
    el('hr'),
    row(`Frame barcode (${pr.mode})`, `${pr.sampled} sampled · valid ${share(pr.valid)} · ${bad}`, pr.sampled && pr.valid < 0.9 * pr.sampled ? 'warn' : ''),
    row(`  ${label}`, pr.latency ? `${pcts(pr.latency)} (n ${pr.latency.n})` : '—'),
    pr.pageToCapture ? row('  page→capture (stamps)', pcts(pr.pageToCapture)) : null,
  ];
}

// Everything the latency probe and the stage stats hold, as one JSON document.
const probeDumpWait = [];
function exportLatency() {
  return new Promise((resolve) => {
    if (!S.worker) { resolve(null); return; }
    const done = (m) => resolve(m);
    probeDumpWait.push(done);
    post({ type: 'probeDump' });
    setTimeout(() => {
      const i = probeDumpWait.indexOf(done);
      if (i >= 0) { probeDumpWait.splice(i, 1); resolve(null); }
    }, 2000);
  }).then((d) => d && {
    kind: 'kloudit-recon-latency', version: 1, exportedAt: new Date().toISOString(),
    host: S.welcome ? { name: S.welcome.host, os: S.welcome.os, version: S.welcome.version, features: S.welcome.features } : null,
    client: { ua: navigator.userAgent, hz: S.hz, dpr: devicePixelRatio, screen: [Math.round(screen.width * devicePixelRatio), Math.round(screen.height * devicePixelRatio)] },
    connection: S.conn, video: S.videoCfg, probe: d.probe, stages: d.stages,
    renderer: S.lastStats?.renderer || null, present: storedPresent(),
  });
}
S.exportLatency = exportLatency;

async function downloadLatency() {
  const data = await exportLatency();
  S.surface.focus();
  if (!data) { toast('No latency data yet.', 'warn'); return; }
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' });
  const name = `recon-latency-${(data.host?.name || 'host').replace(/[^\w.-]+/g, '_')}-${data.exportedAt.replace(/[:.]/g, '-')}.json`;
  const a = el('a', { href: URL.createObjectURL(blob), download: name });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 5000);
}

function drawSpark(c) {
  const ctx = c.getContext('2d');
  const h = S.history;
  if (h.length < 2) return;
  const max = Math.max(30, ...h);
  ctx.strokeStyle = '#38e1c6';
  ctx.lineWidth = 1.5;
  ctx.beginPath();
  h.forEach((v, i) => {
    const x = (i / (h.length - 1)) * c.width;
    const y = c.height - (v / max) * (c.height - 4) - 2;
    i ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
  });
  ctx.stroke();
}

// ---------------------------------------------------------------------------
// Paste text

function pasteDialog() {
  const ta = el('textarea', { placeholder: 'Text to type on the host (passwords, chat, commands)…', maxlength: '4000' });
  const close = () => bg.remove();
  const bg = el('div', { class: 'modal-bg', onclick: (e) => { if (e.target === bg) close(); } },
    el('div', { class: 'card modal' }, el('h3', {}, 'Type text on the host'),
      el('p', { class: 'hint' }, 'Sent as Unicode keystrokes into the focused window on the PC.'), ta,
      el('div', { class: 'modal-actions' },
        el('button', { onclick: async () => { try { ta.value = await navigator.clipboard.readText(); } catch { toast('Clipboard access denied', 'warn'); } } }, 'From clipboard'),
        el('button', { onclick: close }, 'Cancel'),
        el('button', { class: 'btn-primary', onclick: () => { if (ta.value) sendIn(P.textEvent(ta.value)); close(); S.surface.focus(); } }, 'Send'))));
  $('modal-root').append(bg);
  ta.focus();
}

// ---------------------------------------------------------------------------
// Settings drawer

function toggleDrawer() {
  const d = $('drawer');
  d.classList.toggle('open');
  if (d.classList.contains('open') && locked()) document.exitPointerLock();
  if (!d.classList.contains('open')) S.surface.focus();
}

function presentHint() {
  const s = storedPresent();
  if (!s) {
    return 'Auto tries each renderer on the live stream for about 10 s on the first connection: a desynchronized context first, the 2D canvas ' +
      'unless another draws clearly faster. A heuristic: the latency rig decides.';
  }
  return `Auto: ${LABELS[s.winner]}, picked ${s.at.slice(0, 10)}${s.why ? ` (${s.why})` : ''}.`;
}

function field(label, control, hint) {
  return el('div', {}, el('label', {}, label), control, hint ? el('div', { class: 'hint' }, hint) : null);
}

function select(key, options, onChange) {
  const s = el('select', {});
  for (const [v, l] of options) s.append(el('option', { value: v, selected: String(prefs[key]) === String(v) }, l));
  s.addEventListener('change', () => { prefs[key] = s.value; savePrefs(); onChange?.(); });
  return s;
}

function check(key, label, onChange) {
  const c = el('input', { type: 'checkbox', checked: !!prefs[key] });
  c.addEventListener('change', () => { prefs[key] = c.checked; savePrefs(); onChange?.(); });
  return el('label', { class: 'check' }, c, label);
}

let applyTimer = 0;
const applyLive = () => {
  clearTimeout(applyTimer);
  applyTimer = setTimeout(() => sendCtl({ t: 'settings', prefs: hostPrefs() }), 250);
};
const needsReconnect = () => toast('Applies on the next connection — click Reconnect.', 'info', 3500);
// Frame pacing (step 4.4) applies live in the worker (the host is not involved).
const applyPacing = () => post({ type: 'prefs', prefs: { pacing: prefs.pacing } });
// So does upscaling (Phase 5).
const upscalePrefs = () => ({ upscale: prefs.upscale, sharpness: prefs.sharpness, fsrDenoise: !!prefs.fsrDenoise });
const applyUpscale = () => post({ type: 'prefs', prefs: upscalePrefs() });

function upscaleHint() {
  const r = S.renderer?.name;
  const how = 'Applies at once. Auto: FSR 1 (AMD FidelityFX Super Resolution: edge-adaptive upsampling, then sharpening) when the picture is ' +
    `shown more than ${Math.round((FSR.autoMin - 1) * 100)} % larger than it streams (e.g. 1080p on a 4K screen), bilinear otherwise; ` +
    'never when it is shown at its size or smaller. The overlay shows the sizes and the GPU cost.';
  if (!r || r === 'webgpu') return how;
  // Renderer Auto never picks WebGPU where the 2D canvas is desynchronized
  // (renderers.js pickPath; WebGPU cannot report it): say so.
  const auto = prefs.renderer === 'auto' ? ' (Renderer Auto keeps the desynchronized 2D canvas wherever the browser has one, e.g. Chrome)' : '';
  return `FSR needs the WebGPU renderer: this connection draws with ${LABELS[r] || r} and scales bilinearly. Choose Renderer WebGPU above and Reconnect${auto}. ${how}`;
}

function buildDrawer() {
  const w = S.welcome || {};
  const caps = window.__caps || { codecs: {} };
  const codecName = { h264: 'H.264', hevc: 'HEVC / H.265', av1: 'AV1' };
  const hostFams = new Set((w.encoders || []).map((e) => (e.startsWith('h264') || e === 'libx264' ? 'h264' : e.startsWith('hevc') ? 'hevc' : 'av1')));
  const codecOpts = [['auto', 'Auto (best available)']];
  for (const f of ['h264', 'hevc', 'av1']) {
    if (hostFams.has(f) && caps.codecs[f]) codecOpts.push([f, `${codecName[f]} ${caps.codecs[f] === 'hw' ? '· HW decode' : '· SW decode'}`]);
  }
  const maxFps = w.maxFps || 240;
  const fpsOpts = [30, 60, 90, 120, 144, 165, 240].filter((f) => f <= maxFps).map((f) => [f, `${f} fps`]);
  const monOpts = (w.monitors || []).map((m) => [m.index, `${m.name || 'Display ' + (m.index + 1)} · ${m.w}×${m.h}${m.hz ? '@' + m.hz + 'Hz' : ''}${m.primary ? ' · primary' : ''}`]);

  const bitrate = el('input', { type: 'range', min: '2', max: String(Math.min(250, Math.round((w.maxKbps || 250000) / 1000))), step: '1', value: String(prefs.bitrate) });
  const out = el('output', {}, `${prefs.bitrate} Mbps`);
  bitrate.addEventListener('input', () => { out.textContent = `${bitrate.value} Mbps`; });
  bitrate.addEventListener('change', () => { prefs.bitrate = +bitrate.value; savePrefs(); applyLive(); });
  const vol = el('input', { type: 'range', min: '0', max: '150', value: String(prefs.volume) });
  vol.addEventListener('input', () => { prefs.volume = +vol.value; savePrefs(); if (audio.gain) audio.gain.gain.value = prefs.volume / 100; });
  const jitter = el('input', { type: 'range', min: '10', max: '120', step: '5', value: String(prefs.jitterMs) });
  const jout = el('output', {}, `${prefs.jitterMs} ms`);
  jitter.addEventListener('input', () => { jout.textContent = `${jitter.value} ms`; });
  jitter.addEventListener('change', () => { prefs.jitterMs = +jitter.value; savePrefs(); audio.node?.port.postMessage({ targetMs: prefs.jitterMs }); });
  const sharp = el('input', { type: 'range', min: '0', max: String(FSR.maxSharpness), step: '0.1', value: String(prefs.sharpness) });
  const sout = el('output', {}, `${prefs.sharpness} stops`);
  sharp.addEventListener('input', () => { sout.textContent = `${sharp.value} stops`; });
  sharp.addEventListener('change', () => { prefs.sharpness = +sharp.value; savePrefs(); applyUpscale(); });

  $('drawer').replaceChildren(
    el('h3', {}, 'Stream settings', el('button', { class: 'btn-icon btn-ghost', 'aria-label': 'Close', onclick: toggleDrawer }, '✕')),
    el('div', { class: 'group' }, el('div', { class: 'gtitle' }, 'Video'),
      field('Codec', select('codec', codecOpts, applyLive), 'Auto: HEVC with hardware at both ends, unless this browser decodes another codec clearly faster (timed while connecting). HEVC/AV1 give more quality per bit than H.264.'),
      field('Bitrate', el('div', { class: 'range-row' }, bitrate, out), 'LAN: 50–150 Mbps. Internet: match your upload speed.'),
      field('Frame rate', select('fps', fpsOpts, applyLive)),
      field('Resolution', select('resolution', [['native', 'Native (host display)'], ['client', 'Match this screen'], ['2160', '3840×2160'], ['1440', '2560×1440'], ['1080', '1920×1080'], ['900', '1600×900'], ['720', '1280×720']], applyLive), 'Downscaling happens on the GPU (Windows Graphics Capture).'),
      field('Encoder preset', select('quality', [['speed', 'Lowest latency'], ['balanced', 'Balanced'], ['quality', 'Best quality']], applyLive)),
      monOpts.length > 1 ? field('Display', select('monitor', monOpts, applyLive)) : null,
      check('adaptive', 'Adaptive bitrate on congestion', () => { post({ type: 'prefs', prefs: { adaptive: prefs.adaptive } }); applyLive(); }),
    ),
    el('div', { class: 'group' }, el('div', { class: 'gtitle' }, 'Input'),
      field('Mouse', select('mouse', [['desktop', 'Desktop — absolute, local cursor'], ['game', 'Game — raw relative (pointer lock)']], updateToolbarState)),
      field('Cursor', select('cursor', [['local', 'Local (zero latency)'], ['video', 'In the video stream']], () => { applyLive(); applyCursor(); })),
    ),
    el('div', { class: 'group' }, el('div', { class: 'gtitle' }, 'Audio'),
      check('audio', 'Stream PC audio', applyLive),
      field('Codec', select('audioCodec', [['opus', 'Opus (CELT low-delay)'], ['pcm', 'PCM (lossless, ~1.5 Mbps)']], applyLive)),
      field('Volume', vol),
      field('Jitter buffer', el('div', { class: 'range-row' }, jitter, jout), 'Lower = less delay, higher = fewer glitches on Wi-Fi.'),
    ),
    el('div', { class: 'group' }, el('div', { class: 'gtitle' }, 'Diagnostics'),
      check('latencyProbe', 'Latency probe (host test page)', () => post({ type: 'prefs', prefs: { latencyProbe: prefs.latencyProbe } })),
      el('div', { class: 'hint' }, 'Open tools/latency-test/index.html full-screen on the host PC: the overlay then shows host screen→drawn latency read from the picture. The test pattern source is probed automatically. Export from the overlay.'),
    ),
    el('div', { class: 'group' }, el('div', { class: 'gtitle' }, 'Pipeline'),
      field('Network path', select('path', [['auto', 'Auto (direct, then relay)'], ['direct', 'Direct to PC only'], ['relay', 'Relay via gateway']], needsReconnect)),
      field('Transport', select('transport', [['auto', 'WebTransport (QUIC), fall back to WebSocket'], ['websocket', 'WebSocket only']], needsReconnect)),
      field('Renderer', select('renderer', [['auto', 'Auto (measured in this browser)'], ['canvas2d', '2D canvas (desynchronized)'],
        ['webgl2', 'WebGL2 (desynchronized if granted)'], ['webgpu', 'WebGPU (zero-copy)']], needsReconnect), presentHint()),
      el('button', { class: 'btn-sm', onclick: () => { storePresent(null); toast('Auto measures the renderers again on the next connection.', 'info', 3500); } }, 'Measure renderers again'),
      field('Frame pacing', select('pacing', [['latency', 'Lowest latency (draw on decode)'], ['smooth', 'Smooth (one frame per display refresh)']], applyPacing),
        'Applies at once. Smooth holds each frame for the next display refresh: an even cadence for up to one refresh more latency (overlay: hold).'),
      el('div', {}, el('label', {}, 'Upscaling'),
        select('upscale', [['auto', 'Auto (FSR 1 when shown larger, WebGPU)'], ['off', 'Off (bilinear)'], ['fsr', 'FSR 1 (WebGPU)']], applyUpscale),
        el('div', { class: 'hint', id: 'upscale-hint' }, upscaleHint())),
      field('FSR sharpness', el('div', { class: 'range-row' }, sharp, sout), '0 = sharpest; each stop halves the sharpening (RCAS).'),
      check('fsrDenoise', 'FSR: sharpen noise less (RCAS denoise)', applyUpscale),
      field('Decoder', select('decoder', [['hardware', 'Prefer hardware'], ['software', 'Prefer software']], needsReconnect)),
    ),
    el('div', { class: 'actions' },
      el('button', { onclick: () => { teardown(); S.attempts = 0; connect(); toggleDrawer(); } }, '↻ Reconnect'),
      el('button', { onclick: () => sendCtl({ t: 'keyframe' }) }, 'Request key frame'),
      el('a', { class: 'btn', href: '/' }, 'Machines')),
  );
}

// ---------------------------------------------------------------------------
// Boot

async function boot() {
  for (const [id, icon] of [['btn-fullscreen', 'fullscreen'], ['btn-stats', 'stats'], ['btn-paste', 'paste'], ['btn-settings', 'settings'], ['btn-disconnect', 'disconnect']]) {
    $(id).innerHTML = ICONS[icon];
  }
  $('btn-mouse').onclick = toggleMouseMode;
  $('btn-fullscreen').onclick = toggleFullscreen;
  $('btn-stats').onclick = toggleStats;
  $('btn-paste').onclick = pasteDialog;
  $('btn-settings').onclick = toggleDrawer;
  $('btn-disconnect').onclick = disconnect;
  $('btn-export-latency').onclick = downloadLatency;
  $('stats').classList.toggle('hidden', !prefs.stats);
  updateToolbarState();
  bindSurface(S.surface);
  if (!hostId) {
    splash('No machine selected', 'Go back and pick a PC.', {});
    return;
  }
  await me();
  const [caps, hz] = await Promise.all([capabilities(), measureHz()]);
  window.__caps = caps;
  S.hz = hz;
  const box = $('splash-caps');
  const b = (ok, label) => el('span', { class: `badge ${ok ? 'good' : 'bad'}` }, `${ok ? '✓' : '✗'} ${label}`);
  box.append(b(caps.webtransport, 'WebTransport'), b(caps.webcodecs, 'WebCodecs'), b(caps.isolated, 'Lock-free audio'), b(caps.webgpu, 'WebGPU'), el('span', { class: 'badge' }, `${hz} Hz display`));
  for (const [f, v] of Object.entries(caps.codecs)) if (v) box.append(el('span', { class: `badge ${v === 'hw' ? 'good' : ''}` }, `${f.toUpperCase()} ${v.toUpperCase()}`));
  if (!caps.webcodecs) {
    splash('Browser not supported', 'This browser lacks WebCodecs. Use a current Chrome, Edge, Firefox or Safari.', {});
    return;
  }
  splash('Ready to stream', 'Audio and fullscreen need one click to start.', { button: '▶&nbsp; Start streaming' });
  if (new URLSearchParams(location.search).has('autostart')) start();
}

function start() {
  ensureAudioContext(); // must happen inside the user gesture
  if (prefs.autoFullscreen) toggleFullscreen();
  S.attempts = 0;
  connect();
}

$('btn-start').addEventListener('click', start);
window.addEventListener('beforeunload', () => { S.userClosed = true; teardown(); });
window.__recon = S; // exposed for diagnostics and automated tests

boot().catch((e) => splash('Error', e.message, {}));
