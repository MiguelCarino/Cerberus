/* site.js: behaviour for the cerberus.carino.systems product page.
   - Copy buttons on command blocks copy the commands without their prompts.
   - OS tabs (Linux / macOS / Windows), the visitor's system chosen first.
   - Feature dialogs ([data-open="id"]) and the screenshot viewer, both through
     CarinoUI.modal from carino-ui.js.
   Nothing here makes a network request. */
(function () {
  'use strict';

  /* ---- copy ---- */
  // commandText returns the commands inside a .terminal-wrapper, one per line.
  function commandText(wrap) {
    return Array.prototype.map.call(wrap.querySelectorAll('.cmd'), function (c) {
      return c.textContent;
    }).join('\n');
  }

  function fallbackCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
    ta.remove();
    return ok;
  }

  function wireCopy(btn) {
    var label = btn.querySelector('span') || btn;
    var timer = 0;
    function show(msg, ok) {
      label.textContent = msg;
      btn.classList.toggle('done', ok);
      clearTimeout(timer);
      timer = setTimeout(function () { label.textContent = 'Copy'; btn.classList.remove('done'); }, 1600);
    }
    btn.addEventListener('click', function () {
      var text = commandText(btn.closest('.terminal-wrapper'));
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(function () { show('Copied', true); },
          function () { show(fallbackCopy(text) ? 'Copied' : 'Select and copy', false); });
      } else {
        var ok = fallbackCopy(text);
        show(ok ? 'Copied' : 'Select and copy', ok);
      }
    });
  }

  /* ---- OS tabs ---- */
  // detectOS names the visitor's system for the first tab: linux, macos or windows.
  function detectOS(ua) {
    ua = ua || '';
    if (/Windows/i.test(ua)) return 'windows';
    if (/Macintosh|Mac OS X/i.test(ua) && !/iPhone|iPad/i.test(ua)) return 'macos';
    return 'linux';
  }

  function wireTabs(bar) {
    var tabs = Array.prototype.slice.call(bar.querySelectorAll('[role="tab"]'));
    function select(tab, focus) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute('aria-selected', on ? 'true' : 'false');
        t.tabIndex = on ? 0 : -1;
        t.classList.toggle('active', on);
        var pane = document.getElementById(t.getAttribute('aria-controls'));
        if (pane) pane.hidden = !on;
      });
      if (focus) tab.focus();
    }
    tabs.forEach(function (t, i) {
      t.addEventListener('click', function () { select(t, false); });
      t.addEventListener('keydown', function (e) {
        var n = -1;
        if (e.key === 'ArrowRight') n = (i + 1) % tabs.length;
        else if (e.key === 'ArrowLeft') n = (i - 1 + tabs.length) % tabs.length;
        else if (e.key === 'Home') n = 0;
        else if (e.key === 'End') n = tabs.length - 1;
        if (n >= 0) { e.preventDefault(); select(tabs[n], true); }
      });
    });
    var want = detectOS(navigator.userAgent);
    var first = tabs.filter(function (t) { return t.getAttribute('data-os') === want; })[0] || tabs[0];
    if (first) select(first, false);
  }

  /* ---- dialogs ---- */
  function openModal(el, opener) {
    if (window.CarinoUI && window.CarinoUI.modal) {
      window.CarinoUI.modal.open(el, { returnFocus: opener });
    } else {
      el.hidden = false; // carino-ui.js missing: still show it
    }
  }

  function wireDialogs() {
    document.querySelectorAll('[data-open]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var el = document.getElementById(btn.getAttribute('data-open'));
        if (el) openModal(el, btn);
      });
    });
    var viewer = document.getElementById('dlgShot');
    var img = document.getElementById('dlgShotImg');
    var cap = document.getElementById('dlgShotCap');
    if (!viewer || !img) return;
    document.querySelectorAll('[data-shot]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var thumb = btn.querySelector('img');
        img.src = btn.getAttribute('data-shot');
        img.alt = thumb ? thumb.alt : '';
        if (cap) cap.textContent = btn.getAttribute('data-cap') || '';
        openModal(viewer, btn);
      });
    });
  }

  function boot() {
    document.querySelectorAll('.term-copy').forEach(wireCopy);
    document.querySelectorAll('.os-tabs').forEach(wireTabs);
    wireDialogs();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
