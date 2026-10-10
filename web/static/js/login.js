import { api, setCSRF } from './api.js';

const $ = (id) => document.getElementById(id);
// Where to go after signing in: a page of this gateway, else '/'. The value
// is resolved as location.replace resolves it (the URL parser reads a backslash
// as '/' and drops tabs and newlines, so '/\evil.example' names another host)
// and kept only when it stays on this origin, as the absolute URL: its path
// alone can name another host once resolved ('/.//evil.example' and
// '/%2e//evil.example' resolve to the path '//evil.example', which a relative
// redirect reads as that host), so such a path is refused too.
const next = () => {
  const n = new URLSearchParams(location.search).get('next');
  if (!n || !n.startsWith('/') || /[\\\x00-\x1f\x7f]/.test(n)) return '/';
  try {
    const u = new URL(n, location.origin);
    return u.origin === location.origin && !u.pathname.startsWith('//') ? u.href : '/';
  } catch {
    return '/';
  }
};
let pending = '';

async function init() {
  const st = await api('GET', '/api/state', undefined, { redirect: false });
  $('foot').textContent = `Recon ${st.version} · end-to-end TLS · Argon2id · TOTP`;
  if (st.setupNeeded) {
    $('setup-form').classList.remove('hidden');
    $('setup-token').focus();
    return;
  }
  try {
    const m = await api('GET', '/api/me', undefined, { redirect: false });
    setCSRF(m.csrf);
    location.replace(next());
    return;
  } catch {}
  $('login-form').classList.remove('hidden');
  $('username').focus();
}

function busy(form, on) {
  for (const b of form.querySelectorAll('button')) b.disabled = on;
}

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.currentTarget;
  $('login-error').textContent = '';
  busy(f, true);
  try {
    const r = await api('POST', '/api/login', { username: $('username').value.trim(), password: $('password').value }, { publicPost: true });
    if (r.totpRequired) {
      pending = r.pending;
      f.classList.add('hidden');
      $('totp-form').classList.remove('hidden');
      $('totp').focus();
      return;
    }
    setCSRF(r.csrf);
    location.replace(next());
  } catch (err) {
    $('login-error').textContent = err.message;
    $('password').select();
  } finally {
    busy(f, false);
  }
});

$('totp').addEventListener('input', (e) => {
  const v = e.target.value.replace(/\D/g, '').slice(0, 6);
  e.target.value = v;
  if (v.length === 6) $('totp-form').requestSubmit();
});

$('totp-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.currentTarget;
  $('totp-error').textContent = '';
  busy(f, true);
  try {
    const r = await api('POST', '/api/login/totp', { pending, code: $('totp').value }, { publicPost: true });
    setCSRF(r.csrf);
    location.replace(next());
  } catch (err) {
    $('totp-error').textContent = err.message;
    $('totp').value = '';
    if (err.status === 401 && /expired/.test(err.message)) setTimeout(() => location.reload(), 1200);
  } finally {
    busy(f, false);
  }
});

$('setup-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.currentTarget;
  $('setup-error').textContent = '';
  if ($('setup-pass').value !== $('setup-pass2').value) {
    $('setup-error').textContent = 'Passwords do not match';
    return;
  }
  busy(f, true);
  try {
    const r = await api('POST', '/api/setup', {
      setupToken: $('setup-token').value.trim(), username: $('setup-user').value.trim(), password: $('setup-pass').value,
    }, { publicPost: true });
    setCSRF(r.csrf);
    location.replace('/');
  } catch (err) {
    $('setup-error').textContent = err.message;
  } finally {
    busy(f, false);
  }
});

init().catch((e) => { $('foot').textContent = e.message; });
