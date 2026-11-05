// Security view (#/security): paired devices with a two-press revoke, the
// pair-a-device flow (pairing URL and QR), admin token rotation behind a
// typed confirmation, the audit log (filter by actor and time, newest
// first, paged) and the doctor checklist. Since phase 4 it lives inside
// Settings (the Security subsection); #/security redirects there.
//
// It also exports doctorCard, the compact ok/warn/fail card the Monitor
// view shows. Nothing here polls: data loads on mount, on a button and when
// the tab becomes visible again.

import { api, post } from '../api.js';
import { h, clear, openDialog, toast, errText, pill } from '../dom.js';
import { relTime } from '../util.js';
import { store } from '../store.js';
import { auditUserOptions, normUsers } from '../users.js';
import {
  doctorCountsOf,
  AUDIT_RANGES, ACTOR_KINDS, auditQuery, auditPage, routeLabel, statusTone, actorTone,
  normDevices, deviceNameError, normPairing, otherPairURLs, canPair, nextPress,
  ROTATE_WORD, rotateReady, normRotate,
  normDoctor, doctorCounts, doctorTone, STATUS_TONE,
  qrMatrix, qrText, qrImage, qrPath,
} from '../security.js';

const SVG = 'http://www.w3.org/2000/svg';
const fmtTime = (ms) => (ms ? new Date(ms).toLocaleString() : '');
const iso = (ms) => (ms ? new Date(ms).toISOString() : '');

// copyText copies to the clipboard with a toast either way.
async function copyText(text, what) {
  try {
    await navigator.clipboard.writeText(text);
    toast(`${what} copied`, 'ok');
  } catch {
    toast('Copy failed: select the text and copy it by hand', 'warn');
  }
}

// ---------- doctor (shared with Monitor) ----------

function checkRow(c) {
  return h('li', { class: `doc-row tone-${STATUS_TONE[c.status]}` },
    h('div', { class: 'doc-top' },
      pill(c.status, STATUS_TONE[c.status]),
      h('span', { class: 'doc-title', text: c.title })),
    c.detail ? h('p', { class: 'doc-detail', text: c.detail }) : null,
    c.status !== 'ok' && c.fix ? h('div', { class: 'doc-fix' },
      h('span', { class: 'field-label', text: 'Fix' }),
      h('code', { class: 'doc-fix-text', text: c.fix }),
      h('button', { class: 'btn btn-ghost btn-xs', type: 'button', 'aria-label': `Copy fix for ${c.title}`, onclick: () => copyText(c.fix, 'Fix') }, 'Copy')) : null);
}

export function doctorList(checks) {
  if (!checks.length) return h('p', { class: 't-sec', text: 'No checks reported.' });
  return h('ul', { class: 'doc-list' }, checks.map(checkRow));
}

function countsLine(n) {
  return h('span', { class: 'doc-counts' },
    h('span', { class: 'doc-n ok' }, h('b', { text: String(n.ok) }), ' ok'),
    h('span', { class: 'doc-n warn' }, h('b', { text: String(n.warn) }), ' warn'),
    h('span', { class: 'doc-n err' }, h('b', { text: String(n.fail) }), ' fail'));
}

// doctorCard is the Monitor card: counts, tap for the full list. It loads
// once when mounted and again when the tab returns. Returns {el, unmount}.
export function doctorCard() {
  let checks = null;
  let gone = false;
  const body = h('span', { class: 'doc-card-body' }, h('span', { class: 't-sec', text: 'Checking…' }));
  const el = h('button', { class: 'doc-card', type: 'button', 'aria-label': 'Doctor checks: open details', onclick: () => open() },
    h('span', { class: 'group-title doc-card-title', text: 'Doctor' }), body, h('span', { class: 'doc-card-go', 'aria-hidden': 'true', text: '›' }));

  async function load() {
    try {
      const raw = await api('/api/doctor');
      if (gone) return;
      checks = normDoctor(raw);
      // Viewers and monitors get only the counts ({counts, checks: []}).
      const n = doctorCountsOf(raw, checks);
      if (!checks.length && !store.can('doctor')) checks = null;
      el.className = `doc-card tone-${doctorTone(n)}`;
      clear(body).append(countsLine(n));
    } catch (e) {
      if (gone) return;
      checks = null;
      el.className = 'doc-card tone-info';
      clear(body).append(h('span', { class: 't-sec', text: `Unavailable: ${errText(e)}` }));
    }
  }

  function open() {
    // Counts only (viewer, monitor): nothing more to show.
    if (!checks && !store.can('doctor')) {
      toast('The full checklist is for admins.', 'info');
      return;
    }
    openDialog((close) => ({
      title: 'Doctor',
      sub: checks ? `${checks.length} checks` : 'Not loaded',
      body: checks ? doctorList(checks) : h('p', { class: 't-sec', text: 'The checks could not be loaded.' }),
      foot: [
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => { close(); load(); } }, 'Run again'),
        store.can('settings') ? h('a', { class: 'btn btn-primary', href: '#/security', onclick: () => close() }, 'Security') : null,
      ],
    }));
  }

  const onVisible = () => load();
  window.addEventListener('cerberus:visible', onVisible);
  load();
  return { el, unmount: () => { gone = true; window.removeEventListener('cerberus:visible', onVisible); } };
}

// ---------- QR ----------

function qrNode(qr, url) {
  const img = qrImage(qr);
  if (img) return h('img', { class: 'qr-img', src: img, alt: 'Pairing QR code' });
  const m = qrMatrix(qr);
  if (m) {
    const { d, size } = qrPath(m);
    const svg = document.createElementNS(SVG, 'svg');
    svg.setAttribute('viewBox', `0 0 ${size} ${size}`);
    svg.setAttribute('class', 'qr-svg');
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', 'Pairing QR code');
    svg.setAttribute('shape-rendering', 'crispEdges');
    const bg = document.createElementNS(SVG, 'rect');
    bg.setAttribute('width', String(size));
    bg.setAttribute('height', String(size));
    bg.setAttribute('fill', '#fff');
    const p = document.createElementNS(SVG, 'path');
    p.setAttribute('d', d);
    p.setAttribute('fill', '#000');
    svg.append(bg, p);
    return svg;
  }
  const text = qrText(qr);
  if (text) {
    // Terminal-style art: which colour is "dark" depends on the encoder,
    // so let the person flip it if their camera does not read it.
    const pre = h('pre', { class: 'qr-ascii', 'aria-label': 'Pairing QR code (text)', text });
    return h('div', { class: 'qr-ascii-wrap' }, pre,
      h('button', { class: 'btn btn-ghost btn-xs', type: 'button', onclick: () => pre.classList.toggle('inverted') }, 'Invert colours'));
  }
  return h('p', { class: 't-sec', text: url ? 'No QR code returned: open the link on the device instead.' : 'No QR code returned.' });
}

// ---------- pairing (Security and Account) ----------

// openPairDialog pairs a new device for the signed-in person (body may
// carry "user" when the admin token pairs for someone). onPaired(p) runs
// after the server made it.
export function openPairDialog(onPaired = () => {}, body = {}) {
  let closed = false;
  return openDialog((close) => {
    const name = h('input', { class: 'input', placeholder: 'e.g. phone', maxlength: '40', autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off' });
    const err = h('p', { class: 'field-hint secret-err' });
    const go = h('button', { class: 'btn btn-primary', type: 'submit' }, 'Pair');
    const wrap = h('div', {});
    const form = h('form', { class: 'task-form', onsubmit: async (e) => {
      e.preventDefault();
      const problem = deviceNameError(name.value);
      if (problem) { err.textContent = problem; return; }
      go.disabled = true;
      try {
        const p = normPairing(await post('/api/devices', { ...body, name: name.value.trim() }));
        if (closed) return;
        clear(wrap).append(pairResult(p, close));
        onPaired(p);
      } catch (e2) {
        err.textContent = errText(e2);
        go.disabled = false;
      }
    } },
    h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Device name' }), name),
    h('p', { class: 'field-hint', text: 'Shown in this list and in the audit log as owner:<name>.' }),
    err,
    h('div', { class: 'form-actions' },
      h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'), go));
    wrap.append(form);
    setTimeout(() => name.focus(), 0);
    return { title: 'Pair a device', sub: 'A new token for one device', body: wrap };
  }).then((v) => { closed = true; return v; });
}

function pairResult(p, close) {
  const others = otherPairURLs(p);
  const urlBox = h('input', { class: 'input mono pair-url', readonly: true, value: p.url, 'aria-label': 'Pairing link', onfocus: (e) => e.target.select() });
  return h('div', { class: 'pair-result' },
    h('div', { class: 'alert warn', text: 'This link holds the device’s token and is shown only now. Anyone who has it can use Cerberus as you until you revoke the device. Do not paste it in chats.' }),
    h('div', { class: 'qr-box' }, qrNode(p.qr, p.url)),
    h('p', { class: 'field-hint', text: `Scan it with ${p.name || 'the device'}’s camera, or open the link there.` }),
    p.url ? h('div', { class: 'pair-url-row' }, urlBox,
      h('button', { class: 'btn', type: 'button', onclick: () => copyText(p.url, 'Link') }, 'Copy')) : null,
    others.length ? h('details', { class: 'pair-others' },
      h('summary', { text: 'Other addresses (' + others.length + ')' }),
      h('p', { class: 'field-hint', text: 'If the device cannot open the link above, try one it can reach.' }),
      ...others.map((u) => h('div', { class: 'pair-url-row' },
        h('input', { class: 'input mono pair-url', readonly: true, value: u, 'aria-label': 'Other pairing link', onfocus: (e) => e.target.select() }),
        h('button', { class: 'btn', type: 'button', onclick: () => copyText(u, 'Link') }, 'Copy')))) : null,
    h('div', { class: 'form-actions' }, h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close() }, 'Done')));
}

// ---------- view ----------

// mount renders the Security panel. Settings embeds it (opts.embedded:
// no page title, Settings draws its own); #/security itself now redirects
// to #/settings/security.
export function mount(root, _route, opts = {}) {
  const embedded = !!opts.embedded;
  const state = { devices: [], range: '24h', actor: '', user: '', pages: 1, armed: new Map() };
  const me = store.me();
  // Users and roles: names on devices and the audit's user filter, once
  // accounts exist.
  const multi = !!(me && me.hasUsers && store.can('users'));
  let gone = false;

  const devicesEl = h('div', { class: 'dev-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const doctorEl = h('div', { class: 'sec-doctor' }, h('p', { class: 't-sec', text: 'Checking…' }));
  const doctorSum = h('span', { class: 'sec-sum' });
  const auditEl = h('div', { class: 'aud-list' }, h('p', { class: 't-sec', text: 'Loading…' }));
  const moreBtn = h('button', { class: 'btn', type: 'button', hidden: true, onclick: () => { state.pages += 1; loadAudit(); } }, 'Older entries');

  const rangeChips = h('div', { class: 'fchips aud-ranges', role: 'group', 'aria-label': 'Time window' }, AUDIT_RANGES.map((r) =>
    h('button', { class: 'fchip', type: 'button', 'data-range': r.key, 'aria-pressed': String(r.key === state.range), onclick: () => setRange(r.key) }, r.label)));
  const actorSel = h('select', { class: 'select fchip-select', 'aria-label': 'Actor', onchange: (e) => { state.actor = e.target.value; actorInput.value = ''; state.pages = 1; loadAudit(); } },
    ACTOR_KINDS.map(([v, t]) => h('option', { value: v, text: t })));
  const userSel = h('select', { class: 'select fchip-select', 'aria-label': 'User', hidden: !multi,
    onchange: (e) => { state.user = e.target.value; state.pages = 1; loadAudit(); } },
  auditUserOptions([]).map(([v, t]) => h('option', { value: v, text: t })));
  const actorInput = h('input', {
    class: 'input input-sm aud-actor', type: 'search', placeholder: 'e.g. owner:phone', autocapitalize: 'off', spellcheck: 'false',
    'aria-label': 'Exact actor',
    onchange: (e) => { state.actor = e.target.value.trim() || actorSel.value; state.pages = 1; loadAudit(); },
  });

  const pairBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => pair() }, '+ Pair a device');
  const pairHint = h('p', { class: 'field-hint sec-hint', hidden: true,
    text: 'Pairing needs the admin token: pair from the desktop where you signed in with it, or run cerberus pair on the host.' });

  root.append(h('section', { class: embedded ? 'sec sec-embedded' : 'view view-pad sec' },
    embedded ? null : h('div', { class: 'view-head' },
      h('div', {}, h('h1', { class: 'view-title', text: 'Security' }),
        h('p', { class: 'view-sub', text: 'Who can reach this host, what they did, and what to fix.' }))),

    h('div', { class: 'sec-head' }, h('h2', { class: 'group-title', text: 'Devices' }), pairBtn),
    h('p', { class: 'field-hint sec-hint', text: 'Each paired device has its own token. Revoking one signs it out at once, live streams included.' }),
    pairHint,
    devicesEl,

    h('div', { class: 'sec-head' }, h('h2', { class: 'group-title', text: 'Admin token' })),
    h('div', { class: 'sec-card' },
      h('p', { class: 'dlg-msg', text: 'The admin token opens everything. Rotate it if it may have leaked: browsers signed in with it and any script that uses it stop working, and running agents keep only model access until they end. Paired devices are not affected.' }),
      h('div', { class: 'form-actions' }, h('button', { class: 'btn btn-danger', type: 'button', onclick: () => rotate() }, 'Rotate token…'))),

    h('div', { class: 'sec-head' }, h('h2', { class: 'group-title', text: 'Doctor' }), doctorSum,
      h('button', { class: 'btn', type: 'button', onclick: () => loadDoctor() }, 'Run again')),
    doctorEl,

    h('div', { class: 'sec-head' }, h('h2', { class: 'group-title', text: 'Audit log' }),
      h('button', { class: 'btn', type: 'button', onclick: () => loadAudit() }, 'Refresh')),
    h('div', { class: 'aud-tools' }, rangeChips, h('div', { class: 'aud-actor-row' }, userSel, actorSel, actorInput)),
    auditEl,
    h('div', { class: 'aud-more' }, moreBtn)));

  // ---------- devices ----------
  async function loadDevices() {
    try {
      const raw = await api('/api/devices');
      if (gone) return;
      state.devices = normDevices(raw);
      const may = canPair(raw);
      pairBtn.hidden = !may;
      pairHint.hidden = may;
      renderDevices();
    } catch (e) {
      if (!gone) clear(devicesEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  function renderDevices() {
    clear(devicesEl);
    if (!state.devices.length) {
      devicesEl.append(h('div', { class: 'empty' },
        h('p', { class: 'empty-title', text: 'No paired devices' }),
        h('p', { class: 't-sec', text: 'Pair your phone or laptop so it has its own token you can revoke.' })));
      return;
    }
    devicesEl.append(...state.devices.map((d) => {
      const btn = h('button', { class: 'btn btn-danger', type: 'button', 'aria-label': `Revoke ${d.name}` }, 'Revoke');
      btn.addEventListener('click', () => pressRevoke(d, btn));
      return h('div', { class: 'dev-row' },
        h('div', { class: 'dev-main' },
          h('div', { class: 'dev-name' }, h('span', { text: d.name }), d.current ? pill('this device', 'ok') : null,
            d.client ? pill('MCP client', 'info') : null,
            multi && (d.userName || d.user) ? h('span', { class: 'owner-chip', title: 'Acts for this user', text: d.userName || d.user }) : null),
          h('div', { class: 'sess-meta' },
            d.created ? h('span', { title: fmtTime(d.created), text: `paired ${relTime(iso(d.created))}` }) : null,
            h('span', { title: fmtTime(d.lastSeen), text: d.lastSeen ? `seen ${relTime(iso(d.lastSeen))}` : 'never used' }),
            d.lastAddr ? h('span', { class: 'mono', text: d.lastAddr }) : null)),
        h('div', { class: 'dev-actions' }, btn));
    }));
  }

  // Two presses: the first arms the button for 4 s, the second revokes.
  function pressRevoke(d, btn) {
    const now = Date.now();
    const { fire, armedAt } = nextPress(state.armed.get(d.id) || 0, now);
    clearTimeout(btn._t);
    if (!fire) {
      state.armed.set(d.id, armedAt);
      btn.textContent = d.current ? 'Revoke this device?' : 'Press again to revoke';
      btn.classList.add('armed');
      btn._t = setTimeout(() => { state.armed.delete(d.id); btn.textContent = 'Revoke'; btn.classList.remove('armed'); }, 4000);
      return;
    }
    state.armed.delete(d.id);
    btn.disabled = true;
    btn.textContent = 'Revoking…';
    api(`/api/devices/${encodeURIComponent(d.id)}`, { method: 'DELETE' })
      .then(() => toast(`${d.name} revoked`, 'ok'), (e) => toast(errText(e), 'err'))
      .finally(() => { if (!gone) { loadDevices(); loadAudit(); } });
  }

  async function pair() {
    await openPairDialog(() => { if (!gone) { loadDevices(); loadAudit(); } });
  }

  // ---------- token rotation ----------
  async function rotate() {
    const result = await openDialog((close) => {
      const typed = h('input', { class: 'input mono', placeholder: ROTATE_WORD, autocapitalize: 'characters', spellcheck: 'false', autocomplete: 'off', 'aria-label': `Type ${ROTATE_WORD} to confirm` });
      const go = h('button', { class: 'btn btn-danger', type: 'submit', disabled: true }, 'Rotate now');
      const err = h('p', { class: 'field-hint secret-err' });
      typed.addEventListener('input', () => { go.disabled = !rotateReady(typed.value); });
      const form = h('form', { class: 'task-form', onsubmit: async (e) => {
        e.preventDefault();
        if (!rotateReady(typed.value)) return;
        go.disabled = true;
        try {
          close({ ok: true, ...normRotate(await post('/api/token/rotate', {})) });
        } catch (e2) {
          err.textContent = errText(e2);
          go.disabled = false;
        }
      } },
      h('ul', { class: 'rot-list' },
        h('li', { text: 'Every browser and script using the admin token is signed out, maybe this one too.' }),
        h('li', { text: 'The config file gets the new token; desktop clients reading it pick it up.' }),
        h('li', { text: 'Running agents keep model access only until they end; new runs get new tokens.' }),
        h('li', { text: 'Paired devices keep working.' })),
      h('label', { class: 'field' }, h('span', { class: 'field-label', text: `Type ${ROTATE_WORD} to confirm` }), typed),
      err,
      h('div', { class: 'form-actions' },
        h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => close() }, 'Cancel'), go));
      setTimeout(() => typed.focus(), 0);
      return { title: 'Rotate admin token', sub: 'This cannot be undone', body: form };
    });
    if (!result || !result.ok) return;
    toast('Admin token rotated', 'ok');
    if (result.token || result.url) {
      await openDialog((close) => ({
        title: 'New admin token',
        sub: 'Shown only now',
        body: h('div', { class: 'pair-result' },
          h('div', { class: 'alert warn', text: 'Store it somewhere safe (a password manager). It is also in the config file on the host.' }),
          result.token ? h('div', { class: 'pair-url-row' },
            h('input', { class: 'input mono pair-url', readonly: true, value: result.token, 'aria-label': 'New admin token', onfocus: (e) => e.target.select() }),
            h('button', { class: 'btn', type: 'button', onclick: () => copyText(result.token, 'Token') }, 'Copy')) : null,
          result.url ? h('div', { class: 'pair-url-row' },
            h('input', { class: 'input mono pair-url', readonly: true, value: result.url, 'aria-label': 'Sign-in link', onfocus: (e) => e.target.select() }),
            h('button', { class: 'btn', type: 'button', onclick: () => copyText(result.url, 'Link') }, 'Copy')) : null),
        foot: h('button', { class: 'btn btn-primary', type: 'button', onclick: () => close() }, 'Done'),
      }));
    }
    // A 401 here (this browser used the old token) switches to Login.
    if (!gone) { loadDevices(); loadAudit(); }
  }

  // ---------- doctor ----------
  async function loadDoctor() {
    clear(doctorSum);
    try {
      const checks = normDoctor(await api('/api/doctor'));
      if (gone) return;
      clear(doctorSum).append(countsLine(doctorCounts(checks)));
      clear(doctorEl).append(doctorList(checks));
    } catch (e) {
      if (!gone) clear(doctorEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  // ---------- audit ----------
  function setRange(key) {
    state.range = key;
    state.pages = 1;
    for (const b of rangeChips.querySelectorAll('[data-range]')) b.setAttribute('aria-pressed', String(b.dataset.range === key));
    loadAudit();
  }

  let auditSeq = 0;
  async function loadAudit() {
    const seq = ++auditSeq;
    moreBtn.disabled = true;
    try {
      const raw = await api(auditQuery({ rangeKey: state.range, actor: state.actor, user: state.user, pages: state.pages }));
      if (gone || seq !== auditSeq) return;
      const { rows, more } = auditPage(raw, state.pages);
      renderAudit(rows);
      moreBtn.hidden = !more;
    } catch (e) {
      if (!gone && seq === auditSeq) { clear(auditEl).append(h('div', { class: 'alert err', text: errText(e) })); moreBtn.hidden = true; }
    } finally {
      moreBtn.disabled = false;
    }
  }

  function renderAudit(rows) {
    clear(auditEl);
    if (!rows.length) {
      auditEl.append(h('p', { class: 't-sec aud-empty', text: 'Nothing recorded in this window.' }));
      return;
    }
    auditEl.append(h('ol', { class: 'aud-rows' }, rows.map((r) => h('li', { class: `aud-row tone-${statusTone(r.status)}` },
      h('div', { class: 'aud-top' },
        h('time', { class: 'aud-time', datetime: iso(r.time), title: fmtTime(r.time), text: r.time ? relTime(iso(r.time)) : '?' }),
        h('button', {
          class: `pill ${actorTone(r.kind)} aud-actor-pill`, type: 'button', title: `Show only ${r.actor}`,
          onclick: () => { actorInput.value = r.actor; state.actor = r.actor; state.pages = 1; loadAudit(); },
        }, r.actor || 'system'),
        r.user ? h('button', {
          class: 'owner-chip aud-user', type: 'button', title: `Show only ${r.user}`,
          onclick: () => { state.user = r.user; if ([...userSel.options].some((o) => o.value === r.user)) userSel.value = r.user; state.pages = 1; loadAudit(); },
        }, r.user) : null,
        r.status ? pill(String(r.status), statusTone(r.status)) : null),
      h('div', { class: 'aud-route mono' },
        r.method ? h('span', { class: 'aud-method', text: r.method }) : null,
        h('span', { text: routeLabel(r) })),
      r.targets.length || r.detail || r.remote ? h('div', { class: 'aud-meta' },
        r.targets.length ? h('span', { class: 'mono', text: r.targets.join(', ') }) : null,
        r.detail ? h('span', { class: 'aud-detail', text: r.detail }) : null,
        r.remote ? h('span', { class: 'mono t-muted', text: r.remote }) : null) : null))));
  }

  // The audit's user filter lists the accounts (admins only).
  async function loadUsers() {
    if (!multi) return;
    try {
      const { users } = normUsers(await api('/api/users'));
      if (gone) return;
      const keep = state.user;
      clear(userSel).append(...auditUserOptions(users).map(([v, t]) => h('option', { value: v, text: t })));
      if (keep && ![...userSel.options].some((o) => o.value === keep)) userSel.append(h('option', { value: keep, text: keep }));
      userSel.value = keep;
    } catch { /* the filter keeps its two fixed choices */ }
  }

  function loadAll() {
    loadUsers();
    loadDevices();
    loadDoctor();
    loadAudit();
  }
  const onVisible = () => loadAll();
  window.addEventListener('cerberus:visible', onVisible);
  loadAll();
  return () => {
    gone = true;
    window.removeEventListener('cerberus:visible', onVisible);
  };
}
