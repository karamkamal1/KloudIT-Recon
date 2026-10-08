// HDR10 presentation (guide step 4.5, experimental), used by the WebGPU
// renderer (renderers.js) and the stream worker.
//
// An HDR10 generation (VideoConfig hdr: 10-bit BT.2020 PQ, limited range) is
// not drawn through importExternalTexture: Chrome converts an external
// texture to SDR (measured on Chromium 141 here: a PQ frame's 100 / 1000 /
// 10000 cd/m2 patches import as 0.51 / 0.75 / 0.996 linear, all inside
// [0, 1]: no headroom; docs/VENDOR_NOTES.md "3.9/4.5 HDR end to end"). The
// decoded planes are copied instead (VideoFrame.copyTo of the visible
// rectangle, writeTexture into r16uint / r8uint textures; formats in
// planeLayout), and two passes draw them:
//
//   convert  planes -> an rgba16float intermediate of the visible size:
//            BT.2020 non-constant-luminance Y'CbCr, limited range (Y 64-940,
//            CbCr 64-960 in 10-bit codes) -> R'G'B' (still PQ-encoded,
//            clamped to 0-1); 4:2:0 chroma interpolated bilinearly, sited as
//            HEVC's chroma_sample_loc_type 0 (co-sited with the even luma
//            column, between the two rows), which the native helper writes
//   output   the intermediate, sampled bilinearly onto the canvas at the
//            letterboxed rectangle: the SMPTE ST 2084 (PQ) EOTF -> cd/m2,
//            BT.2020 -> the canvas's primaries (sRGB / BT.709, or Display
//            P3), then
//              extended  (an HDR display, canvas rgba16float with toneMapping
//                        "extended"): cd/m2 / SDR white (reference white,
//                        203 cd/m2 per ITU-R BT.2408, configurable) = 1.0,
//                        values above 1 for highlights and below 0 for
//                        colours outside the canvas's gamut, encoded with the
//                        sRGB curve extended to all reals (the canvas takes
//                        extended-sRGB-encoded values: measured, 0.5 reads
//                        128 in a 2D canvas)
//              tonemap   (the HDR setting Off with an HDR stream running, the
//                        display left HDR mode, or a canvas without extended
//                        range): the ITU-R BT.2390 EETF (the Hermite knee in
//                        PQ space) applied to max(R, G, B), hue preserved by
//                        scaling R, G and B alike, from the stream's peak
//                        (MaxCLL, else the mastering display's, else 1000
//                        cd/m2) to the SDR white, then clipped to 0-1
//                        (colours outside the gamut clip) on an SDR canvas
//
// Both passes run in the draw call; the copy runs between the decoder's
// output and the frame pacer (stream-worker.js), its time counted in the draw
// stage. FSR upscaling is off for HDR frames (the bilinear output pass draws;
// renderers.js says why).

export const HDR_MODES = ['auto', 'off'];
export const HDR_LABELS = { auto: 'Auto', off: 'Off' };
// SDR reference white in cd/m2 (1.0 on an extended-range canvas; the SDR
// peak when tone mapping): ITU-R BT.2408's 203 by default.
export const HDR_WHITES = [100, 160, 203, 250, 300];
export const HDR_WHITE = 203;
export const hdrWhite = (v) => (HDR_WHITES.includes(+v) ? +v : HDR_WHITE);

// The codec strings the client asks isConfigSupported about for a 10-bit
// decoder (HEVC Main 10 level 5.1, AV1 Main 10-bit level 5.1).
export const HDR_CODECS = { hevc: 'hev1.2.4.L153.B0', av1: 'av01.0.13M.10' };

// SMPTE ST 2084.
const M1 = 2610 / 16384;
const M2 = (2523 / 4096) * 128;
const C1 = 3424 / 4096;
const C2 = (2413 / 4096) * 32;
const C3 = (2392 / 4096) * 32;
/** PQ value (0-1) -> cd/m2. */
export function pqEotf(e) {
  const p = Math.max(e, 0) ** (1 / M2);
  return 10000 * (Math.max(p - C1, 0) / (C2 - C3 * p)) ** (1 / M1);
}
/** cd/m2 -> PQ value (0-1). */
export function pqOetf(l) {
  const y = Math.min(Math.max(l / 10000, 0), 1) ** M1;
  return ((C1 + C2 * y) / (1 + C3 * y)) ** M2;
}

// CIE 1931 xy of the primaries (red, green, blue) and the D65 white point.
export const PRIMARIES = {
  bt2020: [[0.708, 0.292], [0.170, 0.797], [0.131, 0.046]],
  srgb: [[0.640, 0.330], [0.300, 0.600], [0.150, 0.060]],
  'display-p3': [[0.680, 0.320], [0.265, 0.690], [0.150, 0.060]],
};
const D65 = [0.3127, 0.3290];

function inv3(m) {
  const [a, b, c, d, e, f, g, h, i] = m.flat();
  const A = e * i - f * h;
  const B = -(d * i - f * g);
  const C = d * h - e * g;
  const det = a * A + b * B + c * C;
  return [[A / det, -(b * i - c * h) / det, (b * f - c * e) / det], [B / det, (a * i - c * g) / det, -(a * f - c * d) / det],
    [C / det, -(a * h - b * g) / det, (a * e - b * d) / det]];
}
const mul3 = (x, y) => x.map((row) => [0, 1, 2].map((j) => row.reduce((s, v, k) => s + v * y[k][j], 0)));
// RGB -> XYZ of a set of primaries with the D65 white (SMPTE RP 177).
function rgbToXyz(p) {
  const cols = p.map(([x, y]) => [x / y, 1, (1 - x - y) / y]);
  const m = [0, 1, 2].map((r) => cols.map((c) => c[r]));
  const w = [D65[0] / D65[1], 1, (1 - D65[0] - D65[1]) / D65[1]];
  const s = inv3(m).map((row) => row.reduce((acc, v, k) => acc + v * w[k], 0));
  return m.map((row) => row.map((v, k) => v * s[k]));
}
/** The 3x3 matrix (rows) from linear BT.2020 to linear RGB of a canvas colour space (srgb, display-p3). */
export function fromBT2020(space) {
  return mul3(inv3(rgbToXyz(PRIMARIES[space] || PRIMARIES.srgb)), rgbToXyz(PRIMARIES.bt2020));
}

/** The peak luminance an HDR10 stream's tone mapping starts from: MaxCLL, else the mastering display's peak, else 1000 cd/m2. */
export function sourcePeak(md) {
  if (md?.maxCll > 0) return md.maxCll;
  if (md?.maxLuminance > 0) return md.maxLuminance;
  return 1000;
}

// The decoder's output formats the copy handles (VideoFrame.format): plane
// texture formats, chroma subsampling, whether U and V share a plane (NV12,
// P010), and the factor from a texel's value to a 10-bit code (8-bit x 4,
// 12-bit / 4; P010 keeps its 10 bits at the top of 16).
const FORMATS = {
  I420P10: { tex: ['r16uint', 'r16uint', 'r16uint'], sub: [2, 2], scale: 1 },
  I420P12: { tex: ['r16uint', 'r16uint', 'r16uint'], sub: [2, 2], scale: 0.25 },
  I422P10: { tex: ['r16uint', 'r16uint', 'r16uint'], sub: [2, 1], scale: 1 },
  I422P12: { tex: ['r16uint', 'r16uint', 'r16uint'], sub: [2, 1], scale: 0.25 },
  I444P10: { tex: ['r16uint', 'r16uint', 'r16uint'], sub: [1, 1], scale: 1 },
  I444P12: { tex: ['r16uint', 'r16uint', 'r16uint'], sub: [1, 1], scale: 0.25 },
  P010: { tex: ['r16uint', 'rg16uint'], sub: [2, 2], scale: 1 / 64 },
  I420: { tex: ['r8uint', 'r8uint', 'r8uint'], sub: [2, 2], scale: 4 },
  I422: { tex: ['r8uint', 'r8uint', 'r8uint'], sub: [2, 1], scale: 4 },
  I444: { tex: ['r8uint', 'r8uint', 'r8uint'], sub: [1, 1], scale: 4 },
  NV12: { tex: ['r8uint', 'rg8uint'], sub: [2, 2], scale: 4 },
};
FORMATS.I420AP10 = { ...FORMATS.I420P10, tex: [...FORMATS.I420P10.tex] }; // alpha plane not copied
FORMATS.I420A = { ...FORMATS.I420, tex: [...FORMATS.I420.tex] };

/**
 * How the planes of a w x h frame of format fmt (VideoFrame.format) are
 * uploaded: { key, planes: [{ format, w, h }], sub, scale, semi }, or null for
 * a format without a plane path (opaque GPU frames: format null; RGB).
 */
export function planeLayout(fmt, w, h) {
  const f = FORMATS[fmt];
  if (!f || !(w > 0 && h > 0)) return null;
  const cw = Math.ceil(w / f.sub[0]);
  const ch = Math.ceil(h / f.sub[1]);
  const planes = f.tex.map((format, i) => ({ format, w: i ? cw : w, h: i ? ch : h }));
  return { key: `${fmt}:${w}x${h}`, planes, sub: f.sub, scale: f.scale, semi: planes.length === 2, bits: f.tex[0] === 'r8uint' ? 8 : fmt.endsWith('P12') ? 12 : 10 };
}

/** Whether a frame's colour space is HDR10's (BT.2020, PQ). */
export const isPQ = (cs) => cs?.transfer === 'pq';

export const CONVERT_WGSL = `
@group(0) @binding(0) var yTex: texture_2d<u32>;
@group(0) @binding(1) var uTex: texture_2d<u32>;
@group(0) @binding(2) var vTex: texture_2d<u32>;
struct Conv { sub: vec2f, scale: f32, semi: f32 };
@group(0) @binding(3) var<uniform> conv: Conv;
@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  return vec4f(p[i], 0.0, 1.0);
}
fn ld(t: texture_2d<u32>, p: vec2i) -> vec2f {
  let v = textureLoad(t, clamp(p, vec2i(0), vec2i(textureDimensions(t)) - 1), 0);
  return vec2f(f32(v.r), f32(v.g));
}
fn cbcr(i: vec2i) -> vec2f {
  let u = ld(uTex, i);
  return select(vec2f(u.x, ld(vTex, i).x), u, conv.semi > 0.5);
}
@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  let p = vec2i(pos.xy);
  // Chroma sample (i, j) sits at luma column sub.x * i and between the rows
  // of its block (chroma_sample_loc_type 0).
  let c = vec2f(f32(p.x) / conv.sub.x, (f32(p.y) - 0.5 * (conv.sub.y - 1.0)) / conv.sub.y);
  let c0 = floor(c);
  let f = c - c0;
  let i = vec2i(c0);
  let ch = mix(mix(cbcr(i), cbcr(i + vec2i(1, 0)), f.x), mix(cbcr(i + vec2i(0, 1)), cbcr(i + vec2i(1, 1)), f.x), f.y) * conv.scale;
  let y = (ld(yTex, p).x * conv.scale - 64.0) / 876.0;
  let cb = (ch.x - 512.0) / 896.0;
  let cr = (ch.y - 512.0) / 896.0;
  // BT.2020 NCL: Kr 0.2627, Kb 0.0593.
  let rgb = vec3f(y + 1.4746 * cr, y - 0.16455312 * cb - 0.57135313 * cr, y + 1.8814 * cb);
  return vec4f(clamp(rgb, vec3f(0.0), vec3f(1.0)), 1.0);
}`;

export const OUTPUT_WGSL = `
@group(0) @binding(0) var samp: sampler;
@group(0) @binding(1) var inter: texture_2d<f32>;
struct Out { m0: vec4f, m1: vec4f, m2: vec4f, white: f32, tone: f32, srcPeak: f32, dstPeak: f32 };
@group(0) @binding(2) var<uniform> o: Out;
struct VOut { @builtin(position) pos: vec4f, @location(0) uv: vec2f };
@vertex fn vs(@builtin(vertex_index) i: u32) -> VOut {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  var v: VOut;
  v.pos = vec4f(p[i], 0.0, 1.0);
  v.uv = vec2f((p[i].x + 1.0) * 0.5, (1.0 - p[i].y) * 0.5);
  return v;
}
const M1 = 0.1593017578125;
const M2 = 78.84375;
const C1 = 0.8359375;
const C2 = 18.8515625;
const C3 = 18.6875;
fn eotf(e: vec3f) -> vec3f {
  let p = pow(max(e, vec3f(1e-12)), vec3f(1.0 / M2));
  return 10000.0 * pow(max(max(p - C1, vec3f(0.0)) / (C2 - C3 * p), vec3f(1e-12)), vec3f(1.0 / M1));
}
fn oetf(l: f32) -> f32 {
  let y = pow(clamp(l / 10000.0, 1e-12, 1.0), M1);
  return pow((C1 + C2 * y) / (1.0 + C3 * y), M2);
}
// ITU-R BT.2390 EETF, black at 0: PQ e of the source range [0, srcPeak] into
// [0, dstPeak] (PQ values): identity below the knee KS, a Hermite spline
// from it to the target peak.
fn eetf(e: f32) -> f32 {
  let e1 = min(e / o.srcPeak, 1.0);
  let maxLum = o.dstPeak / o.srcPeak;
  let ks = 1.5 * maxLum - 0.5;
  if (ks >= 1.0 || e1 < ks) { return e1 * o.srcPeak; }
  let t = (e1 - ks) / (1.0 - ks);
  let t2 = t * t;
  let t3 = t2 * t;
  return ((2.0 * t3 - 3.0 * t2 + 1.0) * ks + (t3 - 2.0 * t2 + t) * (1.0 - ks) + (-2.0 * t3 + 3.0 * t2) * maxLum) * o.srcPeak;
}
fn srgb(c: vec3f) -> vec3f {
  let a = abs(c);
  return sign(c) * select(1.055 * pow(max(a, vec3f(1e-12)), vec3f(1.0 / 2.4)) - 0.055, 12.92 * a, a <= vec3f(0.0031308));
}
@fragment fn fs(v: VOut) -> @location(0) vec4f {
  var lin = eotf(textureSampleLevel(inter, samp, v.uv, 0.0).rgb);
  if (o.tone > 0.5) {
    let m = max(max(lin.r, lin.g), lin.b);
    if (m > 0.0) {
      lin = lin * (eotf(vec3f(eetf(oetf(m)))).x / m);
    }
  }
  var rgb = vec3f(dot(o.m0.xyz, lin), dot(o.m1.xyz, lin), dot(o.m2.xyz, lin)) / o.white;
  if (o.tone > 0.5) {
    rgb = clamp(rgb, vec3f(0.0), vec3f(1.0));
  }
  return vec4f(srgb(rgb), 1.0);
}`;

/**
 * The output pass's uniforms (12 + 4 floats): the BT.2020 -> canvas matrix,
 * the SDR white, tone mapping on/off and its source / target peaks (PQ).
 */
export function outputUniforms(out, { space, white, tone, peak }) {
  const m = fromBT2020(space);
  for (let r = 0; r < 3; r++) out.set([...m[r], 0], 4 * r);
  out[12] = white;
  out[13] = tone ? 1 : 0;
  out[14] = pqOetf(peak);
  out[15] = pqOetf(white);
  return out;
}

/** Half-float bits -> number. */
export function halfToFloat(h) {
  const s = h & 0x8000 ? -1 : 1;
  const e = (h >> 10) & 31;
  const m = h & 1023;
  if (e === 31) return m ? NaN : s * Infinity;
  return s * (e ? 2 ** (e - 15) * (1 + m / 1024) : 2 ** -14 * (m / 1024));
}
