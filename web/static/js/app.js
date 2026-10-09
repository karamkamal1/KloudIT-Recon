import { api, me, el, toast, capabilities, setCSRF } from './api.js';

const $ = (id) => document.getElementById(id);
let user = null;

const ICON_PC = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/></svg>';

function ago(ts) {
  if (!ts || ts.startsWith('0001')) return 'never';
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

const encoderLabel = (e) => ({
  h264_nvenc: 'NVENC H.264', hevc_nvenc: 'NVENC HEVC', av1_nvenc: 'NVENC AV1',
  h264_amf: 'AMF H.264', hevc_amf: 'AMF HEVC', av1_amf: 'AMF AV1',
  h264_qsv: 'QSV H.264', hevc_qsv: 'QSV HEVC', av1_qsv: 'QSV AV1',
  libx264: 'x264 (CPU)', libsvtav1: 'SVT-AV1 (CPU)', 'libaom-av1': 'AOM AV1 (CPU)',
}[e] || e);

// ---------------------------------------------------------------------------
// Modals

// A label and the input it names (for assistive technology: a label next to
// its input names nothing).
let ids = 0;
function field(label, input) {
  input.id ||= `field-${++ids}`;
  return [el('label', { for: input.id }, label), input];
}

// The dialog is named by its title.
function modal(title, body, actions = []) {
  const root = $('modal-root');
  const close = () => { bg.remove(); document.removeEventListener('keydown', esc); };
  const esc = (e) => { if (e.key === 'Escape') close(); };
  const titleId = `modal-title-${++ids}`;
  const bg = el('div', { class: 'modal-bg', onclick: (e) => { if (e.target === bg) close(); } },
    el('div', { class: 'card modal', role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId },
      el('h3', { id: titleId }, title), body,
      el('div', { class: 'modal-actions' }, ...actions.map((a) => el('button', { class: a.primary ? 'btn-primary' : a.danger ? 'btn-danger' : '', onclick: () => a.run(close) }, a.label)),
        el('button', { onclick: close }, actions.length ? 'Cancel' : 'Close'))));
  document.addEventListener('keydown', esc);
  root.append(bg);
  const first = bg.querySelector('input,textarea,select');
  if (first) first.focus();
  return close;
}

function copyBtn(text, label = 'Copy') {
  return el('button', { class: 'btn-sm', onclick: async (e) => {
    try { await navigator.clipboard.writeText(text); e.target.textContent = 'Copied ✓'; } catch { toast('Copy failed — select the text manually', 'warn'); }
  } }, label);
}

function pairingModal(name, code, id) {
  const install = `powershell -ExecutionPolicy Bypass -File .\\install-host.ps1 -PairingCode "${code}" -InstallViGEm`;
  const pairOnly = `& "$env:ProgramFiles\\KlouditRecon\\recon-host.exe" pair "${code}"`;
  const status = el('p', { class: 'status' }, el('span', { class: 'dot' }), 'Waiting for the PC to connect…');
  const close = modal(`Pair “${name}”`, el('div', {},
    el('p', { class: 'hint' }, 'On the Windows PC, signed in as the user who plays: open PowerShell as administrator, ',
      el('code', {}, 'cd'), ' into the unzipped ', el('code', {}, 'host-windows-amd64'), ' folder and paste this. It installs FFmpeg, ',
      'the controller driver and a logon task, pairs, and starts the agent:'),
    el('div', { class: 'code-box' }, install), el('div', { class: 'modal-actions' }, copyBtn(install), copyBtn(code, 'Copy code only')),
    el('p', { class: 'hint' }, 'Agent already installed (re-pairing)? Paste this instead; the running agent picks it up:'),
    el('div', { class: 'code-box' }, pairOnly), el('div', { class: 'modal-actions' }, copyBtn(pairOnly)),
    el('p', { class: 'hint' }, 'The code contains a secret that lets the PC register with this gateway — it is shown once. You can re-pair later to rotate it.'),
    status,
  ));
  // Watch for the agent to come online and confirm.
  const t = setInterval(async () => {
    if (!document.body.contains(status)) { clearInterval(t); return; }
    try {
      const { hosts } = await api('GET', '/api/hosts');
      if ((hosts || []).some((h) => h.id === id && h.online)) {
        clearInterval(t);
        status.replaceChildren(el('span', { class: 'dot on' }), `${name} is connected ✓`);
        setTimeout(() => { close(); loadHosts(); }, 1200);
      }
    } catch {}
  }, 1500);
}

// ---------------------------------------------------------------------------
// Hosts

// Every 5 s and after a wake. The cards are replaced only when what they show
// changed, and then the focus goes back to the same control of the same host
// (data-focus): a removed element drops the focus to the page, so keyboard
// and screen-reader users lost their place in the list at every refresh.
async function loadHosts() {
  const { hosts } = await api('GET', '/api/hosts');
  const box = $('hosts');
  const next = el('div', {});
  for (const h of hosts || []) next.append(hostCard(h));
  if (user.admin) {
    next.append(el('div', { class: 'card host add-host', role: 'button', tabindex: '0', 'data-focus': 'add', onclick: addHost, onkeydown: (e) => e.key === 'Enter' && addHost() },
      el('div', {}, '+ Add a PC')));
  } else if (!hosts || !hosts.length) {
    next.append(el('div', { class: 'card empty' }, 'No machines yet. Ask an administrator to add one.'));
  }
  if (next.innerHTML === box.innerHTML) return;
  const focus = box.contains(document.activeElement) ? document.activeElement.dataset.focus : null;
  box.replaceChildren(...next.childNodes);
  if (focus) [...box.querySelectorAll('[data-focus]')].find((x) => x.dataset.focus === focus)?.focus();
}

function hostCard(h) {
  const card = el('div', { class: `card host ${h.online ? 'online' : ''}` });
  const icon = el('div', { class: 'host-icon' });
  icon.innerHTML = ICON_PC;
  const state = h.streaming ? ['busy', `Streaming${h.user ? ' · ' + h.user : ''}`] : h.online ? ['on', 'Online'] : ['', `Offline · seen ${ago(h.lastSeen)}`];
  const badges = el('div', { class: 'badges' });
  const encs = (h.encoders || []).filter((e) => !e.startsWith('lib') || !(h.encoders || []).some((x) => !x.startsWith('lib')));
  for (const e of encs.slice(0, 4)) badges.append(el('span', { class: `badge ${e.startsWith('lib') ? '' : 'accent'}` }, encoderLabel(e)));
  if (h.direct) badges.append(el('span', { class: 'badge good', title: 'The browser can connect straight to the PC (no relay hop)' }, 'direct path'));
  for (const m of (h.monitors || []).slice(0, 2)) badges.append(el('span', { class: 'badge' }, `${m.w}×${m.h}${m.hz ? '@' + m.hz : ''}`));

  const actions = el('div', { class: 'host-actions' });
  if (h.online) {
    actions.append(el('a', { class: 'btn btn-primary', href: `/stream?host=${encodeURIComponent(h.id)}`, 'data-focus': `${h.id}:main` }, '▶  Connect'));
  } else if (h.canWake) {
    actions.append(el('button', { class: 'btn-primary', 'data-focus': `${h.id}:main`, onclick: () => wake(h) }, '⏻  Wake PC'));
  } else {
    actions.append(el('button', { class: 'btn-primary', disabled: true }, 'Offline'));
  }
  if (user.admin) {
    actions.append(el('button', { class: 'btn-icon', title: 'Manage', 'aria-label': 'Manage', 'data-focus': `${h.id}:manage`, onclick: () => manageHost(h) }, '⋯'));
  }
  card.append(
    el('div', { class: 'host-top' }, icon, el('div', {},
      el('div', { class: 'host-name' }, h.name),
      el('div', { class: 'status' }, el('span', { class: `dot ${state[0]}` }), state[1]))),
    el('div', { class: 'host-meta' }, [h.os, h.version && `agent ${h.version}`, h.ip].filter(Boolean).join(' · ') || 'Not paired yet'),
    badges, actions);
  return card;
}

async function wake(h) {
  try {
    await api('POST', `/api/hosts/${h.id}/wake`, {});
    toast(`Magic packet sent to ${h.name}. It usually takes 10–30 s to come online.`, 'ok', 6000);
    let n = 0;
    const t = setInterval(async () => { await loadHosts(); if (++n > 20) clearInterval(t); }, 3000);
  } catch (e) { toast(e.message, 'error'); }
}

function addHost() {
  const name = el('input', { type: 'text', placeholder: 'Gaming PC', maxlength: '64' });
  modal('Add a PC', el('div', {}, field('Name', name)), [{
    label: 'Create pairing code', primary: true,
    run: async (close) => {
      try {
        const r = await api('POST', '/api/hosts', { name: name.value || 'Gaming PC' });
        close();
        pairingModal(r.name, r.pairingCode, r.id);
        loadHosts();
      } catch (e) { toast(e.message, 'error'); }
    },
  }]);
}

function manageHost(h) {
  const name = el('input', { type: 'text', value: h.name, maxlength: '64' });
  modal(`Manage ${h.name}`, el('div', {},
    field('Name', name),
    el('p', { class: 'hint' }, 'Re-pairing issues a new secret and disconnects the current agent until you run the new pairing command on the PC.')), [
    { label: 'Remove', danger: true, run: async (close) => {
      if (!confirm(`Remove ${h.name}? The agent will be disconnected.`)) return;
      await api('DELETE', `/api/hosts/${h.id}`); close(); loadHosts();
    } },
    { label: 'Re-pair', run: async (close) => {
      const r = await api('POST', `/api/hosts/${h.id}/repair`, {}); close(); pairingModal(r.name, r.pairingCode, r.id); loadHosts();
    } },
    { label: 'Save', primary: true, run: async (close) => {
      try { await api('POST', `/api/hosts/${h.id}/rename`, { name: name.value }); close(); loadHosts(); } catch (e) { toast(e.message, 'error'); }
    } },
  ]);
}

// ---------------------------------------------------------------------------
// Account

function accountModal() {
  const cur = el('input', { type: 'password', autocomplete: 'current-password' });
  const nw = el('input', { type: 'password', autocomplete: 'new-password', minlength: '10' });
  const totpBox = el('div', {});
  const renderTOTP = () => {
    totpBox.replaceChildren();
    if (user.totp) {
      const pw = el('input', { type: 'password', placeholder: 'Password to confirm', 'aria-label': 'Password to confirm' });
      totpBox.append(el('p', { class: 'hint' }, '✅ Two-factor authentication is on.'), pw,
        el('div', { class: 'modal-actions' }, el('button', { class: 'btn-danger btn-sm', onclick: async () => {
          try { await api('POST', '/api/me/totp/disable', { password: pw.value }); user.totp = false; renderTOTP(); toast('2FA disabled', 'warn'); } catch (e) { toast(e.message, 'error'); }
        } }, 'Disable 2FA')));
    } else {
      totpBox.append(el('p', { class: 'hint' }, 'Protect your account with an authenticator app (TOTP).'),
        el('button', { class: 'btn-sm', onclick: async () => {
          const r = await api('POST', '/api/me/totp/begin', {});
          const code = el('input', { type: 'text', inputmode: 'numeric', maxlength: '6', placeholder: '123456' });
          totpBox.replaceChildren(
            el('img', { class: 'qr', src: r.qr, alt: 'TOTP QR code' }),
            el('p', { class: 'hint' }, 'Scan with your authenticator, or enter the key manually:'),
            el('div', { class: 'code-box' }, r.secret), ...field('Code from the app', code),
            el('div', { class: 'modal-actions' }, el('button', { class: 'btn-primary btn-sm', onclick: async () => {
              try { await api('POST', '/api/me/totp/enable', { secret: r.secret, code: code.value }); user.totp = true; renderTOTP(); toast('2FA enabled', 'ok'); } catch (e) { toast(e.message, 'error'); }
            } }, 'Enable')));
        } }, 'Set up 2FA'));
    }
  };
  renderTOTP();
  modal(`Account · ${user.username}`, el('div', {},
    el('div', { class: 'gtitle hint' }, 'Two-factor authentication'), totpBox,
    el('hr', { class: 'sep' }),
    field('Current password', cur), field('New password (10+ characters)', nw),
    el('div', { class: 'modal-actions' }, el('button', { class: 'btn-sm', onclick: async () => {
      try { await api('POST', '/api/me/password', { current: cur.value, new: nw.value }); cur.value = nw.value = ''; toast('Password changed — other sessions were signed out', 'ok'); } catch (e) { toast(e.message, 'error'); }
    } }, 'Change password'))));
}

async function usersModal() {
  const { users } = await api('GET', '/api/users');
  const rows = el('tbody', {}, ...(users || []).map((u) => el('tr', {},
    el('td', {}, u.username), el('td', {}, u.admin ? 'admin' : 'user'), el('td', {}, u.totp ? 'on' : '—'), el('td', {}, ago(u.lastLogin)),
    el('td', {}, u.username === user.username ? '' : el('button', { class: 'btn-sm btn-danger', onclick: async () => {
      if (!confirm(`Delete ${u.username}?`)) return;
      await api('DELETE', `/api/users/${encodeURIComponent(u.username)}`); closeM(); usersModal();
    } }, 'Delete')))));
  const name = el('input', { type: 'text', placeholder: 'username', 'aria-label': 'New user name' });
  const pw = el('input', { type: 'password', placeholder: 'password (10+ chars)', 'aria-label': 'New user password (10+ characters)' });
  const adm = el('input', { type: 'checkbox' });
  const closeM = modal('Users', el('div', {},
    el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, 'User'), el('th', {}, 'Role'), el('th', {}, '2FA'), el('th', {}, 'Last login'), el('th', {}))), rows),
    el('label', {}, 'Add user'), name, pw,
    el('label', { class: 'check' }, adm, 'Administrator'),
  ), [{ label: 'Add user', primary: true, run: async (close) => {
    try { await api('POST', '/api/users', { username: name.value, password: pw.value, admin: adm.checked }); close(); usersModal(); } catch (e) { toast(e.message, 'error'); }
  } }]);
}

async function auditModal() {
  const { entries } = await api('GET', '/api/audit');
  modal('Audit log', el('table', {},
    el('thead', {}, el('tr', {}, el('th', {}, 'Time'), el('th', {}, 'Event'), el('th', {}, 'User'), el('th', {}, 'IP'), el('th', {}, 'Detail'))),
    el('tbody', {}, ...(entries || []).map((e) => el('tr', {},
      el('td', {}, new Date(e.time).toLocaleString()), el('td', {}, e.event), el('td', {}, e.user || ''), el('td', {}, e.ip || ''), el('td', {}, e.detail || ''))))));
}

// ---------------------------------------------------------------------------

async function showCaps() {
  const c = await capabilities();
  const box = $('caps');
  const b = (ok, label, title) => el('span', { class: `badge ${ok ? 'good' : 'bad'}`, title }, `${ok ? '✓' : '✗'} ${label}`);
  box.append(
    b(c.webtransport, 'WebTransport', 'QUIC streams + datagrams (lowest latency). Falls back to WebSocket.'),
    b(c.webcodecs, 'WebCodecs', 'Hardware video decode without media-element buffering'),
    b(c.webgpu, 'WebGPU', 'Zero-copy rendering of decoded frames'),
    b(c.isolated, 'Lock-free audio', 'SharedArrayBuffer ring to the AudioWorklet'),
    b(c.rawPointer, 'Raw pointer', 'pointerrawupdate events for sub-frame mouse latency'),
    b(c.keyboardLock, 'Keyboard lock', 'Capture Esc / Alt+Tab / Win in fullscreen'));
  for (const [fam, v] of Object.entries(c.codecs)) {
    box.append(el('span', { class: `badge ${v === 'hw' ? 'good' : v ? '' : 'bad'}` }, `${fam.toUpperCase()} ${v === 'hw' ? 'HW' : v === 'sw' ? 'SW' : '✗'}`));
  }
}

async function init() {
  user = await me();
  $('version').textContent = user.version;
  if (user.admin) {
    $('add-host').classList.remove('hidden');
    $('nav-users').classList.remove('hidden');
    $('nav-audit').classList.remove('hidden');
  }
  $('add-host').onclick = addHost;
  $('nav-account').onclick = accountModal;
  $('nav-users').onclick = usersModal;
  $('nav-audit').onclick = auditModal;
  $('nav-logout').onclick = async () => { await api('POST', '/api/logout', {}).catch(() => {}); setCSRF(''); location.href = '/login'; };
  await loadHosts();
  setInterval(() => { if (!document.hidden && !document.querySelector('.modal-bg')) loadHosts().catch(() => {}); }, 5000);
  showCaps();
  const st = await api('GET', '/api/state');
  if (st.caInstalled) {
    $('ca-hint').append('Seeing certificate warnings? Install the gateway’s private CA once on this device: ',
      el('a', { href: '/ca.crt' }, 'download ca.crt'), '.');
  }
}

init().catch((e) => toast(e.message, 'error'));
