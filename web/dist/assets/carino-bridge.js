/*! carino-bridge.js — hand files from one Carino page to another.
 *
 * One page opens another and gives it files straight out of memory: no upload,
 * no server in the middle, no re-encoding, no size limit worth worrying about.
 * In the fleet:
 *
 *   files.carino.systems  →  dcm.carino.systems       (a DICOM file, to edit)
 *   Carino DICOM          →  Carino DICOM Editor       (a whole study)
 *   fiscal.carino.systems →  quote.carino.systems      (one tax card)
 *
 * The exchange, both sides:
 *   1. the sender opens the receiver at "#carino-bridge"
 *   2. the receiver announces its listener   { carinoBridge: 'ready', v }
 *   3. the sender posts the files            { carinoBridge: 'files', v, … }
 *   4. the receiver acknowledges             { carinoBridge: 'received' }
 *
 * Step 2 is not ceremony. The sender cannot know when the receiver's listener
 * exists, and a payload posted a moment too early is simply lost. Step 4 is
 * not ceremony either: it is sent only once the receiver has taken the files,
 * so a sender that hears it may tell the user the handoff worked.
 *
 * The v both sides carry is read, not decoration. These are independently
 * deployed static sites, so the two copies can be months apart; a side that
 * meets a protocol it does not know says so, and the sender reports "too old"
 * or "too new" instead of leaving the user with a mystery timeout.
 *
 * Canonical copy: Quote/carino-bridge.js, propagated by Carino-Systems/propagate.sh.
 *
 * The window is opened WITHOUT noopener, deliberately: that flag severs
 * window.opener, and the opener is the channel. Which is exactly why the
 * origin checks below matter — they are the only thing standing between this
 * and "any page may shove files into that tab". A receiver says who it trusts;
 * anyone else is refused, or has to ask.
 *
 *   CarinoBridge.send(url, files, opts)  →  Promise<{ count, acknowledged }>
 *   CarinoBridge.receive(opts)           →  true if this page is a handoff
 */
(function (global) {
  'use strict';

  const KEY      = 'carinoBridge';       // marks our messages
  const MARKER   = 'carino-bridge';      // marks the receiving page's URL
  const TRUST    = 'carino-bridge.trusted';
  const PROTOCOL = 1;                    // bump only for a breaking payload change

  // Carino DICOM and its editor spoke this before the bridge had a name; the
  // message names below keep the old spelling because every build already in
  // the field still sends them.
  // The two sides settle the dialect by ear: whichever "ready" arrives first
  // decides what the payload looks like, so an old page and a new one still
  // understand each other. Only hosts that pass legacy:true accept it.
  const LEGACY = { ready: 'carino-pacs-ready', files: 'carino-pacs-files' };

  /* ── shared ─────────────────────────────────────────────────── */

  // Everything that crosses the bridge lands as a plain File, whichever shape
  // it travelled in: a File itself, a Blob, {name,type,body} or legacy {name,buf}.
  function toFile(entry) {
    if (entry instanceof File) return entry;
    if (entry instanceof Blob) return new File([entry], 'file', { type: entry.type || '' });
    if (!entry || typeof entry !== 'object') return null;
    const body = entry.body !== undefined ? entry.body : entry.buf;
    if (body === undefined || body === null) return null;
    if (body instanceof File && (!entry.name || entry.name === body.name)) return body;
    try { return new File([body], entry.name || 'file', { type: entry.type || (body.type || '') }); }
    catch (_) { return null; }
  }

  function payloadFiles(data, legacyOk) {
    if (!data || typeof data !== 'object') return null;
    let list = null;
    if (data[KEY] === 'files' && Array.isArray(data.files)) list = data.files;
    else if (legacyOk && data.type === LEGACY.files && Array.isArray(data.files)) list = data.files;
    if (!list) return null;
    const files = list.map(toFile).filter(Boolean);
    return files.length ? files : null;
  }

  /* ── receiving ──────────────────────────────────────────────── */

  // Origins the user has waved through by hand, remembered per receiver origin
  // (localStorage already scopes itself that way).
  function trustedList() {
    try { const v = JSON.parse(localStorage.getItem(TRUST) || '[]'); return Array.isArray(v) ? v : []; }
    catch (_) { return []; }
  }
  function trustRemember(origin) {
    try {
      const l = trustedList();
      if (!l.includes(origin)) { l.push(origin); localStorage.setItem(TRUST, JSON.stringify(l)); }
    } catch (_) { /* private mode, or the user does not want it kept */ }
  }
  function trustForget(origin) {
    try {
      localStorage.setItem(TRUST, JSON.stringify(
        origin ? trustedList().filter(o => o !== origin) : []));
    } catch (_) {}
  }

  function referrerOrigin() {
    try { return document.referrer ? new URL(document.referrer).origin : null; }
    catch (_) { return null; }
  }

  /* Arm this page to receive.
   *
   *   allow      origins that may deliver without asking; the token
   *              'same-origin' stands for this page's own origin
   *   otherwise  what an origin outside that list gets: 'deny' (default) or 'ask'
   *   ask        (origin, count) → bool | Promise<bool>, for the 'ask' case
   *   remember   let a granted 'ask' stick for that origin next time
   *   onFiles    (File[], { origin, trusted }) — where the files actually go;
   *              returning false refuses them, and the sender is told so
   *              instead of being left believing the handoff worked
   *   legacy     also understand the pre-bridge carino-pacs-* messages
   *
   * Returns false when this is an ordinary visit, so the caller can carry on.
   */
  function receive(opts) {
    opts = opts || {};
    const marker = opts.marker || MARKER;
    const here = location.hash || location.search || '';
    if (!global.opener) return false;
    if (!new RegExp('(^|[#&?])' + marker + '\\b').test(here)) return false;

    const allow = (opts.allow || []).map(o => (o === 'same-origin' ? location.origin : o));
    const otherwise = opts.otherwise === 'ask' ? 'ask' : 'deny';
    const remember = opts.remember === true && otherwise === 'ask';
    const verdict = origin =>
      (allow.includes(origin) || (remember && trustedList().includes(origin))) ? 'allow' : otherwise;

    const reply = (e, msg) => { try { e.source.postMessage(msg, e.origin); } catch (_) {} };

    let taken = false;
    const onMsg = async e => {
      if (taken || e.source !== global.opener) return;
      const d = e.data;
      if (!d || typeof d !== 'object') return;
      if (d[KEY] !== 'files' && !(opts.legacy && d.type === LEGACY.files)) return;
      // The verdict comes before anything is said back. A denied origin gets
      // silence, not the shape of a listener it can then work against.
      const call = verdict(e.origin);
      if (call === 'deny') return;                 // not ours to take; stay listening
      // A payload from a newer bridge than this copy knows: say so out loud
      // rather than dropping it, since to the sender the two are identical.
      if (d[KEY] === 'files' && +d.v > PROTOCOL) {
        reply(e, { [KEY]: 'unsupported', v: PROTOCOL });
        return;
      }
      const files = payloadFiles(d, !!opts.legacy);
      if (!files) return;

      taken = true;                                // one handoff per page, and no
      global.removeEventListener('message', onMsg);// second sender racing the dialog
      if (call === 'ask') {
        let ok = false;
        try { ok = opts.ask ? await opts.ask(e.origin, files.length) : false; } catch (_) {}
        if (!ok) { reply(e, { [KEY]: 'declined' }); return; }
        if (remember) trustRemember(e.origin);
      }
      // Drop the marker: reloading this tab should be an ordinary visit, not a
      // handoff waiting on a sender that finished long ago.
      try { history.replaceState(null, '', location.pathname + location.search); } catch (_) {}
      // The acknowledgement is the receiver's word that it has the files, so it
      // waits for the handler to take them: one that refuses them (returns
      // false) or throws leaves the sender with a refusal rather than a success
      // it can do nothing about.
      let kept = false;
      try { kept = (await opts.onFiles(files, { origin: e.origin, trusted: call === 'allow' })) !== false; }
      catch (err) { if (opts.onError) opts.onError(err); }
      reply(e, kept ? { [KEY]: 'received', n: files.length } : { [KEY]: 'declined' });
    };
    global.addEventListener('message', onMsg);

    // Who hears that we are listening. Preferably just the page we expected —
    // a targetOrigin that does not match the opener means the message is never
    // delivered, so naming the wrong one costs nothing but silence. '*' is used
    // only when we are willing to ask about strangers anyway and the browser
    // withheld the referrer; the announcement carries no data of its own.
    const ref = referrerOrigin();
    let targets;
    if (ref && verdict(ref) === 'allow') targets = [ref];
    else if (otherwise === 'ask') targets = [ref || '*'];
    else targets = allow;

    targets.forEach(o => {
      try { global.opener.postMessage({ [KEY]: 'ready', v: PROTOCOL }, o); } catch (_) {}
      if (opts.legacy) { try { global.opener.postMessage({ type: LEGACY.ready }, o); } catch (_) {} }
    });
    return true;
  }

  /* ── sending ────────────────────────────────────────────────── */

  /* Open the receiver and give it files.
   *
   *   url      the receiving page; its origin is the only one talked to
   *   files    File | File[] | {name,type,body}[] — or a function returning
   *            any of those, called once the far side is listening, so nothing
   *            is fetched or read for a window that never opened
   *   opts     { timeout = 60000, legacy, transfer, marker, name }
   */
  function send(url, files, opts) {
    opts = opts || {};
    let abs, target;
    try { const u = new URL(url, location.href); abs = u.href; target = u.origin; }
    catch (_) { return Promise.reject(new Error('bridge: that is not a URL')); }

    const sep = abs.includes('#') ? '&' : '#';
    const win = global.open(abs + sep + (opts.marker || MARKER), opts.name || '_blank');
    if (!win) return Promise.reject(new Error('bridge: pop-up blocked'));

    return new Promise((resolve, reject) => {
      let done = false, posted = false;
      const finish = (fn, arg) => {
        if (done) return;
        done = true;
        global.removeEventListener('message', onMsg);
        clearTimeout(timer);
        fn(arg);
      };
      const timer = setTimeout(() => finish(reject, new Error(posted
        ? 'bridge: no acknowledgement from the other page'
        : 'bridge: the other page never answered')), opts.timeout || 60000);

      async function deliver(legacy) {
        posted = true;
        let items = (typeof files === 'function') ? await files() : files;
        items = (Array.isArray(items) ? items : [items]).filter(Boolean);
        if (!items.length) throw new Error('bridge: nothing to send');

        const entries = [];
        for (const item of items) {
          if (legacy) {
            const f = toFile(item);
            entries.push({ name: f.name, buf: (item && item.buf) || await f.arrayBuffer() });
          } else if (item instanceof File || item instanceof Blob) {
            const f = toFile(item);
            entries.push({ name: f.name, type: f.type, body: f });
          } else {
            entries.push({
              name: item.name || 'file', type: item.type || '',
              body: item.body !== undefined ? item.body : item.buf,
            });
          }
        }
        // Raw buffers travel by transfer: the sender is handing them over for
        // good, so a copy would be waste. Files clone as they are.
        const transfer = opts.transfer === false ? [] :
          entries.map(e => (legacy ? e.buf : e.body)).filter(b => b instanceof ArrayBuffer);

        win.postMessage(legacy ? { type: LEGACY.files, files: entries }
                               : { [KEY]: 'files', v: PROTOCOL, files: entries }, target, transfer);
        // The old dialect has no acknowledgement to wait for.
        if (legacy) finish(resolve, { count: entries.length, acknowledged: false });
      }

      const onMsg = e => {
        if (done || e.source !== win || e.origin !== target || !e.data) return;
        const d = e.data;
        if (!posted && (d[KEY] === 'ready' || (opts.legacy && d.type === LEGACY.ready))) {
          if (+d.v > PROTOCOL) return finish(reject, new Error('bridge: this page is too old for the other one'));
          deliver(d[KEY] !== 'ready').catch(err => finish(reject, err));
        } else if (d[KEY] === 'received') {
          finish(resolve, { count: d.n || 0, acknowledged: true });
        } else if (d[KEY] === 'declined') {
          finish(reject, new Error('bridge: the other page declined the files'));
        } else if (d[KEY] === 'unsupported') {
          finish(reject, new Error('bridge: the other page is too old for this one'));
        }
      };
      global.addEventListener('message', onMsg);
    });
  }

  global.CarinoBridge = {
    send, receive,
    MARKER,
    trusted: { list: trustedList, forget: trustForget },
  };
})(window);
