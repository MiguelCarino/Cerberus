/* theme-boot.js: the fleet's no-flash theme snippet, as a file so the page's
   meta CSP can stay script-src 'self'. Loaded synchronously in <head>,
   before the stylesheets paint:
   ?theme=gold|color (review links; not stored) > carino-theme cookie
   (shared across *.carino.systems) > localStorage > Gold & Black.
   carino-theme.js owns the switch that changes it afterwards. */
(function () {
  var d = document.documentElement, t = null;
  function pick(v) { return v === 'gold' ? 'gold' : (v === 'color' || v === 'colour') ? 'color' : null; }
  try { t = pick(new URLSearchParams(location.search).get('theme')); } catch (e) {}
  if (!t) { try { var m = document.cookie.match(/(?:^|;\s*)carino-theme=([^;]*)/); if (m) t = pick(decodeURIComponent(m[1])); } catch (e) {} }
  if (!t) { try { t = pick(localStorage.getItem('carino-theme')); } catch (e) {} }
  t = t || 'gold';
  d.setAttribute('data-carino-theme', t);
  var dark = d.getAttribute('data-theme') !== 'light' && window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches;
  var meta = document.getElementById('carinoThemeColor');
  if (meta) meta.content = t === 'gold' ? '#050505' : (dark ? '#0e1420' : '#eef3fc');
})();
