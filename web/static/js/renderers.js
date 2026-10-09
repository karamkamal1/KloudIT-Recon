// Presentation paths (guide step 4.3), used by the stream worker on the
// OffscreenCanvas the main thread hands it:
//
//   canvas2d  2D context with desynchronized: true (front-buffer rendering
//             where the browser supports it): drawImage(frame)
//   webgl2    WebGL2 context with desynchronized: true: texImage2D(frame)
//             into a texture, one triangle
//   webgpu    importExternalTexture(frame) (zero copy), one triangle; WebGPU
//             canvases have no low-latency (desynchronized) mode. A picture
//             shown larger than it streams can be upscaled with FSR 1 (EASU +
//             RCAS, fsr1.js; Phase 5) instead of the bilinear sampler
//
// Every renderer draws the part of the frame the video config calls visible
// (P.visibleArea, step 1.7) scaled to fit and centred ("letterbox") into a
// canvas whose backing store is the device-pixel size of its box on the page
// (resize(); until the main thread reports it, the video's visible size), so
// the compositor never scales the canvas. The renderer closes each frame once
// drawn, except the WebGPU renderer, which keeps exactly one (this.prev)
// until the next draw so the GPU never samples a closed frame (step 4.1).
//
// Interface: name (the path), desynchronized (what getContextAttributes()
// reports: true / false, or null when the context has no such attribute or
// mode), gpu (the adapter or GL renderer, '' if unknown), lost (the context
// or device is gone), canvasSize(), draw(frame, req, vis), resize(w, h),
// redraw() (after a resize, where the last picture is still at hand), idle()
// (another renderer takes over), destroy(), loseContext() (test hook: as if
// the GPU context were lost), setUpscale({ mode, sharpness, denoise, input })
// and upscaleInfo() (client-side upscaling, fsr1.js: only the WebGPU path
// runs FSR; the others scale bilinearly and say so), upscaled (whether FSR
// drew the last picture). draw() fills req (a latency probe sample,
// stream-worker.js) with either a clone of the frame (req.clone) or a
// promise of the barcode cells' mean luma read back from the GPU (req.luma);
// it throws when nothing could be drawn (the frame is closed then).
//
// HDR10 (step 4.5, hdr.js): only the WebGPU renderer draws HDR streams
// (hdrBlocked(format, visibleRect): null, or why a frame cannot take the HDR
// path): prepare(frame, inspect) copies the decoded planes into textures
// (resolving to the planes, which draw(frame, req, vis, planes) takes),
// setHdr({ want, why, white, space, peak }) says whether the canvas shows
// extended range or tone-maps, hdrInfo() describes it for the overlay. The
// other renderers never get an HDR stream (the client does not offer HDR
// with them, and withdraws the offer when one takes over).

import * as P from './protocol.js';
import { upscaleSettings, upscalePlan, upscaleWhy, easuConstants, rcasConstants, easuWGSL, RCAS_WGSL, COPY_WGSL } from './fsr1.js';
import { HDR_WHITE, CONVERT_WGSL, OUTPUT_WGSL, planeLayout, outputUniforms, halfToFloat } from './hdr.js';

export const PATHS = ['canvas2d', 'webgl2', 'webgpu'];
export const LABELS = { canvas2d: '2D canvas', webgl2: 'WebGL2', webgpu: 'WebGPU' };

// Auto's pick from a bake-off (stream-worker.js), per path: draw and display
// ({ p50, n }), drawRounds (draw p50 per round), fps, desynchronized, errors
// (failed draws), lost (context gone). A heuristic, not a measurement of
// presentation: the display stage is the main thread's next animation frame,
// which a worker's canvas does not go through (it is the same for every path
// unless one holds the page's frames back), and the draw stage is the
// worker's draw call. The latency rig (step 0.3) decides on real clients.
//
//   out    failed or skipped draws, a lost context, too few samples, fewer
//          than minShare of the best path's frames per second (from 10 fps
//          up), or a display p50 more than a refresh above the best
//   then   a context that reports desynchronized (front-buffer
//          presentation) before one that does not
//   then   the first in PATHS (the 2D default) unless another path's draw
//          p50 is lower by more than marginMs in every round (a near tie
//          keeps the default)
//
// Returns { winner (null: none measured), why, out: { path: reason } }.
export const PICK = { minDraw: 30, minDisplay: 8, minShare: 0.8, marginMs: 1 };

export function pickPath(results, refreshMs = 1000 / 60, rule = PICK) {
  const out = {};
  let pool = PATHS.filter((p) => results[p] && !results[p].error);
  const drop = (test) => { for (const p of pool) { const why = test(results[p]); if (why) out[p] = why; } pool = pool.filter((p) => !out[p]); };
  drop((x) => (x.lost ? 'context lost' : x.errors ? 'draw errors' : x.draw?.n >= rule.minDraw && x.display?.n >= rule.minDisplay ? '' : 'too few samples'));
  const fps = Math.max(0, ...pool.map((p) => results[p].fps));
  if (fps >= 10) drop((x) => (x.fps < rule.minShare * fps ? 'drops frames' : ''));
  const disp = Math.min(...pool.map((p) => results[p].display.p50));
  drop((x) => (x.display.p50 > disp + refreshMs ? 'display lags' : ''));
  if (!pool.length) return { winner: null, why: 'no path measured', out };
  const desync = pool.filter((p) => results[p].desynchronized === true);
  const byDesync = desync.length > 0 && desync.length < pool.length;
  if (byDesync) pool = desync;
  const def = pool[0];
  const d = results[def].drawRounds || [];
  const faster = (p) => {
    const r = results[p].drawRounds || [];
    return r.length > 0 && r.length === d.length && r.every((v, i) => v !== null && d[i] !== null && v + rule.marginMs < d[i]);
  };
  const clear = pool.filter((p) => p !== def && faster(p)).sort((a, b) => results[a].draw.p50 - results[b].draw.p50);
  if (clear.length) return { winner: clear[0], why: `draws over ${rule.marginMs} ms faster than ${LABELS[def]} in every round`, out };
  let why = pool.length > 1 ? `no other path draws over ${rule.marginMs} ms faster` : 'the only path left';
  if (byDesync) why = pool.length > 1 ? `desynchronized; ${why}` : 'the only desynchronized context';
  return { winner: def, why, out };
}

export function withTimeout(p, ms, what) {
  let t;
  return Promise.race([
    p.finally(() => clearTimeout(t)),
    new Promise((_, rej) => { t = setTimeout(() => rej(new Error(`${what} timed out`)), ms); }),
  ]);
}

/** Where a vw x vh picture goes in a W x H canvas: scaled to fit, centred (integer pixels). */
export function letterbox(W, H, vw, vh) {
  const s = Math.min(W / vw, H / vh);
  const w = Math.min(W, Math.max(1, Math.round(vw * s)));
  const h = Math.min(H, Math.max(1, Math.round(vh * s)));
  return { x: (W - w) >> 1, y: (H - h) >> 1, w, h };
}

/** The bars around a letterboxed picture r in a W x H canvas, [x, y, w, h] from the top left. */
export function barRects(W, H, r) {
  return [[0, 0, r.x, H], [r.x + r.w, 0, W - r.x - r.w, H], [r.x, 0, r.w, r.y], [r.x, r.y + r.h, r.w, H - r.y - r.h]]
    .filter(([, , w, h]) => w > 0 && h > 0);
}

const attr = (ctx, key) => {
  try {
    const a = ctx.getContextAttributes?.();
    return a && key in a ? !!a[key] : null;
  } catch {
    return null;
  }
};

const sameRect = (a, b) => !!a && !!b && a.x === b.x && a.y === b.y && a.w === b.w && a.h === b.h;

/** The last n values (a ring): mean, p50, p95 (ms). */
export class Samples {
  constructor(n) {
    this.v = new Float64Array(n);
    this.n = 0;
    this.i = 0;
  }
  push(x) {
    this.v[this.i] = x;
    this.i = (this.i + 1) % this.v.length;
    this.n = Math.min(this.n + 1, this.v.length);
  }
  summary() {
    if (!this.n) return null;
    const a = this.v.slice(0, this.n).sort();
    const q = (p) => +a[Math.min(a.length - 1, Math.floor(p * a.length))].toFixed(3);
    return { mean: +(a.reduce((s, x) => s + x, 0) / a.length).toFixed(3), p50: q(0.5), p95: q(0.95), n: a.length };
  }
}

class Renderer {
  constructor(c) {
    this.c = c;
    this.box = null; // [w, h]: device pixels of the canvas box (main thread), null: the video's size
    this.rect = null; // where the picture went last
    this.inW = 0; // the picture's size in its pixels (the visible area, rounded)
    this.inH = 0;
    this.desynchronized = null;
    this.gpu = '';
    this.lost = false;
    this.up = upscaleSettings(); // client-side upscaling setting (fsr1.js)
    this.upscaled = false; // FSR drew the last picture
  }
  setUpscale(o) {
    this.up = upscaleSettings(o);
  }
  // Upscaling state for the overlay: the setting, the last picture's size in
  // and out (device pixels), the scale factor and, when FSR did not draw it,
  // why (the bilinear path drew it). This path has no FSR.
  upscaleInfo() {
    const r = this.rect;
    if (!r || !this.inW) return { ...this.up, active: false, why: 'no picture yet' };
    const plan = upscalePlan(this.up.mode, this.inW, this.inH, r.w, r.h);
    return { ...this.up, active: this.upscaled, in: [this.inW, this.inH], out: [r.w, r.h], scale: +plan.scale.toFixed(3),
      why: this.upscaled ? '' : plan.active ? this.noFsr() : plan.why };
  }
  noFsr() {
    return 'FSR needs the WebGPU renderer';
  }
  resize(w, h) {
    this.box = w > 0 && h > 0 ? [Math.round(w), Math.round(h)] : null;
  }
  canvasSize() {
    return [this.c.width, this.c.height];
  }
  // Size the canvas for a picture of vis.w x vis.h; returns where the picture
  // goes, whether the canvas was resized (contents cleared) and whether the
  // picture moved (the bars changed).
  fit(vis) {
    const [W, H] = this.box || [Math.max(1, Math.round(vis.w)), Math.max(1, Math.round(vis.h))];
    let resized = false;
    if (this.c.width !== W || this.c.height !== H) {
      this.c.width = W;
      this.c.height = H;
      resized = true;
    }
    const r = letterbox(W, H, vis.w, vis.h);
    const moved = !sameRect(r, this.rect);
    this.rect = r;
    this.inW = Math.max(1, Math.round(vis.w));
    this.inH = Math.max(1, Math.round(vis.h));
    return { r, resized, moved };
  }
  redraw() {}
  idle() {}
  destroy() {}
  loseContext() {}
  // HDR10 (hdr.js): only the WebGPU renderer draws it.
  setHdr() {}
  hdrBlocked() {
    return { scope: 'renderer', why: `HDR needs the WebGPU renderer (${this.name} draws now)` };
  }
  hdrInfo() {
    return { path: null, why: 'HDR needs the WebGPU renderer' };
  }
}

// ---------------------------------------------------------------------------
// (A) 2D canvas, desynchronized

export class Canvas2DRenderer extends Renderer {
  static async create(c) { return new Canvas2DRenderer(c); }
  constructor(c) {
    super(c);
    this.ctx = c.getContext('2d', { alpha: false, desynchronized: true });
    if (!this.ctx) throw new Error('no 2D context');
    this.name = 'canvas2d';
    this.desynchronized = attr(this.ctx, 'desynchronized');
  }
  // req: latency probe sample; the corner is read back after the draw from a clone.
  // vis: the part of the frame to show (P.visibleArea).
  draw(frame, req, vis) {
    if (req) req.clone = frame.clone();
    const { r, resized, moved } = this.fit(vis);
    const W = this.c.width;
    const H = this.c.height;
    if (moved && !resized) {
      // A resized canvas is opaque black already; otherwise only the bars are
      // painted, never the picture's area (a front buffer may be on screen).
      this.ctx.fillStyle = '#000';
      for (const [x, y, w, h] of barRects(W, H, r)) this.ctx.fillRect(x, y, w, h);
    }
    if (r.x || r.y || r.w !== vis.w || r.h !== vis.h || vis.fx < 1 || vis.fy < 1) this.ctx.drawImage(frame, 0, 0, vis.w, vis.h, r.x, r.y, r.w, r.h);
    else this.ctx.drawImage(frame, 0, 0);
    frame.close();
  }
}

// ---------------------------------------------------------------------------
// (C) WebGL2, desynchronized requested: texImage2D(frame) + one triangle

const GL_ATTRS = { alpha: false, antialias: false, depth: false, stencil: false, desynchronized: true, preserveDrawingBuffer: false, powerPreference: 'high-performance' };

// One triangle over the viewport (the picture's letterbox rectangle). Texture
// coordinates span the visible part only (crop.xy, VideoConfig crop) and stop
// half a texel inside it (crop.zw), so a cropped edge never blends in padding.
// texImage2D puts the frame's top row at t = 0 (UNPACK_FLIP_Y_WEBGL false).
const GL_VS = `#version 300 es
uniform vec4 crop;
out vec2 uv;
void main() {
  vec2 p = vec2(gl_VertexID == 2 ? 3.0 : -1.0, gl_VertexID == 0 ? -3.0 : 1.0);
  gl_Position = vec4(p, 0.0, 1.0);
  uv = vec2((p.x + 1.0) * 0.5, (1.0 - p.y) * 0.5) * crop.xy;
}`;

const GL_FS = `#version 300 es
precision highp float;
uniform sampler2D tex;
uniform vec4 crop;
in vec2 uv;
out vec4 color;
void main() {
  color = vec4(texture(tex, min(uv, crop.zw)).rgb, 1.0);
}`;

// Latency probe on the WebGL2 path: one texel per barcode cell, the mean of
// 4x4 samples over the cell's inner half of the texture the frame was drawn
// from (like the WebGPU probe); readPixels into a pixel buffer, a fence, and
// getBufferSubData once the fence has passed: no pipeline stall.
const GL_PROBE_FS = `#version 300 es
precision highp float;
uniform sampler2D tex;
uniform vec2 cell;
out vec4 color;
void main() {
  vec2 c = floor(gl_FragCoord.xy);
  vec3 acc = vec3(0.0);
  for (int j = 0; j < 4; j++) {
    for (int i = 0; i < 4; i++) {
      vec2 f = vec2(0.3125 + float(i) * 0.125, 0.3125 + float(j) * 0.125);
      acc += texture(tex, (c + f) * cell).rgb;
    }
  }
  color = vec4(acc / 16.0, 1.0);
}`;

function glProgram(gl, vs, fs) {
  const prog = gl.createProgram();
  for (const [type, src] of [[gl.VERTEX_SHADER, vs], [gl.FRAGMENT_SHADER, fs]]) {
    const s = gl.createShader(type);
    gl.shaderSource(s, src);
    gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) throw new Error(`shader: ${gl.getShaderInfoLog(s)}`);
    gl.attachShader(prog, s);
  }
  gl.linkProgram(prog);
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) throw new Error(`program: ${gl.getProgramInfoLog(prog)}`);
  return prog;
}

function glTexture(gl) {
  const t = gl.createTexture();
  gl.bindTexture(gl.TEXTURE_2D, t);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE);
  gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE);
  return t;
}

function glRendererName(gl) {
  try {
    const ext = gl.getExtension('WEBGL_debug_renderer_info');
    return String(gl.getParameter(ext ? ext.UNMASKED_RENDERER_WEBGL : gl.RENDERER) || '');
  } catch {
    return '';
  }
}

export class WebGL2Renderer extends Renderer {
  // The program, a VideoFrame upload and its orientation on a scratch canvas
  // first: a canvas cannot switch context types, so the real one is only
  // claimed once WebGL2 demonstrably draws frames the right way up.
  static selfTest() {
    const c = new OffscreenCanvas(4, 4);
    const gl = c.getContext('webgl2', GL_ATTRS);
    if (!gl) throw new Error('WebGL2 unavailable');
    try {
      const prog = glProgram(gl, GL_VS, GL_FS);
      const src = new OffscreenCanvas(4, 4);
      const g = src.getContext('2d');
      g.fillStyle = '#f00';
      g.fillRect(0, 0, 4, 2);
      g.fillStyle = '#00f';
      g.fillRect(0, 2, 4, 2);
      const vf = new VideoFrame(src, { timestamp: 0 });
      try {
        glTexture(gl);
        gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, vf);
      } finally {
        vf.close();
      }
      gl.viewport(0, 0, 4, 4);
      gl.useProgram(prog);
      gl.uniform4f(gl.getUniformLocation(prog, 'crop'), 1, 1, 1, 1);
      gl.drawArrays(gl.TRIANGLES, 0, 3);
      const px = new Uint8Array(64);
      gl.readPixels(0, 0, 4, 4, gl.RGBA, gl.UNSIGNED_BYTE, px);
      const err = gl.getError();
      // readPixels starts at the bottom row: blue there, red at the top.
      const bottom = px.subarray(0, 4);
      const top = px.subarray(48, 52);
      if (err || !(top[0] > 200 && top[2] < 60 && bottom[2] > 200 && bottom[0] < 60)) {
        throw new Error(`WebGL2 self-test: ${err ? `GL error 0x${err.toString(16)}` : `frame drawn wrong (top ${[...top]}, bottom ${[...bottom]})`}`);
      }
    } finally {
      gl.getExtension('WEBGL_lose_context')?.loseContext();
    }
  }

  static async create(c, { log = () => {} } = {}) {
    WebGL2Renderer.selfTest();
    const gl = c.getContext('webgl2', GL_ATTRS);
    if (!gl) throw new Error('WebGL2 context refused');
    return new WebGL2Renderer(c, gl, log);
  }

  constructor(c, gl, log) {
    super(c);
    this.gl = gl;
    this.log = log;
    this.name = 'webgl2';
    this.desynchronized = attr(gl, 'desynchronized');
    this.gpu = glRendererName(gl);
    this.failed = ''; // the decoder's frames do not upload: every draw throws
    c.addEventListener?.('webglcontextlost', (e) => { e.preventDefault(); this.lost = true; log('WebGL2 context lost'); });
    c.addEventListener?.('webglcontextrestored', () => {
      try { this.init(); this.lost = false; log('WebGL2 context restored'); } catch (e) { log(`WebGL2 context not restored: ${e.message}`); }
    });
    this.init();
  }

  init() {
    const gl = this.gl;
    this.prog = glProgram(gl, GL_VS, GL_FS);
    this.cropLoc = gl.getUniformLocation(this.prog, 'crop');
    gl.useProgram(this.prog);
    gl.uniform1i(gl.getUniformLocation(this.prog, 'tex'), 0);
    gl.activeTexture(gl.TEXTURE0);
    this.tex = glTexture(gl);
    gl.clearColor(0, 0, 0, 1);
    this.last = null; // the frame in the texture: { vis, fw, fh } (redraw after a resize)
    this.checked = false;
    this.probeBufs = [];
    try {
      this.probeProg = glProgram(gl, GL_VS, GL_PROBE_FS);
      this.cellLoc = gl.getUniformLocation(this.probeProg, 'cell');
      gl.useProgram(this.probeProg);
      gl.uniform1i(gl.getUniformLocation(this.probeProg, 'tex'), 0);
      this.probeTex = gl.createTexture();
      gl.bindTexture(gl.TEXTURE_2D, this.probeTex);
      gl.texStorage2D(gl.TEXTURE_2D, 1, gl.RGBA8, P.BARCODE_COLS, P.BARCODE_ROWS);
      this.probeFbo = gl.createFramebuffer();
      gl.bindFramebuffer(gl.FRAMEBUFFER, this.probeFbo);
      gl.framebufferTexture2D(gl.FRAMEBUFFER, gl.COLOR_ATTACHMENT0, gl.TEXTURE_2D, this.probeTex, 0);
      const status = gl.checkFramebufferStatus(gl.FRAMEBUFFER);
      gl.bindFramebuffer(gl.FRAMEBUFFER, null);
      if (status !== gl.FRAMEBUFFER_COMPLETE) throw new Error(`framebuffer status 0x${status.toString(16)}`);
      // Two readbacks in flight at most (probe.inflight in the worker).
      this.probeBufs = [0, 1].map(() => {
        const b = gl.createBuffer();
        gl.bindBuffer(gl.PIXEL_PACK_BUFFER, b);
        gl.bufferData(gl.PIXEL_PACK_BUFFER, P.BARCODE_BITS * 4, gl.STREAM_READ);
        return b;
      });
      gl.bindBuffer(gl.PIXEL_PACK_BUFFER, null);
    } catch (e) {
      this.probeProg = null; // the probe falls back to a frame copy
      this.log(`WebGL2 latency probe unavailable (${e.message})`);
    }
    gl.bindTexture(gl.TEXTURE_2D, this.tex);
  }

  draw(frame, req, vis) {
    const gl = this.gl;
    if (this.lost || this.failed) {
      frame.close();
      throw new Error(this.failed || 'WebGL2 context lost');
    }
    const probe = req && this.probeProg && this.probeBufs.length;
    if (req && !probe) req.clone = frame.clone();
    const fw = frame.displayWidth;
    const fh = frame.displayHeight;
    try {
      gl.bindTexture(gl.TEXTURE_2D, this.tex);
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.RGBA, gl.RGBA, gl.UNSIGNED_BYTE, frame);
      if (!this.checked) {
        // Once per renderer (getError waits for the GPU process): the upload
        // path works for the decoder's frames (hardware frames may differ
        // from the self-test's). If not, the texture stays empty (black):
        // every later draw fails too.
        this.checked = true;
        const err = gl.getError();
        if (err) {
          this.failed = `WebGL2 error 0x${err.toString(16)} uploading a ${frame.format || 'opaque'} frame`;
          throw new Error(this.failed);
        }
      }
    } finally {
      frame.close(); // the texture holds the picture now
    }
    this.last = { vis, fw, fh };
    this.present(vis, fw, fh);
    if (probe) req.luma = this.probeRead(req, fw, fh);
  }

  present(vis, fw, fh) {
    const gl = this.gl;
    const { r } = this.fit(vis);
    const W = this.c.width;
    const H = this.c.height;
    // The bars (bottom-left origin here); the picture's area is never
    // cleared first (on a front buffer the clear could reach the screen).
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    const bars = barRects(W, H, r);
    if (bars.length) {
      gl.enable(gl.SCISSOR_TEST);
      for (const [x, y, w, h] of bars) {
        gl.scissor(x, H - y - h, w, h);
        gl.clear(gl.COLOR_BUFFER_BIT);
      }
      gl.disable(gl.SCISSOR_TEST);
    }
    gl.viewport(r.x, H - r.y - r.h, r.w, r.h);
    gl.useProgram(this.prog);
    gl.uniform4f(this.cropLoc, vis.fx, vis.fy, vis.fx - 0.5 / fw, vis.fy - 0.5 / fh);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  }

  redraw() {
    if (this.last && !this.lost) this.present(this.last.vis, this.last.fw, this.last.fh);
  }

  async probeRead(req, fw, fh) {
    const gl = this.gl;
    const buf = this.probeBufs.pop();
    gl.bindFramebuffer(gl.FRAMEBUFFER, this.probeFbo);
    gl.viewport(0, 0, P.BARCODE_COLS, P.BARCODE_ROWS);
    gl.useProgram(this.probeProg);
    gl.uniform2f(this.cellLoc, req.cell / fw, req.cell / fh);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
    gl.bindBuffer(gl.PIXEL_PACK_BUFFER, buf);
    gl.readPixels(0, 0, P.BARCODE_COLS, P.BARCODE_ROWS, gl.RGBA, gl.UNSIGNED_BYTE, 0);
    gl.bindBuffer(gl.PIXEL_PACK_BUFFER, null);
    gl.bindFramebuffer(gl.FRAMEBUFFER, null);
    const sync = gl.fenceSync(gl.SYNC_GPU_COMMANDS_COMPLETE, 0);
    gl.flush();
    try {
      // The fence's status only changes between tasks: poll on timers.
      for (let i = 0; ; i++) {
        if (gl.isContextLost()) throw new Error('WebGL2 context lost');
        const s = gl.clientWaitSync(sync, 0, 0);
        if (s === gl.ALREADY_SIGNALED || s === gl.CONDITION_SATISFIED) break;
        if (s === gl.WAIT_FAILED || i > 250) throw new Error('readback fence did not pass');
        await new Promise((res) => setTimeout(res, 4));
      }
      const px = new Uint8Array(P.BARCODE_BITS * 4);
      gl.bindBuffer(gl.PIXEL_PACK_BUFFER, buf);
      gl.getBufferSubData(gl.PIXEL_PACK_BUFFER, 0, px);
      gl.bindBuffer(gl.PIXEL_PACK_BUFFER, null);
      const luma = [];
      for (let k = 0; k < P.BARCODE_BITS; k++) luma.push(0.299 * px[4 * k] + 0.587 * px[4 * k + 1] + 0.114 * px[4 * k + 2]);
      return luma;
    } finally {
      gl.deleteSync(sync);
      this.probeBufs.push(buf);
    }
  }

  destroy() {
    this.gl.getExtension('WEBGL_lose_context')?.loseContext();
  }

  loseContext() {
    this.gl.getExtension('WEBGL_lose_context')?.loseContext();
  }
}

// ---------------------------------------------------------------------------
// (B) WebGPU, importExternalTexture

const WGSL = `
@group(0) @binding(0) var samp: sampler;
@group(0) @binding(1) var tex: texture_external;
@group(0) @binding(2) var<uniform> crop: vec4f; // xy: visible share of the frame (VideoConfig crop); zw: largest coordinate
struct VOut { @builtin(position) pos: vec4f, @location(0) uv: vec2f };
@vertex fn vs(@builtin(vertex_index) i: u32) -> VOut {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  var o: VOut;
  o.pos = vec4f(p[i], 0.0, 1.0);
  o.uv = vec2f((p[i].x + 1.0) * 0.5, (1.0 - p[i].y) * 0.5) * crop.xy;
  return o;
}
@fragment fn fs(v: VOut) -> @location(0) vec4f {
  return textureSampleBaseClampToEdge(tex, samp, min(v.uv, crop.zw));
}`;

// Latency probe on the WebGPU path: one texel per barcode cell, the mean of
// 4x4 samples over the cell's inner half, rendered from the external texture
// the frame is drawn from; copyTextureToBuffer + mapAsync read the 8x3 texels
// back without a frame readback or a pipeline stall.
const PROBE_WGSL = `
@group(0) @binding(0) var samp: sampler;
@group(0) @binding(1) var tex: texture_external;
@group(0) @binding(2) var<uniform> cell: vec4f; // xy: cell size in texture coordinates
@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  return vec4f(p[i], 0.0, 1.0);
}
@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  let c = floor(pos.xy);
  var acc = vec3f(0.0);
  for (var j = 0; j < 4; j++) {
    for (var i = 0; i < 4; i++) {
      let f = vec2f(0.3125 + f32(i) * 0.125, 0.3125 + f32(j) * 0.125);
      acc += textureSampleBaseClampToEdge(tex, samp, (c + f) * cell.xy).rgb;
    }
  }
  return vec4f(acc / 16.0, 1.0);
}`;

export class WebGPURenderer extends Renderer {
  // Runs the full import -> draw -> submit path on a scratch canvas first: a
  // canvas cannot switch context types, so the real one is only claimed once
  // WebGPU demonstrably works on this device/driver.
  static async selfTest(device, format, pipelineFor) {
    const scratch = new OffscreenCanvas(16, 16);
    const ctx = scratch.getContext('webgpu');
    ctx.configure({ device, format, alphaMode: 'opaque' });
    const src = new OffscreenCanvas(16, 16);
    src.getContext('2d').fillRect(0, 0, 16, 16);
    const vf = new VideoFrame(src, { timestamp: 0 });
    const crop = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
    try {
      device.pushErrorScope('validation');
      const { pipeline, sampler } = pipelineFor;
      const ext = device.importExternalTexture({ source: vf });
      device.queue.writeBuffer(crop, 0, new Float32Array([1, 1, 1, 1]));
      const bg = device.createBindGroup({
        layout: pipeline.getBindGroupLayout(0),
        entries: [{ binding: 0, resource: sampler }, { binding: 1, resource: ext }, { binding: 2, resource: { buffer: crop } }],
      });
      const enc = device.createCommandEncoder();
      const pass = enc.beginRenderPass({ colorAttachments: [{ view: ctx.getCurrentTexture().createView(), loadOp: 'clear', storeOp: 'store' }] });
      pass.setPipeline(pipeline);
      pass.setBindGroup(0, bg);
      pass.draw(3);
      pass.end();
      device.queue.submit([enc.finish()]);
      await withTimeout(device.queue.onSubmittedWorkDone(), 1500, 'WebGPU self-test');
      const err = await device.popErrorScope();
      if (err) throw new Error(err.message);
    } finally {
      vf.close();
      crop.destroy();
    }
  }

  static async create(c, { log = () => {} } = {}) {
    if (!self.navigator.gpu) throw new Error('WebGPU unavailable');
    const adapter = await navigator.gpu.requestAdapter({ powerPreference: 'high-performance' });
    if (!adapter) throw new Error('no WebGPU adapter');
    // timestamp-query, where the adapter has it: the GPU cost of the draw
    // passes (FSR upscaling vs the plain path) for the overlay.
    const device = await adapter.requestDevice({ requiredFeatures: adapter.features.has('timestamp-query') ? ['timestamp-query'] : [] });
    const formatProbe = navigator.gpu.getPreferredCanvasFormat();
    {
      const m = device.createShaderModule({ code: WGSL });
      const pl = device.createRenderPipeline({
        layout: 'auto', vertex: { module: m, entryPoint: 'vs' }, fragment: { module: m, entryPoint: 'fs', targets: [{ format: formatProbe }] },
        primitive: { topology: 'triangle-list' },
      });
      await WebGPURenderer.selfTest(device, formatProbe, { pipeline: pl, sampler: device.createSampler() });
    }
    const ctx = c.getContext('webgpu');
    const format = navigator.gpu.getPreferredCanvasFormat();
    ctx.configure({ device, format, alphaMode: 'opaque' });
    const module = device.createShaderModule({ code: WGSL });
    const pipeline = device.createRenderPipeline({
      layout: 'auto',
      vertex: { module, entryPoint: 'vs' },
      fragment: { module, entryPoint: 'fs', targets: [{ format }] },
      primitive: { topology: 'triangle-list' },
    });
    const r = new WebGPURenderer(c);
    const info = adapter.info || {};
    Object.assign(r, {
      device, ctx, format, pipeline, sampler: device.createSampler({ magFilter: 'linear', minFilter: 'linear' }), prev: null, last: null,
      name: 'webgpu', gpu: [info.vendor, info.architecture, info.description].filter(Boolean).join(' '),
    });
    r.crop = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
    r.cropKey = '';
    r.plainPass = { colorAttachments: [{ view: null, loadOp: 'clear', storeOp: 'store', clearValue: { r: 0, g: 0, b: 0, a: 1 } }], timestampWrites: undefined };
    r.plainBG = { layout: pipeline.getBindGroupLayout(0), entries: [{ binding: 0, resource: r.sampler }, { binding: 1, resource: null }, { binding: 2, resource: { buffer: r.crop } }] };
    r.log = log;
    r.initTimestamps();
    r.initFsr();
    r.initHdr();
    r.hdrCanvasOk = WebGPURenderer.hdrCanvasCheck(device);
    // A lost device draws nothing and reports no error: draw() throws then.
    device.lost.then((info) => {
      r.lost = true;
      if (info.reason !== 'destroyed') log(`WebGPU device lost (${info.message || info.reason})`);
    });
    try {
      device.pushErrorScope('validation');
      const pm = device.createShaderModule({ code: PROBE_WGSL });
      r.probePipeline = device.createRenderPipeline({
        layout: 'auto', vertex: { module: pm, entryPoint: 'vs' }, fragment: { module: pm, entryPoint: 'fs', targets: [{ format: 'rgba8unorm' }] },
        primitive: { topology: 'triangle-list' },
      });
      const err = await device.popErrorScope();
      if (err) throw new Error(err.message);
      r.probeTex = device.createTexture({ size: [P.BARCODE_COLS, P.BARCODE_ROWS], format: 'rgba8unorm', usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.COPY_SRC });
      r.probeCell = device.createBuffer({ size: 16, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
      // Two readbacks in flight at most (probe.inflight); rows are 256-byte aligned.
      r.probeBufs = [0, 1].map(() => device.createBuffer({ size: 256 * P.BARCODE_ROWS, usage: GPUBufferUsage.COPY_DST | GPUBufferUsage.MAP_READ }));
    } catch (e) {
      r.probePipeline = null; // the probe falls back to a frame copy
      log(`WebGPU latency probe unavailable (${e.message})`);
    }
    return r;
  }

  draw(frame, req, vis, planes) {
    if (this.lost) {
      planes?.release();
      frame.close();
      throw new Error('WebGPU device lost');
    }
    if (planes) {
      // HDR10: the planes' textures hold the picture, the frame can go
      // (and the kept one of an earlier SDR draw once the GPU is done).
      frame.close();
      this.idle();
      try {
        this.drawHdr(vis, planes);
      } finally {
        planes.release();
      }
      this.last = vis;
      return;
    }
    this.hdr.last = null;
    const buf = this.present(frame, vis, req);
    if (buf) req.luma = this.probeRead(buf);
    else if (req) req.clone = frame.clone();
    // Keep the frame alive until the next one so the GPU never samples a closed frame.
    if (this.prev) this.prev.close();
    this.prev = frame;
    this.last = vis;
  }

  // Draws frame letterboxed (and the probe cells when req asks for them):
  // upscaled with FSR (EASU + RCAS) when the setting and the scale call for
  // it and its pipelines are ready, else the bilinear sampler; returns the
  // probe's readback buffer, if any. Every pass is encoded and submitted
  // here, so the draw stage (stream-worker.js) covers them.
  present(frame, vis, req) {
    this.hdrCanvas('sdr');
    const { r } = this.fit(vis);
    const fsr = !upscaleWhy(this.up.mode, Math.min(r.w / this.inW, r.h / this.inH)) && this.fsrPipelines(this.up.input);
    const ext = this.device.importExternalTexture({ source: frame });
    const enc = this.device.createCommandEncoder();
    // A timestamp readback buffer when this draw is timed: FSR's, and the
    // plain path's for comparison once FSR has drawn (else nothing is timed).
    const stamp = fsr || this.gpuFsr.n ? this.stampSlot() : null;
    if (fsr) this.fsrPasses(enc, ext, r, stamp);
    else this.plainDraw(enc, ext, frame, vis, r, stamp);
    if (stamp) {
      enc.resolveQuerySet(this.ts.set, 0, 3, this.ts.resolve, 0);
      enc.copyBufferToBuffer(this.ts.resolve, 0, stamp, 0, 24);
    }
    const buf = req ? this.probePass(enc, ext, frame, req) : null;
    this.device.queue.submit([enc.finish()]);
    if (stamp) this.stampRead(stamp, fsr ? (this.up.input === 'copy' ? 'copy' : 'fsr') : 'plain');
    this.upscaled = fsr;
    return buf;
  }

  // The plain path: one triangle sampling the external texture bilinearly.
  plainDraw(enc, ext, frame, vis, r, stamp) {
    const key = `${vis.fx},${vis.fy},${frame.displayWidth},${frame.displayHeight}`;
    if (this.cropKey !== key) {
      // Texture coordinates span the visible part only (VideoConfig crop).
      this.cropKey = key;
      this.device.queue.writeBuffer(this.crop, 0, new Float32Array([vis.fx, vis.fy, vis.fx - 0.5 / frame.displayWidth, vis.fy - 0.5 / frame.displayHeight]));
    }
    this.plainBG.entries[1].resource = ext;
    const bg = this.device.createBindGroup(this.plainBG);
    this.plainBG.entries[1].resource = null;
    this.plainPass.colorAttachments[0].view = this.ctx.getCurrentTexture().createView();
    this.plainPass.timestampWrites = stamp ? this.ts.both : undefined;
    const pass = enc.beginRenderPass(this.plainPass);
    pass.setViewport(r.x, r.y, r.w, r.h, 0, 1);
    pass.setPipeline(this.pipeline);
    pass.setBindGroup(0, bg);
    pass.draw(3);
    pass.end();
    this.plainPass.colorAttachments[0].view = null;
  }

  // ---- FSR 1 upscaling (fsr1.js) -------------------------------------------
  // Created once: the uniform buffers and the pass and bind group descriptors
  // with the renderer; the pipelines when FSR is first needed (a renderer
  // that never shows a picture enlarged compiles nothing), only those of the
  // input in use (FSR_PIPES), asynchronously while the bilinear path keeps
  // drawing; the intermediate texture (EASU output, the output size) and the
  // copy (the input size) on size changes. Per frame only what WebGPU
  // requires: the bind group of the external texture (it expires with the
  // frame), the command encoder and the canvas texture's view; the uniforms
  // are written when they change.
  initFsr() {
    const d = this.device;
    const uniform = () => d.createBuffer({ size: 32, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
    const pass = () => ({ colorAttachments: [{ view: null, loadOp: 'clear', storeOp: 'store', clearValue: { r: 0, g: 0, b: 0, a: 1 } }], timestampWrites: undefined });
    const easuU = uniform();
    const rcasU = uniform();
    this.fsr = {
      pipes: {}, pending: {}, easuU, rcasU,
      easuData: new Float32Array(8), easuLast: new Float32Array(8).fill(NaN), rcasData: new Float32Array(8), rcasLast: new Float32Array(8).fill(NaN),
      inter: null, copyTex: null, rcasBG: null, easuTexBG: null,
      easuExtBG: { layout: null, entries: [{ binding: 0, resource: null }, { binding: 1, resource: { buffer: easuU } }] },
      copyBG: { layout: null, entries: [{ binding: 0, resource: null }] },
      easuPass: pass(), copyPass: pass(), rcasPass: pass(),
    };
    this.fsrError = '';
  }

  // Whether the pipelines of an input variant are ready; starts compiling
  // the missing ones (each once; none after a compile error).
  fsrPipelines(input) {
    const F = this.fsr;
    let ready = true;
    for (const name of WebGPURenderer.FSR_PIPES[input]) {
      if (F.pipes[name]) continue;
      ready = false;
      if (!F.pending[name] && !this.fsrError) F.pending[name] = this.compileFsr(name);
    }
    return ready;
  }

  async compileFsr(name) {
    const [code, format] = {
      easuExt: [easuWGSL(true), 'rgba8unorm'], easuTex: [easuWGSL(false), 'rgba8unorm'], copy: [COPY_WGSL, 'rgba8unorm'], rcas: [RCAS_WGSL, this.format],
    }[name];
    const module = this.device.createShaderModule({ code });
    try {
      const p = await this.device.createRenderPipelineAsync({
        layout: 'auto', vertex: { module, entryPoint: 'vs' }, fragment: { module, entryPoint: 'fs', targets: [{ format }] },
        primitive: { topology: 'triangle-list' },
      });
      this.fsr.pipes[name] = p;
      if (name === 'easuExt') this.fsr.easuExtBG.layout = p.getBindGroupLayout(0);
      if (name === 'copy') this.fsr.copyBG.layout = p.getBindGroupLayout(0);
      return true;
    } catch (e) {
      const info = await module.getCompilationInfo?.().catch(() => null);
      const m = info?.messages?.find((x) => x.type === 'error');
      const msg = m ? `${m.message} (line ${m.lineNum})` : e.message;
      if (!this.fsrError && !this.destroyed) this.log(`FSR upscaling unavailable (${name}: ${msg}); the WebGPU renderer scales bilinearly`);
      this.fsrError ||= `${name}: ${msg}`;
      return false;
    }
  }

  // Resolves to whether FSR can draw with the input set now, compiling what
  // it needs (tests; drawing never waits for it).
  get fsrReady() {
    const input = this.up.input;
    this.fsrPipelines(input);
    return Promise.all(WebGPURenderer.FSR_PIPES[input].map((n) => this.fsr.pending[n] || Promise.resolve(!!this.fsr.pipes[n]))).then((ok) => ok.every(Boolean));
  }

  // The intermediate texture at the output size (EASU -> RCAS) and, for
  // input "copy", the viewport's copy: (re)created when the size changes.
  fsrTargets(w, h, copy) {
    const F = this.fsr;
    const d = this.device;
    const usage = GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.TEXTURE_BINDING;
    if (!F.inter || F.inter.width !== w || F.inter.height !== h) {
      F.inter?.destroy(); // freed once the submitted passes that use it are done
      F.inter = d.createTexture({ size: [w, h], format: 'rgba8unorm', usage });
      const view = F.inter.createView();
      F.easuPass.colorAttachments[0].view = view;
      F.rcasBG = d.createBindGroup({ layout: F.pipes.rcas.getBindGroupLayout(0), entries: [{ binding: 0, resource: view }, { binding: 1, resource: { buffer: F.rcasU } }] });
    }
    if (copy && (!F.copyTex || F.copyTex.width !== this.inW || F.copyTex.height !== this.inH)) {
      F.copyTex?.destroy();
      F.copyTex = d.createTexture({ size: [this.inW, this.inH], format: 'rgba8unorm', usage });
      const view = F.copyTex.createView();
      F.copyPass.colorAttachments[0].view = view;
      F.easuTexBG = d.createBindGroup({ layout: F.pipes.easuTex.getBindGroupLayout(0), entries: [{ binding: 0, resource: view }, { binding: 1, resource: { buffer: F.easuU } }] });
    }
  }

  // Writes a uniform buffer only when its values changed (no per-frame upload).
  static writeChanged(device, buf, data, last) {
    for (let i = 0; i < data.length; i++) {
      if (data[i] !== last[i]) {
        device.queue.writeBuffer(buf, 0, data);
        last.set(data);
        return;
      }
    }
  }

  // EASU (frame -> intermediate at the output size), RCAS (intermediate ->
  // the canvas at the letterboxed rectangle), with the copy first for input
  // "copy". Timed (stamp) from the first pass's start to RCAS's end.
  fsrPasses(enc, ext, r, stamp) {
    const F = this.fsr;
    const d = this.device;
    const copy = this.up.input === 'copy';
    this.fsrTargets(r.w, r.h, copy);
    WebGPURenderer.writeChanged(d, F.easuU, easuConstants(F.easuData, this.inW, this.inH, r.w, r.h), F.easuLast);
    WebGPURenderer.writeChanged(d, F.rcasU, rcasConstants(F.rcasData, r.x, r.y, r.w, r.h, this.up.sharpness, this.up.denoise), F.rcasLast);
    let easuBG = F.easuTexBG;
    if (copy) {
      F.copyBG.entries[0].resource = ext;
      const bg = d.createBindGroup(F.copyBG);
      F.copyBG.entries[0].resource = null;
      F.copyPass.timestampWrites = stamp ? this.ts.copy : undefined;
      const p = enc.beginRenderPass(F.copyPass);
      p.setPipeline(F.pipes.copy);
      p.setBindGroup(0, bg);
      p.draw(3);
      p.end();
    } else {
      F.easuExtBG.entries[0].resource = ext;
      easuBG = d.createBindGroup(F.easuExtBG);
      F.easuExtBG.entries[0].resource = null;
    }
    F.easuPass.timestampWrites = stamp && !copy ? this.ts.begin : undefined;
    const e = enc.beginRenderPass(F.easuPass);
    e.setPipeline(copy ? F.pipes.easuTex : F.pipes.easuExt);
    e.setBindGroup(0, easuBG);
    e.draw(3);
    e.end();
    F.rcasPass.colorAttachments[0].view = this.ctx.getCurrentTexture().createView();
    F.rcasPass.timestampWrites = stamp ? this.ts.end : undefined;
    const p = enc.beginRenderPass(F.rcasPass);
    p.setViewport(r.x, r.y, r.w, r.h, 0, 1);
    p.setPipeline(F.pipes.rcas);
    p.setBindGroup(0, F.rcasBG);
    p.draw(3);
    p.end();
    F.rcasPass.colorAttachments[0].view = null;
  }

  // GPU time of the draw passes (timestamp-query, where the device has it):
  // at most every STAMP_MS, two readbacks in flight, once FSR has drawn; the
  // samples go to the FSR or the plain ring (input "copy": also the copy pass
  // alone, the cost the copy adds). Timestamps: 0 the first pass starts, 1 the last pass
  // (RCAS or the plain pass) ends, 2 the copy pass ends. Browsers may
  // quantize them (Chrome: 100 µs without its WebGPU developer features), so
  // a short pass often reads 0: zero-length samples are kept (in every ring),
  // which keeps the mean the overlay shows unbiased over many samples.
  initTimestamps() {
    this.gpuFsr = new Samples(100);
    this.gpuCopy = new Samples(100);
    this.gpuPlain = new Samples(100);
    this.ts = null;
    if (!this.device.features?.has('timestamp-query')) return;
    try {
      const d = this.device;
      const querySet = d.createQuerySet({ type: 'timestamp', count: 3 });
      this.ts = {
        set: querySet, last: -Infinity,
        resolve: d.createBuffer({ size: 24, usage: GPUBufferUsage.QUERY_RESOLVE | GPUBufferUsage.COPY_SRC }),
        free: [0, 1].map(() => d.createBuffer({ size: 24, usage: GPUBufferUsage.COPY_DST | GPUBufferUsage.MAP_READ })),
        both: { querySet, beginningOfPassWriteIndex: 0, endOfPassWriteIndex: 1 },
        begin: { querySet, beginningOfPassWriteIndex: 0 },
        copy: { querySet, beginningOfPassWriteIndex: 0, endOfPassWriteIndex: 2 },
        end: { querySet, endOfPassWriteIndex: 1 },
      };
    } catch {
      this.ts = null;
    }
  }

  stampSlot() {
    const T = this.ts;
    if (!T || !T.free.length) return null;
    const t = performance.now();
    if (t - T.last < WebGPURenderer.STAMP_MS) return null;
    T.last = t;
    return T.free.pop();
  }

  // kind: plain, fsr, or copy (FSR with input "copy").
  async stampRead(buf, kind) {
    try {
      await buf.mapAsync(GPUMapMode.READ);
      const v = new BigUint64Array(buf.getMappedRange(), 0, 3);
      // An invalid pair (end before start, or absurdly long) gives no sample.
      const ns = Number(v[1] - v[0]);
      const copyNs = Number(v[2] - v[0]);
      if (ns >= 0 && ns < 1e9) {
        (kind === 'plain' ? this.gpuPlain : this.gpuFsr).push(ns / 1e6);
        if (kind === 'copy' && copyNs >= 0 && copyNs <= ns) this.gpuCopy.push(copyNs / 1e6);
      }
    } catch {
      // device lost or destroyed: no sample
    } finally {
      if (buf.mapState === 'mapped') buf.unmap();
      this.ts.free.push(buf);
    }
  }

  noFsr() {
    if (this.hdr.last) return 'FSR is off for HDR streams (the HDR output pass scales bilinearly)';
    return this.fsrError ? `FSR unavailable (${this.fsrError})` : 'FSR shaders still compiling';
  }

  upscaleInfo() {
    const info = super.upscaleInfo();
    info.gpu = this.ts ? { method: 'timestamp-query', fsr: this.gpuFsr.summary(), copy: this.gpuCopy.summary(), plain: this.gpuPlain.summary() } : null;
    return info;
  }

  redraw() {
    if (this.hdr.last && this.last) this.redrawHdr(this.last);
    else if (this.prev && this.last) this.present(this.prev, this.last, null);
  }

  // ---- HDR10 (step 4.5, hdr.js) --------------------------------------------
  // Whether a WebGPU canvas here keeps extended range: a scratch canvas
  // configured rgba16float with toneMapping "extended" must report both back
  // (getConfiguration, Chrome 131+; browsers before toneMapping ignore it).
  static hdrCanvasCheck(device) {
    try {
      const ctx = new OffscreenCanvas(4, 4).getContext('webgpu');
      ctx.configure({ device, format: 'rgba16float', toneMapping: { mode: 'extended' }, alphaMode: 'opaque' });
      if (typeof ctx.getConfiguration !== 'function') return { ok: false, why: 'the canvas cannot confirm extended range (getConfiguration: Chrome 131+)' };
      const c = ctx.getConfiguration();
      ctx.unconfigure();
      if (c?.format !== 'rgba16float' || c?.toneMapping?.mode !== 'extended') {
        return { ok: false, why: `the canvas does not keep extended range (${c?.format}, toneMapping ${c?.toneMapping?.mode || 'standard'})` };
      }
      return { ok: true, why: '' };
    } catch (e) {
      return { ok: false, why: `the canvas cannot show extended range (${e.message})` };
    }
  }

  // Created with the renderer: uniforms and pass descriptors; the pipelines
  // with the first HDR frame (prepare); texture sets per plane layout (one
  // per frame between its copy and its draw, at most HDR_SETS), the
  // intermediate on size changes.
  initHdr() {
    const d = this.device;
    const u = (size) => d.createBuffer({ size, usage: GPUBufferUsage.UNIFORM | GPUBufferUsage.COPY_DST });
    const pass = () => ({ colorAttachments: [{ view: null, loadOp: 'clear', storeOp: 'store', clearValue: { r: 0, g: 0, b: 0, a: 1 } }] });
    this.hdr = {
      want: false, why: 'no HDR stream', white: HDR_WHITE, space: 'srgb', peak: 1000,
      canvasKey: 'sdr', readback: false, pipes: null, pending: null, error: '', failed: {},
      convU: u(16), convData: new Float32Array(4), convLast: new Float32Array(4).fill(NaN),
      outU: u(64), outData: new Float32Array(16), outLast: new Float32Array(16).fill(NaN),
      outIn: { space: null, white: 0, tone: false, peak: 0 }, // what outData was computed from
      layout: { fmt: undefined, w: 0, h: 0, L: null }, // the last planeLayout (hdrLayout)
      copyOpts: { rect: { x: 0, y: 0, width: 0, height: 0 } }, // copyTo's options (the visible rect), reused
      sets: [], inter: null, outBG: null, staging: null, convPass: pass(), outPass: pass(),
      copy: new Samples(100), bytes: 0, frame: null, last: null, drawn: 0,
    };
  }

  // want: show extended range (else tone-map to SDR), why not; white: SDR
  // reference white (cd/m2); space: the extended canvas's colour space
  // (srgb, display-p3); peak: the stream's peak for tone mapping (cd/m2).
  setHdr(o = {}) {
    const H = this.hdr;
    for (const k of ['want', 'why', 'white', 'space', 'peak']) if (o[k] !== undefined) H[k] = o[k];
  }

  // Why a decoded frame of an HDR10 generation (its format, visible rect)
  // cannot go through the HDR path, or null when it can: { scope, why } with
  // scope 'lost' (the device: another renderer takes over; or a closed
  // frame), 'renderer' (the HDR shaders failed) or 'format' (no plane path:
  // format null, which Chrome's hardware decoders give for 10-bit frames, or a
  // format whose copy failed). Such frames draw through importExternalTexture
  // (Chrome's SDR conversion) until the worker's withdrawn offer brings an
  // SDR generation.
  hdrBlocked(fmt, vr) {
    const H = this.hdr;
    if (this.lost) return { scope: 'lost', why: 'WebGPU device lost' };
    if (!vr) return { scope: 'lost', why: 'the frame is closed' };
    if (H.error) return { scope: 'renderer', why: H.error };
    if (H.failed[fmt]) return { scope: 'format', why: H.failed[fmt] };
    if (this.hdrLayout(fmt, vr.width, vr.height)) return null;
    return { scope: 'format', why: fmt ? `no HDR plane path for ${fmt} frames`
      : "VideoFrame.format null (a hardware decoder's P010 frames, which WebCodecs cannot copy)" };
  }

  // planeLayout(fmt, w, h), kept for the next frames of the same format and size.
  hdrLayout(fmt, w, h) {
    const c = this.hdr.layout;
    if (c.fmt !== fmt || c.w !== w || c.h !== h) this.hdr.layout = { fmt, w, h, L: planeLayout(fmt, w, h) };
    return this.hdr.layout.L;
  }

  async hdrPipes() {
    const H = this.hdr;
    if (H.pipes) return;
    H.pending ||= (async () => {
      const d = this.device;
      const make = async (code, format) => {
        const module = d.createShaderModule({ code });
        try {
          return await d.createRenderPipelineAsync({ layout: 'auto', vertex: { module, entryPoint: 'vs' }, fragment: { module, entryPoint: 'fs', targets: [{ format }] },
            primitive: { topology: 'triangle-list' } });
        } catch (e) {
          const info = await module.getCompilationInfo?.().catch(() => null);
          const m = info?.messages?.find((x) => x.type === 'error');
          throw new Error(m ? `${m.message} (line ${m.lineNum})` : e.message);
        }
      };
      try {
        const [conv, ext, tone] = await Promise.all([make(CONVERT_WGSL, 'rgba16float'), make(OUTPUT_WGSL, 'rgba16float'), make(OUTPUT_WGSL, this.format)]);
        H.pipes = { conv, ext, tone };
      } catch (e) {
        H.error = `HDR shaders: ${e.message}`;
        this.log(`HDR path unavailable (${e.message}); HDR frames draw through importExternalTexture`);
      }
    })();
    await H.pending;
  }

  // Copies the visible area of a decoded frame into a texture set (planes
  // as decoded, see hdr.js planeLayout) and resolves to it: { set, layout,
  // release() }. inspect(buf, layout, planes) sees the copy before the
  // staging buffer is reused (the latency probe's barcode, test hooks).
  async prepare(frame, inspect) {
    const H = this.hdr;
    await this.hdrPipes();
    if (H.error) throw new Error(H.error);
    const vr = frame.visibleRect;
    const fmt = frame.format;
    const L = this.hdrLayout(fmt, vr.width, vr.height);
    if (!L) throw new Error(`no HDR plane path for ${fmt || 'opaque'} frames`);
    const opts = H.copyOpts;
    const { rect } = opts;
    rect.x = vr.x;
    rect.y = vr.y;
    rect.width = vr.width;
    rect.height = vr.height;
    const t0 = performance.now();
    const size = frame.allocationSize(opts);
    if (!H.staging || H.staging.byteLength < size) H.staging = new Uint8Array(size);
    const layout = await frame.copyTo(H.staging, opts);
    if (this.lost || this.destroyed) throw new Error('WebGPU device lost');
    const set = this.hdrSet(L);
    for (let i = 0; i < L.planes.length; i++) {
      const src = set.src[i];
      src.offset = layout[i].offset;
      src.bytesPerRow = layout[i].stride;
      this.device.queue.writeTexture(set.dst[i], H.staging, src, set.size[i]);
    }
    H.copy.push(performance.now() - t0);
    H.bytes = size;
    // The overlay's "decoded" row: new only when the format, size or colour space changes.
    const F = H.frame;
    const cs = frame.colorSpace;
    if (!F || F.format !== fmt || F.w !== vr.width || F.h !== vr.height || F.colorSpace?.primaries !== cs?.primaries ||
      F.colorSpace?.transfer !== cs?.transfer || F.colorSpace?.matrix !== cs?.matrix || F.colorSpace?.fullRange !== cs?.fullRange) {
      H.frame = { format: fmt, colorSpace: cs?.toJSON?.() || null, w: vr.width, h: vr.height, bits: L.bits };
    }
    let released = false;
    const planes = { set, layout: L, renderer: this, release: () => { if (!released) { released = true; set.busy = false; } } };
    try {
      inspect?.(H.staging, layout, planes);
    } catch (e) {
      planes.release();
      throw e;
    }
    return planes;
  }

  hdrSet(L) {
    const H = this.hdr;
    let s = H.sets.find((x) => !x.busy && x.key === L.key);
    if (!s) {
      if (H.sets.filter((x) => x.key === L.key).length >= WebGPURenderer.HDR_SETS) throw new Error('no free HDR texture set (a frame kept its planes)');
      for (const x of H.sets) if (!x.busy && x.key !== L.key) x.tex.forEach((t) => t.destroy()); // another size or format: no longer needed
      H.sets = H.sets.filter((x) => x.busy || x.key === L.key);
      const d = this.device;
      const tex = L.planes.map((p) => d.createTexture({ size: [p.w, p.h], format: p.format, usage: GPUTextureUsage.TEXTURE_BINDING | GPUTextureUsage.COPY_DST }));
      const v = tex.map((t) => t.createView());
      const bg = d.createBindGroup({ layout: H.pipes.conv.getBindGroupLayout(0),
        entries: [{ binding: 0, resource: v[0] }, { binding: 1, resource: v[1] }, { binding: 2, resource: v[2] || v[1] }, { binding: 3, resource: { buffer: H.convU } }] });
      // writeTexture's arguments per plane (prepare sets the offset and stride of each copy).
      const dst = tex.map((texture) => ({ texture }));
      const src = L.planes.map((p) => ({ offset: 0, bytesPerRow: 0, rowsPerImage: p.h }));
      const size = L.planes.map((p) => [p.w, p.h]);
      s = { key: L.key, tex, bg, dst, src, size, busy: false };
      H.sets.push(s);
    }
    s.busy = true;
    return s;
  }

  // The canvas configuration: SDR (the preferred format: SDR streams and
  // tone-mapped HDR) or extended (rgba16float, toneMapping "extended", the
  // HDR colour space); readback (test hook) adds COPY_SRC.
  hdrCanvas(mode) {
    const H = this.hdr;
    const key = `${mode === 'extended' ? `extended:${H.space}` : 'sdr'}${H.readback ? ':readback' : ''}`;
    if (key === H.canvasKey) return;
    H.canvasKey = key;
    const usage = GPUTextureUsage.RENDER_ATTACHMENT | (H.readback ? GPUTextureUsage.COPY_SRC : 0);
    if (mode === 'extended') this.ctx.configure({ device: this.device, format: 'rgba16float', colorSpace: H.space, toneMapping: { mode: 'extended' }, alphaMode: 'opaque', usage });
    else this.ctx.configure({ device: this.device, format: this.format, alphaMode: 'opaque', usage });
  }

  // An HDR frame: planes -> the intermediate (PQ R'G'B', the visible size),
  // then the output pass onto the canvas at the letterboxed rectangle.
  drawHdr(vis, planes) {
    const H = this.hdr;
    const d = this.device;
    const { r } = this.fit(vis);
    const tone = !H.want;
    if (planes.capture) H.readback = true;
    this.hdrCanvas(tone ? 'sdr' : 'extended');
    if (!H.inter || H.inter.width !== this.inW || H.inter.height !== this.inH) {
      H.inter?.destroy(); // freed once the submitted passes that use it are done
      H.inter = d.createTexture({ size: [this.inW, this.inH], format: 'rgba16float', usage: GPUTextureUsage.RENDER_ATTACHMENT | GPUTextureUsage.TEXTURE_BINDING });
      const view = H.inter.createView();
      H.convPass.colorAttachments[0].view = view;
      H.outBG = [H.pipes.ext, H.pipes.tone].map((p) => d.createBindGroup({ layout: p.getBindGroupLayout(0),
        entries: [{ binding: 0, resource: this.sampler }, { binding: 1, resource: view }, { binding: 2, resource: { buffer: H.outU } }] }));
    }
    const L = planes.layout;
    H.convData.set([L.sub[0], L.sub[1], L.scale, L.semi ? 1 : 0]);
    WebGPURenderer.writeChanged(d, H.convU, H.convData, H.convLast);
    const enc = d.createCommandEncoder();
    const p = enc.beginRenderPass(H.convPass);
    p.setPipeline(H.pipes.conv);
    p.setBindGroup(0, planes.set.bg);
    p.draw(3);
    p.end();
    const tex = this.hdrOutput(enc, r, tone);
    const cap = planes.capture ? this.hdrCapture(enc, tex, r, planes.capture) : null;
    d.queue.submit([enc.finish()]);
    H.last = { tone };
    H.drawn++;
    this.upscaled = false;
    cap?.();
  }

  hdrOutput(enc, r, tone) {
    const H = this.hdr;
    const k = H.outIn;
    const space = tone ? 'srgb' : H.space;
    if (k.space !== space || k.white !== H.white || k.tone !== tone || k.peak !== H.peak) {
      k.space = space;
      k.white = H.white;
      k.tone = tone;
      k.peak = H.peak;
      outputUniforms(H.outData, k);
      WebGPURenderer.writeChanged(this.device, H.outU, H.outData, H.outLast);
    }
    const tex = this.ctx.getCurrentTexture();
    H.outPass.colorAttachments[0].view = tex.createView();
    const p = enc.beginRenderPass(H.outPass);
    p.setViewport(r.x, r.y, r.w, r.h, 0, 1);
    p.setPipeline(tone ? H.pipes.tone : H.pipes.ext);
    p.setBindGroup(0, H.outBG[tone ? 1 : 0]);
    p.draw(3);
    p.end();
    H.outPass.colorAttachments[0].view = null;
    return tex;
  }

  // The last HDR picture again (a resize, a changed setting): the output
  // pass from the intermediate, which still holds it.
  redrawHdr(vis) {
    const H = this.hdr;
    if (!H.inter) return;
    const { r } = this.fit(vis);
    const tone = !H.want;
    this.hdrCanvas(tone ? 'sdr' : 'extended');
    const enc = this.device.createCommandEncoder();
    this.hdrOutput(enc, r, tone);
    this.device.queue.submit([enc.finish()]);
    H.last = { tone };
  }

  // Test hook: the canvas pixels at points of the picture (visible-area
  // pixels: [x, y]) after this draw; c.resolve gets them with the output's
  // settings. Returns the readback to start after the submit.
  hdrCapture(enc, tex, r, c) {
    const H = this.hdr;
    const n = c.points.length;
    const buf = this.device.createBuffer({ size: 256 * n, usage: GPUBufferUsage.COPY_DST | GPUBufferUsage.MAP_READ });
    const at = c.points.map(([x, y]) => [Math.min(r.x + r.w - 1, Math.floor(r.x + ((x + 0.5) * r.w) / this.inW)), Math.min(r.y + r.h - 1, Math.floor(r.y + ((y + 0.5) * r.h) / this.inH))]);
    at.forEach(([x, y], k) => enc.copyTextureToBuffer({ texture: tex, origin: [x, y] }, { buffer: buf, offset: 256 * k }, [1, 1]));
    const format = tex.format;
    const out = { mode: H.want ? 'extended' : 'tonemap', white: H.white, space: H.want ? H.space : 'srgb', peak: H.peak, format, at, rect: r };
    return () => buf.mapAsync(GPUMapMode.READ).then(() => {
      const m = buf.getMappedRange();
      out.values = at.map((_, k) => {
        if (format === 'rgba16float') return [...new Uint16Array(m, 256 * k, 4)].slice(0, 3).map(halfToFloat);
        const b = new Uint8Array(m, 256 * k, 4);
        return (format.startsWith('bgra') ? [b[2], b[1], b[0]] : [b[0], b[1], b[2]]).map((v) => v / 255);
      });
      buf.unmap();
      buf.destroy();
      c.resolve(out);
    }, (e) => c.resolve({ ...out, error: e.message }));
  }

  hdrInfo() {
    const H = this.hdr;
    return {
      canvas: this.hdrCanvasOk, want: H.want, why: H.why, white: H.white, space: H.space, peak: H.peak, error: H.error, failed: H.failed,
      path: H.last ? (H.last.tone ? 'tonemap' : 'extended') : null, canvasConfig: H.canvasKey, frame: H.frame, drawn: H.drawn,
      copy: H.copy.summary(), bytes: H.bytes,
    };
  }

  probePass(enc, ext, frame, req) {
    if (!this.probePipeline || !this.probeBufs.length) return null;
    const buf = this.probeBufs.pop();
    this.device.queue.writeBuffer(this.probeCell, 0, new Float32Array([req.cell / frame.displayWidth, req.cell / frame.displayHeight, 0, 0]));
    const bg = this.device.createBindGroup({
      layout: this.probePipeline.getBindGroupLayout(0),
      entries: [{ binding: 0, resource: this.sampler }, { binding: 1, resource: ext }, { binding: 2, resource: { buffer: this.probeCell } }],
    });
    const pass = enc.beginRenderPass({ colorAttachments: [{ view: this.probeTex.createView(), loadOp: 'clear', storeOp: 'store' }] });
    pass.setPipeline(this.probePipeline);
    pass.setBindGroup(0, bg);
    pass.draw(3);
    pass.end();
    enc.copyTextureToBuffer({ texture: this.probeTex }, { buffer: buf, bytesPerRow: 256, rowsPerImage: P.BARCODE_ROWS }, [P.BARCODE_COLS, P.BARCODE_ROWS]);
    return buf;
  }

  async probeRead(buf) {
    try {
      await buf.mapAsync(GPUMapMode.READ);
      const px = new Uint8Array(buf.getMappedRange());
      const luma = [];
      for (let k = 0; k < P.BARCODE_BITS; k++) {
        const o = Math.floor(k / P.BARCODE_COLS) * 256 + (k % P.BARCODE_COLS) * 4;
        luma.push(0.299 * px[o] + 0.587 * px[o + 1] + 0.114 * px[o + 2]);
      }
      return luma;
    } finally {
      if (buf.mapState !== 'unmapped') { try { buf.unmap(); } catch {} }
      this.probeBufs.push(buf);
    }
  }

  // Another renderer takes over: the kept frame is closed once the GPU is done with it.
  idle() {
    const f = this.prev;
    this.prev = null;
    this.last = null;
    if (f) this.device.queue.onSubmittedWorkDone().catch(() => {}).finally(() => f.close());
  }

  destroy() {
    const f = this.prev;
    this.prev = null;
    this.destroyed = true;
    this.device.queue.onSubmittedWorkDone().catch(() => {}).finally(() => {
      f?.close();
      this.device.destroy();
    });
  }

  loseContext() {
    this.device.destroy(); // device.lost follows
  }
}

WebGPURenderer.STAMP_MS = 100;
// Texture sets per plane layout: the frame being copied, the one waiting
// for its draw, and slack (a set is free again once its frame is drawn or
// dropped).
WebGPURenderer.HDR_SETS = 4;
// The FSR pipelines each input variant (fsr1.js FSR.input) draws with.
WebGPURenderer.FSR_PIPES = { copy: ['copy', 'easuTex', 'rcas'], external: ['easuExt', 'rcas'] };

/** Creates the renderer for a path on canvas c (throws if the path does not work here); opts: { log, upscale }. */
export async function createRenderer(path, c, opts = {}) {
  let r;
  switch (path) {
    case 'webgl2': r = await WebGL2Renderer.create(c, opts); break;
    case 'webgpu': r = await WebGPURenderer.create(c, opts); break;
    default: r = await Canvas2DRenderer.create(c, opts);
  }
  r.setUpscale(opts.upscale);
  return r;
}
