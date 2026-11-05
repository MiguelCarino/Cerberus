// Settings view (#/settings[/<group>]): every config setting rendered from
// GET /api/config/schema, edited into a draft and saved as one atomic
// PATCH /api/config {set}. Groups are a left column on desktop and a list
// on phones; each shows its Basic settings, with Advanced ones behind one
// toggle. Fields render by kind (text, number with unit, switch, select,
// duration with a friendly reading, path, list and map editors, a vault
// secret picker), show the server's problems inline and a "needs restart"
// badge. A sticky bar counts the changes and saves or discards them; a
// banner offers the restart when one is pending, then reconnects and
// reloads. Search filters every setting by label or help. The Security
// subsection embeds the phase 3 Security panel; Secrets, Fine-tuning and
// About are panels of their own.
//
// The draft lives at module level, so moving between groups (a hash
// change remounts the view) keeps unsaved edits. Nothing polls: data loads
// on mount and when the tab becomes visible again with no unsaved edits.

import { api, apiResult, post, AuthError } from '../api.js';
import { h, clear, confirmDialog, openDialog, toast, errText, pill } from '../dom.js';
import {
  SECURITY, slug, normSchema, normConfigResponse, normProblems, groupSchema, getAt, setAt, clone, same,
  dirtyDiff, changeCount, needsRestart, itemRel, scalarItem, mapKeyPath, parseValue, formatValue, unitLabel,
  humanDuration, filterFields, problemsAt, fieldForProblem, problemCounts, newItem, moveItem, removeItem,
  itemTitle, mapEntries, entriesToMap, secretChoices, restarted, reconnectDelay, RESTART_GIVE_UP_MS,
} from '../settings.js';
import * as security from './security.js';
import * as secrets from './secrets.js';
import { aboutPanel } from './about.js';
import { finetunePanel } from './finetune.js';
import { settingsIOPanel } from './settingsio.js';
import { usersPanel } from './users.js';
import { permissionsPanel } from './permissions.js';
import { store } from '../store.js';

const ADV_KEY = 'cerberus.settings.advanced';
const readAdvanced = () => { try { return localStorage.getItem(ADV_KEY) === '1'; } catch { return false; } };
const writeAdvanced = (on) => { try { localStorage.setItem(ADV_KEY, on ? '1' : '0'); } catch { /* private window */ } };

// Kept across remounts (group changes are hash changes).
const S = {
  schema: null, groups: [], orig: null, draft: null, problems: [], restartPending: false,
  secrets: null, query: '', advanced: readAdvanced(), focus: '',
};

// Inputs whose text does not parse: concrete path → message. Save waits.
const invalid = new Map();
let uid = 0;
const nextId = () => `set-${++uid}`;

// PANELS are the groups that are not schema groups (each loads itself).
const PANELS = ['users', 'permissions', 'security', 'secrets', 'finetune', 'transfer', 'about'];

const dirty = () => (S.schema && S.orig ? dirtyDiff(S.orig, S.draft, S.schema) : {});

// patchConfig sends changes; resolves {ok, problems, restartPending, message}.
export async function patchConfig(set) {
  const r = await apiResult('/api/config', { method: 'PATCH', body: { set } });
  const body = r.body && typeof r.body === 'object' ? r.body : {};
  return { ok: r.ok, problems: normProblems(body.problems || body.Problems), restartPending: !!body.restart_pending, message: r.message };
}

export function mount(root, r) {
  let gone = false;
  let secUnmount = null;
  const rows = new Map(); // top-level path → {row, badge}
  const want = (r && r.group) || (store.can('settings') ? '' : 'permissions');

  const search = h('input', {
    class: 'input set-search', type: 'search', placeholder: 'Search settings', 'aria-label': 'Search settings',
    autocapitalize: 'off', spellcheck: 'false', value: S.query,
    oninput: (e) => { S.query = e.target.value; renderMain(); },
  });
  const banner = h('div', { class: 'set-banner' });
  const navEl = h('nav', { class: 'set-nav', 'aria-label': 'Setting groups' });
  const mainEl = h('div', { class: 'set-main' });
  const countEl = h('span', { class: 'set-count' });
  const saveBtn = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => save() }, 'Save');
  const discardBtn = h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => discard() }, 'Discard');
  const bar = h('div', { class: 'set-bar', hidden: true, role: 'region', 'aria-label': 'Unsaved changes' },
    countEl, h('span', { class: 'set-bar-gap' }), discardBtn, saveBtn);
  const view = h('section', { class: 'view view-pad set', 'data-pick': want ? '0' : '1' },
    h('div', { class: 'view-head' },
      h('div', {}, h('h1', { class: 'view-title', text: 'Settings' }),
        h('p', { class: 'view-sub', text: store.can('settings') ? 'Every option, explained. Changes apply when you save.' : 'What runs without asking you.' })),
      store.can('settings') ? search : null),
    banner,
    h('div', { class: 'set-layout' }, navEl, mainEl),
    bar);
  root.append(view);

  // ---------- data ----------
  async function load({ schema = !S.schema } = {}) {
    try {
      const [sc, cfg, sec] = await Promise.all([
        schema ? api('/api/config/schema') : null,
        api('/api/config'),
        S.secrets ? null : api('/api/secrets').catch(() => null),
      ]);
      if (gone) return;
      if (sc) { S.schema = normSchema(sc); S.groups = groupSchema(S.schema); }
      if (sec) S.secrets = ((sec && sec.items) || []).map((it) => it.Name ?? it.name).filter(Boolean);
      const c = normConfigResponse(cfg);
      S.orig = c.config || {};
      S.draft = clone(S.orig);
      S.problems = c.problems;
      S.restartPending = c.restartPending;
      invalid.clear();
      // An open Security panel keeps running; redrawing it would load it twice.
      render({ main: !PANELS.includes(want) || !!S.query.trim() });
    } catch (e) {
      if (gone || e instanceof AuthError) return;
      renderNav();
      // Security does not need the config; leave it showing.
      if (!PANELS.includes(want) || S.query.trim()) clear(mainEl).append(h('div', { class: 'alert err', text: errText(e) }));
    }
  }

  // ---------- render ----------
  function currentGroup() {
    if (want === 'users') return 'Users';
    if (want === 'permissions') return 'Permissions';
    if (want === 'security') return SECURITY;
    if (want === 'secrets') return 'Secrets';
    if (want === 'finetune') return 'Fine-tuning';
    if (want === 'transfer') return 'Import & export';
    if (want === 'about') return 'About';
    const g = S.groups.find((x) => x.slug === want);
    return g ? g.group : (S.groups[0] && S.groups[0].group) || '';
  }

  function render({ main = true } = {}) {
    renderBanner();
    renderNav();
    if (main) renderMain();
    updateBar();
  }

  function renderBanner() {
    clear(banner);
    if (S.restartPending) {
      banner.append(h('div', { class: 'alert warn set-restart' },
        h('span', { text: 'Some saved changes take effect after a restart.' }),
        h('button', { class: 'btn btn-primary', type: 'button', onclick: () => restart() }, 'Restart now')));
    }
    const errs = S.problems.filter((p) => p.level === 'error');
    const warns = S.problems.filter((p) => p.level === 'warn');
    if (!errs.length && !warns.length) return;
    const parts = [errs.length && `${errs.length} error${errs.length === 1 ? '' : 's'}`, warns.length && `${warns.length} warning${warns.length === 1 ? '' : 's'}`].filter(Boolean);
    banner.append(h('details', { class: `alert ${errs.length ? 'err' : 'warn'} set-probsum` },
      h('summary', { text: `${parts.join(' and ')} in the configuration` }),
      h('ul', { class: 'set-problist' }, [...errs, ...warns].map((p) => {
        const f = fieldForProblem(S.schema || [], p.path);
        return h('li', {},
          h('button', { class: 'set-problink', type: 'button', onclick: () => goTo(f) },
            h('b', { text: f ? `${f.group || 'Advanced'} › ${f.label || f.path}` : p.path }),
            h('span', { text: ` ${p.message}` })));
      }))));
  }

  function goTo(f) {
    if (!f) return;
    S.focus = f.path;
    S.query = '';
    if (f.level === 'advanced' && !S.advanced) { S.advanced = true; writeAdvanced(true); }
    const target = `#/settings/${slug(f.group || 'Advanced')}`;
    if (location.hash === target) { search.value = ''; renderMain(); } else location.hash = target;
  }

  function renderNav() {
    clear(navEl);
    if (!store.can('settings')) {
      // A user (not an admin) reaches Settings for their own saved
      // permissions only.
      navEl.append(h('a', { class: 'set-nav-item', href: '#/settings/permissions', 'aria-current': 'page' },
        h('span', { class: 'set-nav-label', text: 'Permissions' }), h('span', { class: 'set-nav-sub', text: 'What runs without asking' }),
        h('span', { class: 'set-nav-go', 'aria-hidden': 'true', text: '›' })));
      return;
    }
    const counts = problemCounts(S.schema || [], S.problems);
    const diff = dirty();
    const changedGroups = new Set(Object.keys(diff).map((p) => ((S.schema || []).find((f) => f.path === p) || {}).group || 'Advanced'));
    const cur = S.query ? '' : currentGroup();
    // On phones the bare #/settings is the group list, so nothing is current.
    const showsGroup = want !== '' || window.matchMedia('(min-width: 900px)').matches;
    const item = (label, href, extra) => h('a', { class: 'set-nav-item', href, 'aria-current': label === cur && showsGroup ? 'page' : null },
      h('span', { class: 'set-nav-label', text: label }), extra, h('span', { class: 'set-nav-go', 'aria-hidden': 'true', text: '›' }));
    for (const g of S.groups) {
      const n = counts[g.group];
      navEl.append(item(g.group, `#/settings/${g.slug}`, [
        changedGroups.has(g.group) ? h('span', { class: 'set-dot', title: 'Unsaved changes', 'aria-label': 'unsaved changes' }) : null,
        n && n.error ? pill(String(n.error), 'err') : n && n.warn ? pill(String(n.warn), 'warn') : null,
      ]));
    }
    navEl.append(h('div', { class: 'set-nav-sep' }),
      store.can('users') ? item('Users', '#/settings/users', h('span', { class: 'set-nav-sub', text: 'Accounts, roles, budgets' })) : null,
      item('Permissions', '#/settings/permissions', h('span', { class: 'set-nav-sub', text: 'Saved “always allow” rules, command policy' })),
      item(SECURITY, '#/settings/security',
      h('span', { class: 'set-nav-sub', text: 'Devices, token, doctor, audit' })),
    item('Secrets', '#/settings/secrets', h('span', { class: 'set-nav-sub', text: 'API keys and tokens in the vault' })),
    item('Fine-tuning', '#/settings/finetune', h('span', { class: 'set-nav-sub', text: 'Training examples, ratings and runs' })),
    item('Import & export', '#/settings/transfer', h('span', { class: 'set-nav-sub', text: 'Settings and models to a file, and back' })),
    item('About', '#/settings/about', h('span', { class: 'set-nav-sub', text: 'Version, what Cerberus is, links' })));
  }

  function renderMain() {
    if (secUnmount) { secUnmount(); secUnmount = null; }
    rows.clear();
    invalid.clear(); // every input is rebuilt from the draft
    clear(mainEl);
    const back = h('a', { class: 'btn btn-ghost set-back', href: '#/settings', onclick: (e) => {
      if (!S.query) return;
      S.query = '';
      search.value = '';
      if (location.hash === '#/settings') { e.preventDefault(); renderMain(); }
    } }, '‹ All settings');
    if (!S.query.trim() && want === 'security') {
      view.dataset.pick = '0';
      renderSecurity(back);
      return;
    }
    if (!S.query.trim() && (want === 'users' || want === 'permissions' || want === 'secrets' || want === 'finetune' || want === 'transfer' || want === 'about')) {
      view.dataset.pick = '0';
      renderPanel(back, want);
      return;
    }
    if (!S.schema) { mainEl.append(h('p', { class: 't-sec', text: 'Loading…' })); return; }
    if (S.query.trim()) {
      view.dataset.pick = '0';
      const hits = filterFields(S.schema, S.query);
      mainEl.append(h('div', { class: 'set-group-head' }, back,
        h('h2', { class: 'set-group-title', text: `${hits.length} match${hits.length === 1 ? '' : 'es'}` })));
      if (!hits.length) mainEl.append(h('p', { class: 't-sec', text: 'No setting matches. Try fewer words.' }));
      mainEl.append(...hits.map((f) => fieldRow(f, true)));
      renderNav();
      return;
    }
    view.dataset.pick = want ? '0' : '1';
    const group = currentGroup();
    const g = S.groups.find((x) => x.group === group);
    if (!g) return;
    const adv = h('label', { class: 'set-adv' },
      h('span', { class: 'switch' },
        h('input', { type: 'checkbox', checked: S.advanced, 'aria-label': 'Show advanced settings', onchange: (e) => { S.advanced = e.target.checked; writeAdvanced(S.advanced); renderMain(); } }),
        h('span', { class: 'track' })),
      h('span', { text: `Advanced (${g.advanced.length})` }));
    mainEl.append(h('div', { class: 'set-group-head' }, back, h('h2', { class: 'set-group-title', text: g.group }), g.advanced.length ? adv : null));
    if (!g.basic.length && !S.advanced) {
      mainEl.append(h('p', { class: 't-sec set-empty', text: 'Everything here is advanced. Turn on Advanced to see it.' }));
    }
    mainEl.append(...g.basic.map((f) => fieldRow(f)));
    if (S.advanced && g.advanced.length) {
      if (g.basic.length) mainEl.append(h('h3', { class: 'group-title', text: 'Advanced' }));
      mainEl.append(...g.advanced.map((f) => fieldRow(f)));
    }
    if (S.focus) {
      const el = rows.get(S.focus)?.row;
      S.focus = '';
      if (el) requestAnimationFrame(() => { el.scrollIntoView({ block: 'center' }); el.classList.add('flash'); });
    }
  }

  // The phase 3 Security panel, embedded (it loads and refreshes itself).
  function renderSecurity(back) {
    mainEl.append(h('div', { class: 'set-group-head' }, back, h('h2', { class: 'set-group-title', text: 'Security' })),
      h('p', { class: 'field-hint set-group-sub', text: 'Who can reach this host, what they did, and what to fix.' }));
    const box = h('div', {});
    mainEl.append(box);
    secUnmount = security.mount(box, null, { embedded: true }) || null;
  }

  // Secrets (the vault), Fine-tuning and About, embedded like Security.
  // Each loads itself; secUnmount takes it down when the group changes.
  function renderPanel(back, which) {
    const P = {
      users: ['Users', 'Who can sign in, with which role, and what they may spend.'],
      permissions: ['Permissions', 'Your saved permissions: tool calls that run without asking.'],
      secrets: ['Secrets', 'Provider keys and tokens for agents. Values are write-only here.'],
      finetune: ['Fine-tuning', 'Examples kept for training a local model: review them, rate them, and run your trainer.'],
      transfer: ['Import & export', 'Move these settings and the list of local models to another machine, or keep a copy.'],
      about: ['About', 'This build of Cerberus, and where to read more.'],
    };
    const [title, sub] = P[which];
    mainEl.append(h('div', { class: 'set-group-head' }, back, h('h2', { class: 'set-group-title', text: title })),
      h('p', { class: 'field-hint set-group-sub', text: sub }));
    const box = h('div', {});
    mainEl.append(box);
    const panel = which === 'users' ? usersPanel(box)
      : which === 'permissions' ? permissionsPanel(box)
      : which === 'secrets' ? secrets.mount(box, null, { embedded: true })
      : which === 'finetune' ? finetunePanel(box, { openSetting })
        : which === 'transfer' ? settingsIOPanel(box)
          : aboutPanel(box);
    secUnmount = panel || null;
  }

  // openSetting shows one setting in its group (Advanced turned on when
  // it is an advanced one), flashed like a problem link.
  function openSetting(path) {
    const f = (S.schema || []).find((x) => x.path === path);
    goTo(f || { path, group: 'Advanced', level: 'advanced' });
  }

  // ---------- one setting ----------
  function fieldRow(f, withGroup = false) {
    const get = () => getAt(S.draft, f.path);
    const set = (v) => { setAt(S.draft, f.path, v); changed(); };
    const id = nextId();
    const badge = f.restart ? h('span', { class: 'pill muted set-restart-pill', title: 'A change here needs a daemon restart', text: 'restart' }) : null;
    const reset = h('button', { class: 'btn btn-ghost btn-xs set-reset', type: 'button', hidden: true, title: 'Back to the default value' }, 'Use default');
    const holder = h('div', { class: 'set-control' });
    const paint = () => {
      for (const k of [...invalid.keys()]) if (k === f.path || k.startsWith(f.path + '[') || k.startsWith(f.path + '.') || k.startsWith(f.path + '#')) invalid.delete(k);
      clear(holder).append(editor(f, get, set, f.path, id, paint));
      syncReset();
      updateBar();
    };
    const syncReset = () => { reset.hidden = f.readOnly || f.default === undefined || same(get(), f.default); };
    reset.addEventListener('click', () => { set(clone(f.default)); paint(); });
    const wide = f.kind === 'list' || f.kind === 'map';
    const row = h('div', { class: `set-field${wide ? ' wide' : ''}`, id: `f-${f.path}`, 'data-path': f.path },
      h('div', { class: 'set-field-head' },
        h('label', { class: 'set-label', for: id, text: f.label || f.path }),
        withGroup ? h('span', { class: 'set-where', text: f.group || 'Advanced' }) : null,
        f.readOnly ? pill('read-only', 'muted') : null,
        badge,
        h('span', { class: 'set-dot set-field-dot', title: 'Changed', 'aria-hidden': 'true' }),
        reset),
      f.help ? h('p', { class: 'set-help', text: f.help + defaultNote(f) }) : null,
      holder);
    rows.set(f.path, { row, badge, syncReset });
    paint();
    markRow(f.path, dirty());
    return row;
  }

  function markRow(path, diff) {
    const r = rows.get(path);
    if (!r) return;
    const on = Object.hasOwn(diff, path);
    r.row.classList.toggle('dirty', on);
    if (r.badge) {
      r.badge.className = `pill ${on ? 'warn' : 'muted'} set-restart-pill`;
      r.badge.textContent = on ? 'needs restart' : 'restart';
    }
    r.syncReset?.();
  }

  // ---------- editors by kind ----------
  // editor returns the control for field f; get/set read and write its
  // value; cpath is its concrete path (problems, parse errors); repaint
  // redraws it (lists after add, remove or move).
  function editor(f, get, set, cpath, id, repaint) {
    if (f.kind === 'list') return listEditor(f, get, set, cpath, id, repaint);
    if (f.kind === 'map') return mapEditor(f, get, set, cpath, id);
    return scalarEditor(f, get, set, cpath, id);
  }

  function probsEl(cpath) {
    const ps = problemsAt(S.problems, cpath);
    return ps.length ? h('ul', { class: 'set-probs' }, ps.map((p) => h('li', { class: p.level, text: p.message }))) : null;
  }

  function scalarEditor(f, get, set, cpath, id = nextId()) {
    const msg = h('p', { class: 'set-err', role: 'alert' });
    const wrap = h('div', { class: 'set-scalar' });
    const v = get();
    if (f.kind === 'bool') {
      wrap.append(h('label', { class: 'set-switch' },
        h('span', { class: 'switch' },
          h('input', { id, type: 'checkbox', checked: !!v, disabled: f.readOnly, onchange: (e) => set(e.target.checked) }),
          h('span', { class: 'track' })),
        h('span', { class: 'set-switch-text', text: v ? 'On' : 'Off' })));
      wrap.querySelector('input').addEventListener('change', (e) => { wrap.querySelector('.set-switch-text').textContent = e.target.checked ? 'On' : 'Off'; });
    } else if (f.kind === 'enum') {
      const opts = [...f.enum];
      if (v != null && !opts.includes(v)) opts.push(v);
      wrap.append(h('select', { id, class: 'select set-input', disabled: f.readOnly, onchange: (e) => set(e.target.value) },
        opts.map((o) => h('option', { value: o, selected: o === (v ?? ''), text: o === '' ? '(default)' : o }))));
    } else if (f.kind === 'secret-ref') {
      const choices = secretChoices(S.secrets, v || '');
      wrap.append(h('div', { class: 'set-inline' },
        h('select', { id, class: 'select set-input', disabled: f.readOnly, onchange: (e) => set(e.target.value) },
          h('option', { value: '', text: '(none)' }),
          choices.map((c) => h('option', { value: c.name, selected: c.name === v, text: c.missing ? `${c.name} (not in the vault)` : c.name }))),
        h('a', { class: 'btn btn-ghost btn-xs', href: '#/settings/secrets', title: 'Add or replace secrets' }, 'Secrets')));
      if (!S.secrets) wrap.append(h('p', { class: 'field-hint', text: 'The vault could not be listed; names you set stay as they are.' }));
    } else {
      const numeric = f.kind === 'int' || f.kind === 'float';
      const friendly = h('span', { class: 'set-friendly' });
      const zero = f.path === 'engines.idle_ttl_sec' ? 'stays loaded' : '0 seconds';
      const showFriendly = (n) => { if (f.kind === 'duration-seconds') friendly.textContent = humanDuration(n, zero); };
      const input = h('input', {
        id, class: `input set-input${f.kind === 'path' || f.format === 'command' ? ' mono' : ''}`,
        type: f.format === 'email' ? 'email' : 'text',
        inputmode: f.kind === 'int' ? 'numeric' : f.kind === 'float' ? 'decimal' : f.format === 'url' ? 'url' : null,
        autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off',
        readonly: f.readOnly, value: formatValue(f, v),
        placeholder: f.kind === 'duration-seconds' ? 'e.g. 10m' : f.kind === 'path' ? '~/…' : f.format === 'url' ? 'https://…' : f.format === 'address' ? 'host:port' : null,
      });
      input.addEventListener('input', () => {
        const res = parseValue(f, input.value);
        if (res.error) { invalid.set(cpath, res.error); msg.textContent = res.error; input.setAttribute('aria-invalid', 'true'); updateBar(); return; }
        invalid.delete(cpath);
        msg.textContent = '';
        input.removeAttribute('aria-invalid');
        showFriendly(res.value);
        if (!same(res.value, get())) set(res.value); else updateBar();
      });
      showFriendly(v);
      const unit = unitLabel(f);
      wrap.append(h('div', { class: 'set-inline' }, input,
        unit ? h('span', { class: 'set-unit', text: unit }) : null,
        f.kind === 'duration-seconds' ? friendly : null));
      if (numeric && (f.min != null || f.max != null)) {
        wrap.append(h('p', { class: 'set-range', text: [f.min != null && `min ${f.min}`, f.max != null && `max ${f.max}`].filter(Boolean).join(' · ') }));
      }
    }
    wrap.append(msg);
    const pe = probsEl(cpath);
    if (pe) wrap.append(pe);
    return wrap;
  }

  // Item fields of a struct item, as labelled sub-rows.
  function itemFields(f, getItem, setIn, base) {
    return h('div', { class: 'set-item-fields' }, f.items.filter((it) => !it.sensitive).map((it) => {
      const rel = itemRel(f, it);
      const id = nextId();
      const holder = h('div', {});
      const get = () => getAt(getItem(), rel);
      const set = (v) => setIn(rel, v);
      const paint = () => clear(holder).append(editor(it, get, set, `${base}.${rel}`, id, paint));
      paint();
      return h('div', { class: 'set-sub' },
        h('label', { class: 'set-sublabel', for: id, text: it.label || rel }),
        it.help ? h('p', { class: 'set-help', text: it.help }) : null,
        holder);
    }));
  }

  function moveBtns(i, n, onMove, onRemove, what) {
    return h('span', { class: 'le-btns' },
      h('button', { class: 'btn btn-ghost btn-xs', type: 'button', disabled: i === 0, 'aria-label': `Move ${what} up`, onclick: (e) => { e.preventDefault(); onMove(-1); } }, '↑'),
      h('button', { class: 'btn btn-ghost btn-xs', type: 'button', disabled: i === n - 1, 'aria-label': `Move ${what} down`, onclick: (e) => { e.preventDefault(); onMove(1); } }, '↓'),
      h('button', { class: 'btn btn-ghost btn-xs le-rm', type: 'button', 'aria-label': `Remove ${what}`, onclick: (e) => { e.preventDefault(); onRemove(); } }, '✕'));
  }

  function listEditor(f, get, set, cpath, id, repaint) {
    const list = () => get() || [];
    const sc = scalarItem(f);
    const out = h('div', { class: 'le', id, tabindex: '-1' });
    const items = list();
    const move = (i, d) => { forgetInvalid(cpath); set(moveItem(list(), i, d)); repaint(); };
    const remove = (i) => { forgetInvalid(cpath); set(removeItem(list(), i)); repaint(); };
    if (!items.length) out.append(h('p', { class: 't-sec le-empty', text: 'None yet.' }));
    items.forEach((item, i) => {
      const ip = `${cpath}[${i}]`;
      if (sc) {
        const ed = scalarEditor(sc, () => list()[i], (v) => { const l = clone(list()); l[i] = v; set(l); }, ip);
        out.append(h('div', { class: 'le-row' }, ed, moveBtns(i, items.length, (d) => move(i, d), () => remove(i), sc.label || 'item')));
        return;
      }
      const title = h('span', { class: 'le-title', text: itemTitle(item, i) });
      const setIn = (rel, v) => { const l = clone(list()); setAt(l[i], rel, v); set(l); title.textContent = itemTitle(l[i], i); };
      const hasProbs = S.problems.some((p) => p.path === ip || p.path.startsWith(ip + '.') || p.path.startsWith(ip + '['));
      out.append(h('details', { class: 'le-card', open: hasProbs ? true : null },
        h('summary', { class: 'le-sum' }, title, hasProbs ? pill('check', 'err') : null,
          moveBtns(i, items.length, (d) => move(i, d), () => remove(i), itemTitle(item, i))),
        probsEl(ip),
        itemFields(f, () => list()[i], setIn, ip)));
    });
    out.append(h('button', { class: 'btn le-add', type: 'button', disabled: f.readOnly, onclick: () => {
      set([...list(), newItem(f)]);
      repaint();
      const fresh = document.getElementById(id);
      const cards = fresh ? fresh.querySelectorAll(':scope > .le-card') : [];
      const last = cards[cards.length - 1];
      if (last) { last.open = true; last.querySelector('input, select')?.focus(); }
      else { const ins = fresh ? fresh.querySelectorAll(':scope > .le-row input') : []; ins[ins.length - 1]?.focus(); }
    } }, `+ Add ${sc && sc.label && sc.label !== f.label ? sc.label.toLowerCase() : singular(f.label)}`));
    return out;
  }

  function mapEditor(f, get, set, cpath, id) {
    const sc = scalarItem(f);
    const rowsState = mapEntries(get()).map(([key, value]) => ({ key, value }));
    const keyErr = h('p', { class: 'set-err', role: 'alert' });
    const out = h('div', { class: 'le', id, tabindex: '-1' });
    const list = h('div', { class: 'le-map' });
    const write = () => {
      const { map, error } = entriesToMap(rowsState.map((r) => [r.key, r.value]));
      keyErr.textContent = error || '';
      if (error) invalid.set(cpath + '#keys', error); else invalid.delete(cpath + '#keys');
      if (!error) set(map); else updateBar();
    };
    const paint = () => {
      clear(list);
      if (!rowsState.length) list.append(h('p', { class: 't-sec le-empty', text: 'None yet.' }));
      rowsState.forEach((r, i) => {
        const kp = () => mapKeyPath(cpath, r.key.trim());
        const keyIn = h('input', {
          class: 'input set-input mono le-key', value: r.key, placeholder: f.path.endsWith('env') ? 'NAME' : 'key',
          'aria-label': 'Key', autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off',
          oninput: (e) => { r.key = e.target.value; write(); },
        });
        const rm = h('button', { class: 'btn btn-ghost btn-xs le-rm', type: 'button', 'aria-label': 'Remove entry', onclick: () => { rowsState.splice(i, 1); write(); paint(); } }, '✕');
        if (sc) {
          list.append(h('div', { class: 'le-row le-kv' }, keyIn,
            scalarEditor(sc, () => r.value, (v) => { r.value = v; write(); }, kp()), rm));
        } else {
          list.append(h('div', { class: 'le-card le-mapcard' },
            h('div', { class: 'le-sum' }, keyIn, rm),
            probsEl(kp()),
            itemFields(f, () => r.value, (rel, v) => { r.value = clone(r.value) || {}; setAt(r.value, rel, v); write(); }, kp())));
        }
      });
    };
    paint();
    out.append(list, keyErr,
      h('button', { class: 'btn le-add', type: 'button', disabled: f.readOnly, onclick: () => {
        rowsState.push({ key: '', value: newItem(f) });
        paint();
        list.querySelectorAll('.le-key')[rowsState.length - 1]?.focus();
      } }, `+ Add ${singular(f.label)}`));
    return out;
  }

  function forgetInvalid(prefix) {
    for (const k of [...invalid.keys()]) if (k.startsWith(prefix + '[')) invalid.delete(k);
  }

  // ---------- changes ----------
  function changed() {
    const diff = dirty();
    for (const p of rows.keys()) markRow(p, diff);
    updateBar(diff);
    renderNav(); // the unsaved-changes dots
  }

  function updateBar(diff = dirty()) {
    const n = changeCount(diff);
    const bad = invalid.size;
    bar.hidden = n === 0 && bad === 0;
    const restartNote = needsRestart(diff, S.schema || []) ? ' · needs restart' : '';
    countEl.textContent = bad ? `${bad} value${bad === 1 ? '' : 's'} to fix` : `${n} change${n === 1 ? '' : 's'}${restartNote}`;
    countEl.classList.toggle('bad', bad > 0);
    saveBtn.disabled = n === 0 || bad > 0;
  }

  let saving = false;
  async function save() {
    const set = dirty();
    if (saving || !changeCount(set) || invalid.size) return;
    saving = true;
    saveBtn.disabled = true;
    saveBtn.textContent = 'Saving…';
    try {
      const res = await patchConfig(set);
      if (gone) return;
      if (res.ok) {
        // Saved: the draft is the new baseline until the fresh copy arrives.
        S.orig = clone(S.draft);
        S.problems = res.problems;
        // The server's word, both ways: a save that reverts a pending
        // change clears it (restart_pending is boot vs live).
        S.restartPending = res.restartPending;
        try {
          const cfg = normConfigResponse(await api('/api/config'));
          if (gone) return;
          S.orig = cfg.config || S.orig;
          S.draft = clone(S.orig);
          S.problems = cfg.problems;
          S.restartPending = cfg.restartPending;
        } catch { /* keep the saved draft as the baseline */ }
        if (gone) return;
        invalid.clear();
        render();
        toast(S.restartPending ? 'Saved. Restart to apply some changes.' : 'Saved', 'ok');
      } else if (res.problems.length) {
        const keys = new Set(res.problems.map((p) => p.path + '\0' + p.message));
        S.problems = [...res.problems, ...S.problems.filter((p) => !keys.has(p.path + '\0' + p.message))];
        render();
        toast('Not saved: fix the marked settings', 'err');
      } else {
        toast(res.message || 'Not saved', 'err');
      }
    } catch (e) {
      if (!(e instanceof AuthError)) toast(errText(e), 'err');
    } finally {
      saving = false;
      saveBtn.textContent = 'Save';
      if (!gone) updateBar();
    }
  }

  async function discard() {
    const n = changeCount(dirty());
    if (n > 1 && !(await confirmDialog({ title: 'Discard changes?', message: `${n} unsaved changes will be lost.`, ok: 'Discard', danger: true }))) return;
    S.draft = clone(S.orig);
    invalid.clear();
    render();
  }

  async function restart() {
    const n = changeCount(dirty());
    const ok = await confirmDialog({
      title: 'Restart Cerberus?',
      message: `Cerberus stops cleanly and starts again with the saved settings; this page reconnects by itself.${n ? ` Your ${n} unsaved change${n === 1 ? '' : 's'} will be lost.` : ''} Running agent sessions may be interrupted.`,
      ok: 'Restart',
    });
    if (!ok) return;
    await restartAndReload();
  }

  // ---------- lifecycle ----------
  // The embedded Security panel refreshes itself.
  const onVisible = () => { if (store.can('settings') && !PANELS.includes(want) && !changeCount(dirty()) && !invalid.size) load({ schema: false }); };
  const onUnload = (e) => { if (changeCount(dirty())) { e.preventDefault(); e.returnValue = ''; } };
  window.addEventListener('cerberus:visible', onVisible);
  window.addEventListener('beforeunload', onUnload);

  if (!store.can('settings')) {
    // Not an admin: the config is not theirs to read; only Permissions shows.
    renderNav();
    renderMain();
  } else if (S.schema && S.orig) {
    render();
    // Fresh data unless there are unsaved edits to keep.
    if (!changeCount(dirty())) load({ schema: false });
  } else {
    renderMain();
    load();
  }

  return () => {
    gone = true;
    if (secUnmount) secUnmount();
    window.removeEventListener('cerberus:visible', onVisible);
    window.removeEventListener('beforeunload', onUnload);
  };
}

function defaultNote(f) {
  if (f.default === undefined || f.default === null || f.readOnly) return '';
  if (f.kind === 'list' || f.kind === 'map') return '';
  if (f.kind === 'bool') return ` Default: ${f.default ? 'on' : 'off'}.`;
  if (f.kind === 'duration-seconds') return ` Default: ${humanDuration(f.default, '0')}.`;
  if (f.default === '') return '';
  return ` Default: ${f.default}${f.unit ? ' ' + f.unit : ''}.`;
}

function singular(label) {
  const l = String(label || 'entry').toLowerCase();
  if (l.endsWith('ies')) return l.slice(0, -3) + 'y';
  if (l.endsWith('ses') || l.endsWith('xes')) return l.slice(0, -2);
  return l.endsWith('s') ? l.slice(0, -1) : l;
}

// restartAndReload asks the daemon to restart, waits until a new process
// answers (its uptime restarted), then reloads the page. Attempts back off
// from 1 s to 5 s and give up after 90 s (reloading anyway).
export async function restartAndReload() {
  let before = null;
  try { before = (await api('/api/status'))?.uptime ?? null; } catch { /* still try */ }
  const t0 = Date.now();
  let closeOverlay = () => {};
  openDialog((close) => {
    closeOverlay = close;
    return {
      title: 'Restarting',
      sub: 'This page reloads when Cerberus is back',
      body: h('p', { class: 'dlg-msg set-restarting', text: 'Waiting for Cerberus to come back…' }),
    };
  });
  try {
    await post('/api/config/restart', {});
  } catch (e) {
    if (e instanceof AuthError) return;
    // The connection may drop as the daemon goes down; keep waiting.
  }
  let sawDown = false;
  for (let n = 0; Date.now() - t0 < RESTART_GIVE_UP_MS; n++) {
    await new Promise((r) => setTimeout(r, reconnectDelay(n)));
    try {
      const r = await fetch('/api/status', { credentials: 'same-origin', cache: 'no-store' });
      if (r.status === 401) break;
      if (!r.ok) { sawDown = true; continue; }
      const st = await r.json().catch(() => ({}));
      const elapsed = (Date.now() - t0) / 1000;
      if (restarted(before, elapsed, st.uptime) || (before == null && sawDown)) break;
    } catch {
      sawDown = true;
    }
  }
  closeOverlay();
  location.reload();
}
