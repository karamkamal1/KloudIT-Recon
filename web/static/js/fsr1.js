// Client-side upscaling (guide Phase 5): AMD FidelityFX Super Resolution 1.0
// (FSR 1) ported to WGSL for the WebGPU presentation path (renderers.js).
//
// Ported from ffx_fsr1.h (and the approximations it uses from ffx_a.h),
// https://github.com/GPUOpen-Effects/FidelityFX-FSR, ffx-fsr/ffx_fsr1.h
// "v1.20210629", commit a21ffb8f6c13233ba336352bdff293894c706575; see
// third_party/README.md. The ported code below is under the MIT license of
// the original:
//
// Copyright (c) 2021 Advanced Micro Devices, Inc. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.
//
// Fragment passes of one triangle each (the WebGPU renderer's style; the
// canvas cannot be a storage texture without bgra8unorm-storage, and a
// fragment pass writes it directly), after the copy of input "copy" (below):
//
//   EASU  edge-adaptive spatial upsampling (FsrEasuF, the 32-bit version):
//         the 12-tap kernel b c / e f g h / i j k l / n o around the input
//         texel f under each output pixel, edge direction and length from the
//         luma of the four 2x2 bilinear quads, an anisotropic Lanczos-2
//         approximation along the edge, clamped to the min/max of f g j k (no
//         ringing). Input: the decoded frame (or its copy), output: an
//         rgba8unorm texture of the output size.
//   RCAS  robust contrast-adaptive sharpening (FsrRcasF): the 5-tap cross,
//         the negative lobe solved for the largest value that clips nothing
//         (the limiter), at most FSR_RCAS_LIMIT, scaled by the sharpness
//         (stops: 0 = the most, each stop halves it), optionally reduced on
//         what looks like noise (FSR_RCAS_DENOISE, here a uniform). Output:
//         the canvas, at the letterboxed rectangle.
//
// Colour space: FSR 1 wants perceptual (gamma-encoded, "as if presenting to an
// sRGB display") input in [0, 1]. Decoded video is already non-linear (BT.709
// / sRGB transfer), and WebGPU's external texture hands it over that way, so
// both passes run on it as it is: no linearisation, no conversion between the
// passes (as ffx_fsr1.h asks: "Pass EASU output straight into RCAS").
//
// Differences from ffx_fsr1.h, none in the math:
//   - FsrEasuRF/GF/BF gather each channel of a 2x2 quad (textureGather);
//     texture_external has no gather in WGSL, so the 12 texels are loaded one
//     by one (textureLoad, integer coordinates): the same taps.
//   - The input "viewport" (FsrEasuCon) is the part of the frame the video
//     config shows (P.visibleArea, step 1.7): taps are clamped to it, not to
//     the texture (FSR clamps to the texture edges with its sampler): rows and
//     columns beyond it are encoder padding and must not bleed in. RCAS
//     clamps its taps to the EASU output (ffx_fsr1.h leaves borders to the
//     load callback).
//   - Where a limiter side of RCAS cannot clip (a channel at 0 or at 1 on the
//     whole ring), ffx_fsr1.h computes 0 * inf or 0 / 0 and relies on the
//     GPU's NaN-dropping max() to take the other side; WGSL lets an
//     implementation assume no NaN, so that side is dropped explicitly.
//   - Input (FSR.input): "copy" (the default) first copies the frame's
//     viewport into an rgba8unorm texture of its size, one external-texture
//     load (two planes and the YUV -> RGB conversion) per input pixel, and
//     EASU loads from that; "external" loads EASU's twelve taps from the
//     external texture itself, twelve conversions per output pixel and one
//     pass less. Same taps, same result for 8-bit input; on the test
//     machine's SwiftShader "external" cost 1.7 to 1.9 times the GPU time
//     of "copy" (copy pass included) at 960x540 -> 1920x1080
//     (docs/VENDOR_NOTES.md, Phase 5).

/** Upscaling choices (settings): auto, off, fsr. */
export const UPSCALE = ['auto', 'off', 'fsr'];
export const UPSCALE_LABELS = { auto: 'Auto', off: 'Off', fsr: 'FSR 1' };

// autoMin: Auto upscales above this scale factor (output / input, per axis);
// sharpness: the RCAS default in stops; maxSharpness: the settings slider's
// end; input: where EASU reads the frame (copy: an rgba8unorm copy of the
// viewport; external: the external texture, twelve loads per output pixel).
export const FSR = { autoMin: 1.05, sharpness: 0.2, maxSharpness: 2, input: 'copy' };

/** Normalised upscaling settings ({ mode, sharpness, denoise, input }). */
export function upscaleSettings(o = {}) {
  const s = Number(o.sharpness);
  return {
    mode: UPSCALE.includes(o.mode) ? o.mode : 'auto',
    sharpness: o.sharpness !== undefined && o.sharpness !== null && Number.isFinite(s) ? Math.min(FSR.maxSharpness, Math.max(0, s)) : FSR.sharpness,
    denoise: !!o.denoise,
    input: o.input === 'copy' || o.input === 'external' ? o.input : FSR.input,
  };
}

/**
 * Why a picture shown at `scale` times its size (output / input, the
 * smaller axis, device pixels) is not upscaled with FSR, '' when it is:
 * never when the output is not larger than the input (a picture shown at its
 * size or smaller takes the plain path); mode "fsr" whenever it is larger,
 * "auto" above FSR.autoMin, "off" never.
 */
export function upscaleWhy(mode, scale) {
  if (mode === 'off') return 'off';
  if (!(scale > 1)) return 'not enlarged';
  if (mode === 'auto' && !(scale > FSR.autoMin)) return 'auto: enlarged 5 % or less';
  return '';
}

/** upscaleWhy for a picture of inW x inH shown at outW x outH: { active, scale, why }. */
export function upscalePlan(mode, inW, inH, outW, outH) {
  const scale = Math.min(outW / inW, outH / inH);
  const why = upscaleWhy(mode, scale);
  return { active: !why, scale, why };
}

/**
 * FsrEasuCon for the integer-coordinate port: con0 (output pixel -> input
 * texel position: xy scale, zw offset) for an input viewport of inW x inH
 * shown at outW x outH, and the last texel of the viewport (clamp). The
 * normalised gather offsets (con1..con3) are not needed with integer loads.
 */
export function easuConstants(out, inW, inH, outW, outH) {
  out[0] = inW / outW;
  out[1] = inH / outH;
  out[2] = 0.5 * inW / outW - 0.5;
  out[3] = 0.5 * inH / outH - 0.5;
  out[4] = Math.ceil(inW) - 1;
  out[5] = Math.ceil(inH) - 1;
  out[6] = 0;
  out[7] = 0;
  return out;
}

/**
 * FsrRcasCon plus this port's placement: origin (the letterboxed rectangle's
 * top-left in the canvas), the last texel of the EASU output, the sharpness
 * as a linear factor (exp2(-stops)) and the denoise switch.
 */
export function rcasConstants(out, x, y, w, h, stops, denoise) {
  out[0] = x;
  out[1] = y;
  out[2] = w - 1;
  out[3] = h - 1;
  out[4] = Math.pow(2, -stops);
  out[5] = denoise ? 1 : 0;
  out[6] = 0;
  out[7] = 0;
  return out;
}

const VS = `
@vertex fn vs(@builtin(vertex_index) i: u32) -> @builtin(position) vec4f {
  var p = array<vec2f, 3>(vec2f(-1.0, -3.0), vec2f(-1.0, 1.0), vec2f(3.0, 1.0));
  return vec4f(p[i], 0.0, 1.0);
}`;

// ffx_a.h approximations, bit for bit (they shape FSR's response).
const APPROX = `
fn APrxLoRcpF1(a: f32) -> f32 { return bitcast<f32>(0x7ef07ebbu - bitcast<u32>(a)); }
fn APrxLoRsqF1(a: f32) -> f32 { return bitcast<f32>(0x5f347d74u - (bitcast<u32>(a) >> 1u)); }
fn APrxMedRcpF1(a: f32) -> f32 {
  let b = bitcast<f32>(0x7ef19fffu - bitcast<u32>(a));
  return b * (-b * a + 2.0);
}
// Simplest multi-channel approximate luma possible (luma times 2, in 2 FMA/MAD).
fn luma2(c: vec3f) -> f32 { return c.b * 0.5 + (c.r * 0.5 + c.g); }`;

/**
 * The EASU pass. external: the frame's external texture as input (true) or
 * the rgba8unorm copy of its viewport (false).
 */
export function easuWGSL(external) {
  const src = external
    ? `@group(0) @binding(0) var src: texture_external;
fn tap(p: vec2i, lim: vec2i) -> vec3f { return textureLoad(src, clamp(p, vec2i(0), lim)).rgb; }
fn limit() -> vec2i { return min(vec2i(u.last.xy), vec2i(textureDimensions(src)) - 1); }`
    : `@group(0) @binding(0) var src: texture_2d<f32>;
fn tap(p: vec2i, lim: vec2i) -> vec3f { return textureLoad(src, clamp(p, vec2i(0), lim), 0).rgb; }
fn limit() -> vec2i { return min(vec2i(u.last.xy), vec2i(textureDimensions(src)) - 1); }`;
  return `
struct Easu {
  con0: vec4f, // FsrEasuCon con0: input texel position = output pixel * xy + zw
  last: vec4f, // xy: the last texel of the input viewport (taps are clamped to it)
};
@group(0) @binding(1) var<uniform> u: Easu;
${src}
${VS}
${APPROX}

// FsrEasuTapF: filtering for a given tap.
fn FsrEasuTapF(aC: ptr<function, vec3f>, aW: ptr<function, f32>, off: vec2f, dir: vec2f, len: vec2f, lob: f32, clp: f32, c: vec3f) {
  // Rotate offset by direction.
  var v = vec2f(off.x * dir.x + off.y * dir.y, off.x * (-dir.y) + off.y * dir.x);
  // Anisotropy.
  v *= len;
  // Compute distance^2, limited to the window (at a corner 2 taps can easily be outside).
  let d2 = min(v.x * v.x + v.y * v.y, clp);
  // Approximation of lanczos2 without sin() or rcp(), or sqrt() to get x:
  //  (25/16 * (2/5 * x^2 - 1)^2 - (25/16 - 1)) * (1/4 * x^2 - 1)^2
  //  |_______________________________________|   |_______________|
  //                   base                             window
  var wB = (2.0 / 5.0) * d2 - 1.0;
  var wA = lob * d2 - 1.0;
  wB *= wB;
  wA *= wA;
  wB = (25.0 / 16.0) * wB - (25.0 / 16.0 - 1.0);
  let w = wB * wA;
  *aC += c * w;
  *aW += w;
}

// FsrEasuSetF: accumulate direction and length of one 2x2 quad (bilinear weight w).
//    a
//  b c d
//    e
fn FsrEasuSetF(dir: ptr<function, vec2f>, len: ptr<function, f32>, w: f32, lA: f32, lB: f32, lC: f32, lD: f32, lE: f32) {
  // Direction is the '+' diff; length converts gradient reversal to 0,
  // smoothly to non-reversal at 1, shaped, then adding horz and vert terms.
  let dc = lD - lC;
  let cb = lC - lB;
  var lenX = APrxLoRcpF1(max(abs(dc), abs(cb)));
  let dirX = lD - lB;
  (*dir).x += dirX * w;
  lenX = saturate(abs(dirX) * lenX);
  lenX *= lenX;
  *len += lenX * w;
  // Repeat for the y axis.
  let ec = lE - lC;
  let ca = lC - lA;
  var lenY = APrxLoRcpF1(max(abs(ec), abs(ca)));
  let dirY = lE - lA;
  (*dir).y += dirY * w;
  lenY = saturate(abs(dirY) * lenY);
  lenY *= lenY;
  *len += lenY * w;
}

// FsrEasuF.
@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  // Get position of 'f'.
  var pp = floor(pos.xy) * u.con0.xy + u.con0.zw;
  let fp = floor(pp);
  pp -= fp;
  let o = vec2i(fp);
  let lim = limit();
  // 12-tap kernel.
  //    b c
  //  e f g h
  //  i j k l
  //    n o
  let b = tap(o + vec2i(0, -1), lim);
  let c = tap(o + vec2i(1, -1), lim);
  let e = tap(o + vec2i(-1, 0), lim);
  let f = tap(o, lim);
  let g = tap(o + vec2i(1, 0), lim);
  let h = tap(o + vec2i(2, 0), lim);
  let i = tap(o + vec2i(-1, 1), lim);
  let j = tap(o + vec2i(0, 1), lim);
  let k = tap(o + vec2i(1, 1), lim);
  let l = tap(o + vec2i(2, 1), lim);
  let n = tap(o + vec2i(0, 2), lim);
  let oo = tap(o + vec2i(1, 2), lim);
  let bL = luma2(b);
  let cL = luma2(c);
  let eL = luma2(e);
  let fL = luma2(f);
  let gL = luma2(g);
  let hL = luma2(h);
  let iL = luma2(i);
  let jL = luma2(j);
  let kL = luma2(k);
  let lL = luma2(l);
  let nL = luma2(n);
  let oL = luma2(oo);
  // Accumulate for bilinear interpolation.
  var dir = vec2f(0.0);
  var len = 0.0;
  FsrEasuSetF(&dir, &len, (1.0 - pp.x) * (1.0 - pp.y), bL, eL, fL, gL, jL);
  FsrEasuSetF(&dir, &len, pp.x * (1.0 - pp.y), cL, fL, gL, hL, kL);
  FsrEasuSetF(&dir, &len, (1.0 - pp.x) * pp.y, fL, iL, jL, kL, nL);
  FsrEasuSetF(&dir, &len, pp.x * pp.y, gL, jL, kL, lL, oL);
  // Normalize with approximation, and cleanup close to zero.
  let dir2 = dir * dir;
  var dirR = dir2.x + dir2.y;
  let zro = dirR < 1.0 / 32768.0;
  dirR = APrxLoRsqF1(dirR);
  dirR = select(dirR, 1.0, zro);
  dir.x = select(dir.x, 1.0, zro);
  dir *= vec2f(dirR);
  // Transform from {0 to 2} to {0 to 1} range, and shape with square.
  len = len * 0.5;
  len *= len;
  // Stretch kernel {1.0 vert|horz, to sqrt(2.0) on diagonal}.
  let stretch = (dir.x * dir.x + dir.y * dir.y) * APrxLoRcpF1(max(abs(dir.x), abs(dir.y)));
  // Anisotropic length after rotation: x 1.0 lerp to 'stretch' on edges, y 1.0 lerp to 2x on edges.
  let len2 = vec2f(1.0 + (stretch - 1.0) * len, 1.0 - 0.5 * len);
  // Based on the amount of 'edge', the window shifts from +/-{sqrt(2.0) to slightly beyond 2.0}.
  let lob = 0.5 + ((1.0 / 4.0 - 0.04) - 0.5) * len;
  // Set distance^2 clipping point to the end of the adjustable window.
  let clp = APrxLoRcpF1(lob);
  // Accumulation mixed with min/max of 4 nearest.
  let min4 = min(min(min(f, g), j), k);
  let max4 = max(max(max(f, g), j), k);
  var aC = vec3f(0.0);
  var aW = 0.0;
  FsrEasuTapF(&aC, &aW, vec2f(0.0, -1.0) - pp, dir, len2, lob, clp, b);
  FsrEasuTapF(&aC, &aW, vec2f(1.0, -1.0) - pp, dir, len2, lob, clp, c);
  FsrEasuTapF(&aC, &aW, vec2f(-1.0, 1.0) - pp, dir, len2, lob, clp, i);
  FsrEasuTapF(&aC, &aW, vec2f(0.0, 1.0) - pp, dir, len2, lob, clp, j);
  FsrEasuTapF(&aC, &aW, vec2f(0.0, 0.0) - pp, dir, len2, lob, clp, f);
  FsrEasuTapF(&aC, &aW, vec2f(-1.0, 0.0) - pp, dir, len2, lob, clp, e);
  FsrEasuTapF(&aC, &aW, vec2f(1.0, 1.0) - pp, dir, len2, lob, clp, k);
  FsrEasuTapF(&aC, &aW, vec2f(2.0, 1.0) - pp, dir, len2, lob, clp, l);
  FsrEasuTapF(&aC, &aW, vec2f(2.0, 0.0) - pp, dir, len2, lob, clp, h);
  FsrEasuTapF(&aC, &aW, vec2f(1.0, 0.0) - pp, dir, len2, lob, clp, g);
  FsrEasuTapF(&aC, &aW, vec2f(1.0, 2.0) - pp, dir, len2, lob, clp, oo);
  FsrEasuTapF(&aC, &aW, vec2f(0.0, 2.0) - pp, dir, len2, lob, clp, n);
  // Normalize and dering.
  return vec4f(min(max4, max(min4, aC * (1.0 / aW))), 1.0);
}`;
}

/** The RCAS pass, onto the canvas (the EASU output as input). */
export const RCAS_WGSL = `
struct Rcas {
  place: vec4f, // xy: the letterboxed rectangle's top-left in the canvas; zw: the last texel of the EASU output
  con: vec4f,   // x: FsrRcasCon con[0], the sharpness as a linear factor (exp2(-stops)); y: 1 = FSR_RCAS_DENOISE
};
@group(0) @binding(0) var img: texture_2d<f32>;
@group(0) @binding(1) var<uniform> u: Rcas;
${VS}
${APPROX}

// This is set at the limit of providing unnatural results for sharpening.
const FSR_RCAS_LIMIT = 0.25 - (1.0 / 16.0);

fn FsrRcasLoadF(p: vec2i) -> vec3f { return textureLoad(img, clamp(p, vec2i(0), vec2i(u.place.zw)), 0).rgb; }

// FsrRcasF.
@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  // Algorithm uses minimal 3x3 pixel neighborhood.
  //    b
  //  d e f
  //    h
  let sp = vec2i(floor(pos.xy - u.place.xy));
  let b = FsrRcasLoadF(sp + vec2i(0, -1));
  let d = FsrRcasLoadF(sp + vec2i(-1, 0));
  let e = FsrRcasLoadF(sp);
  let f = FsrRcasLoadF(sp + vec2i(1, 0));
  let h = FsrRcasLoadF(sp + vec2i(0, 1));
  // Luma times 2.
  let bL = luma2(b);
  let dL = luma2(d);
  let eL = luma2(e);
  let fL = luma2(f);
  let hL = luma2(h);
  // Noise detection.
  var nz = 0.25 * bL + 0.25 * dL + 0.25 * fL + 0.25 * hL - eL;
  nz = saturate(abs(nz) * APrxMedRcpF1(max(max(max(bL, dL), eL), max(fL, hL)) - min(min(min(bL, dL), eL), min(fL, hL))));
  nz = -0.5 * nz + 1.0;
  // Min and max of ring.
  let mn4 = min(min(min(b, d), f), h);
  let mx4 = max(max(max(b, d), f), h);
  // Immediate constants for peak range.
  let peakC = vec2f(1.0, -1.0 * 4.0);
  // Limiters, these need to be high precision RCPs. A side that cannot clip
  // (0 * inf where the ring's max is 0, 0 / 0 where its min is 1) is dropped,
  // as the GPU's NaN-dropping max() does in ffx_fsr1.h.
  let hitMin = select(min(mn4, e) * (1.0 / (4.0 * mx4)), vec3f(3.0e38), mx4 <= vec3f(0.0));
  let hitMax = select((peakC.x - max(mx4, e)) * (1.0 / (4.0 * mn4 + peakC.y)), vec3f(-3.0e38), mn4 >= vec3f(1.0));
  let lobeRGB = max(-hitMin, hitMax);
  var lobe = max(-FSR_RCAS_LIMIT, min(max(max(lobeRGB.r, lobeRGB.g), lobeRGB.b), 0.0)) * u.con.x;
  // Apply noise removal.
  lobe *= select(1.0, nz, u.con.y > 0.5);
  // Resolve, which needs the medium precision rcp approximation to avoid visible tonality changes.
  let rcpL = APrxMedRcpF1(4.0 * lobe + 1.0);
  return vec4f((lobe * b + lobe * d + lobe * h + lobe * f + e) * rcpL, 1.0);
}`;

/** The copy pass of input variant "copy": the frame's viewport into an rgba8unorm texture of its size. */
export const COPY_WGSL = `
@group(0) @binding(0) var src: texture_external;
${VS}
@fragment fn fs(@builtin(position) pos: vec4f) -> @location(0) vec4f {
  return vec4f(textureLoad(src, vec2i(floor(pos.xy))).rgb, 1.0);
}`;
