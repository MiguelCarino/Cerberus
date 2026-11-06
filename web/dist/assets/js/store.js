// Shared client state: the session list (kept live by /api/events) and the
// last /api/status. Views subscribe and re-render on change.

import { api, apiResult, AuthError, LiveSource } from './api.js';
import { countWaiting } from './util.js';
import { normAsks } from './inbox.js';
import { normMe, LEGACY_ME, can, roleHome } from './users.js';

const sessions = new Map();
const listeners = new Set();
let status = null;
let events = null;
let loaded = false;
let keepHidden = () => false;
let asks = []; // open chat permission requests (GET /api/chats/asks)
let asksSeq = 0;
let me = null; // who is signed in (GET /api/me, see users.js normMe); null until loaded
let meSig = '';
let meSeq = 0;

export const store = {
  sessions: () => [...sessions.values()],
  session: (id) => sessions.get(id),
  status: () => status,
  loaded: () => loaded,
  waiting: () => countWaiting(sessions.values()),
  asks: () => asks,

  // subscribe(fn) calls fn(kind, payload) on 'sessions' | 'session' |
  // 'status', and for the day phase 6 items on the same event stream:
  // 'inbox' (an inbox event), 'download' (a download item) and 'events-open'
  // (the stream (re)connected: refetch whatever may have been missed),
  // 'resync' ({what: 'inbox'|'downloads'}: the stream fell behind) and
  // 'usage' ({session}: a gateway request charged to that session finished;
  // a refresh hint for the Flow graph) and 'flowrun' ({run, flow, state,
  // step}: a flow run or one of its steps changed; a resync with
  // what: 'flows' means some were missed), 'evalrun' ({run, eval, state,
  // session}: an eval run or one of its sessions changed; resync what:
  // 'evals') and 'asks' (the open chat
  // permission requests changed: store.asks()).
  // 'me' (the signed-in person changed: role, caps, must-change).
  subscribe(fn) { listeners.add(fn); return () => listeners.delete(fn); },

  // ---------- who is signed in (users and roles) ----------
  me: () => me,
  can: (cap) => can(me, cap),
  home: () => roleHome(me),

  // loadMe fetches GET /api/me. A daemon from before users (404) acts as
  // the admin token, as it always did. A 401 throws AuthError (Login takes
  // over); a network failure keeps what was known, or the legacy admin.
  async loadMe() {
    const seq = ++meSeq;
    let next;
    try {
      const r = await apiResult('/api/me', { quiet: true });
      if (r.ok) next = normMe(r.body);
      else if (r.status === 404 || r.status === 405) next = LEGACY_ME;
      else next = me || LEGACY_ME;
    } catch (e) {
      if (e instanceof AuthError) { if (seq === meSeq) store.clearMe(); throw e; }
      next = me || LEGACY_ME;
    }
    if (seq !== meSeq) return me;
    const sig = JSON.stringify([next.kind, next.user && next.user.id, next.role, next.caps, next.mustChange, next.bootstrap, next.hasUsers]);
    me = next;
    if (sig !== meSig) { meSig = sig; emit('me', me); }
    return me;
  },

  // clearMe forgets the signed-in person (sign out, 401).
  clearMe() { me = null; meSig = ''; meSeq++; },

  async refreshSessions() {
    const list = await api('/api/sessions');
    sessions.clear();
    for (const s of list || []) sessions.set(s.ID, s);
    loaded = true;
    emit('sessions');
  },

  async refreshStatus() {
    status = await api('/api/status');
    // the navbar's About window shows the daemon's version, not a date
    if (status && status.version && window.CarinoNav && window.CarinoNav.version) window.CarinoNav.version(status.version);
    emit('status', status);
    return status;
  },

  // refreshAsks refetches every chat's open permission requests. With the
  // chat off (403) or absent there are none. The latest call wins.
  async refreshAsks() {
    const seq = ++asksSeq;
    let list = [];
    try { list = normAsks(await api('/api/chats/asks')); } catch { list = []; }
    if (seq !== asksSeq) return asks;
    asks = list;
    emit('asks', asks);
    return asks;
  },

  upsert(info) {
    sessions.set(info.ID, info);
    emit('session', info);
  },

  // setKeepHidden(fn): while fn() is true the event stream stays open when
  // the tab is hidden (opted-in desktop notifications).
  setKeepHidden(fn) { keepHidden = fn; },

  // startEvents opens /api/events once. Each (re)connect refetches the full
  // list, so events missed while hidden or offline are never lost.
  startEvents() {
    if (events) return;
    events = new LiveSource('/api/events', {
      // Every (re)connect re-reads who is signed in too: a role change or
      // a disable cuts the stream, and the page follows the new rules.
      open: () => {
        store.loadMe().catch(() => {});
        if (can(me, 'sessions') || can(me, 'sessions.watch_all')) store.refreshSessions().catch(() => {});
        else { sessions.clear(); loaded = true; emit('sessions'); }
        if (can(me, 'chat')) store.refreshAsks();
        emit('events-open');
      },
      asks: () => { store.refreshAsks(); },
      inbox: (data) => { const ev = parseJSON(data); if (ev) emit('inbox', ev); },
      download: (data) => { const it = parseJSON(data); if (it) emit('download', it); },
      usage: (data) => { const u = parseJSON(data); if (u && u.session) emit('usage', u); },
      flowrun: (data) => { const f = parseJSON(data); if (f && f.run) emit('flowrun', f); },
      evalrun: (data) => { const f = parseJSON(data); if (f && f.run) emit('evalrun', f); },
      // The server dropped us behind the inbox, the downloads or the flows: refetch.
      resync: (data) => { const r = parseJSON(data); if (r) emit('resync', r); },
      session: (data) => {
        let ev;
        try { ev = JSON.parse(data); } catch { return; }
        if (!ev || !ev.Session) return;
        if (ev.Type === 'removed') {
          sessions.delete(ev.Session.ID);
          emit('sessions');
        } else {
          store.upsert(ev.Session);
        }
      },
    }, { keepHidden: () => keepHidden() }).start();
  },

  stopEvents() {
    if (events) { events.stop(); events = null; }
  },
};

function parseJSON(data) {
  try { return JSON.parse(data); } catch { return null; }
}

function emit(kind, payload) {
  for (const fn of listeners) {
    try { fn(kind, payload); } catch (e) { console.error(e); }
  }
}
