// Small fetch wrapper: JSON in/out, CSRF header, redirect to login on 401.

let csrf = sessionStorage.getItem('recon.csrf') || '';

export function setCSRF(token) {
  csrf = token || '';
  if (csrf) sessionStorage.setItem('recon.csrf', csrf);
  else sessionStorage.removeItem('recon.csrf');
}

export class APIError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export async function api(method, path, body, { publicPost = false, redirect = true } = {}) {
  const headers = { 'Content-Type': 'application/json' };
  if (method !== 'GET') headers['X-Recon-CSRF'] = publicPost ? 'public' : csrf;
  const res = await fetch(path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = {};
  try { data = await res.json(); } catch {}
  if (res.status === 401 && redirect && !publicPost) {
    location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search);
    throw new APIError(401, 'not logged in');
  }
  if (!res.ok) throw new APIError(res.status, data.error || res.statusText);
  return data;
}

/** Ensure we are logged in; refreshes the CSRF token. */
export async function me() {
  const m = await api('GET', '/api/me');
  setCSRF(m.csrf);
  return m;
}

export function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (k === 'text') e.textContent = v;
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

export function toast(msg, level = 'info', ms = 4000) {
  let box = document.getElementById('toasts');
  if (!box) {
    box = el('div', { id: 'toasts', 'aria-live': 'polite' });
    document.body.append(box);
  }
  const t = el('div', { class: `toast ${level}` }, msg);
  box.append(t);
  setTimeout(() => t.classList.add('out'), ms);
  setTimeout(() => t.remove(), ms + 400);
}

/** Feature detection shown on the dashboard and stream pages. */
export async function capabilities() {
  const caps = {
    webtransport: typeof WebTransport !== 'undefined',
    webcodecs: typeof VideoDecoder !== 'undefined',
    webgpu: !!navigator.gpu,
    isolated: self.crossOriginIsolated === true,
    keyboardLock: !!(navigator.keyboard && navigator.keyboard.lock),
    rawPointer: 'onpointerrawupdate' in window,
    codecs: {},
  };
  if (caps.webcodecs) {
    for (const [fam, codec] of [['h264', 'avc1.640033'], ['hevc', 'hev1.1.6.L153.B0'], ['av1', 'av01.0.13M.08']]) {
      const q = (hardwareAcceleration) => VideoDecoder.isConfigSupported({ codec, codedWidth: 1920, codedHeight: 1080, hardwareAcceleration })
        .then((r) => r.supported).catch(() => false);
      const hw = await q('prefer-hardware');
      caps.codecs[fam] = hw ? 'hw' : (await q('no-preference')) ? 'sw' : null;
    }
  }
  return caps;
}
