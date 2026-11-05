// Unit tests for the Settings view's pure helpers (day phase 4): schema
// normalising and grouping, paths, the dirty diff, parse and format by
// kind, durations, search, problems, list and map editing, the first-run
// welcome's changes and the restart check.
// Run: node --test web/test/*.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  GROUP_ORDER, slug, normField, normSchema, visible, groupSchema, getAt, setAt, clone, itemRel, scalarItem, mapKeyPath,
  same, dirtyDiff, changeCount, needsRestart, parseDuration, formatDuration, humanDuration, formatValue, parseValue,
  unitLabel, filterFields, normProblems, problemsAt, problemsUnder, fieldForProblem, problemCounts, newItem, moveItem,
  removeItem, itemTitle, mapEntries, entriesToMap, secretChoices, normConfigResponse, initDone, portOf, welcomeAddress,
  welcomeChanges, welcomePhoneError, ntfyTopic, restarted, reconnectDelay,
} from '../dist/assets/js/settings.js';
import { parseRoute } from '../dist/assets/js/util.js';

// A schema shaped like GET /api/config/schema (JSON tags).
const RAW = [
  { path: 'server.listen', label: 'Listen address', help: 'Where the web UI listens.', group: 'Server', level: 'basic', kind: 'string', format: 'address', restart: true, required: true, default: '127.0.0.1:7340' },
  { path: 'server.token', label: 'Admin token', help: 'secret', group: 'Server', level: 'advanced', kind: 'string', sensitive: true, restart: true },
  { path: 'server.init_done', label: 'Setup finished', help: '', group: 'Server', level: 'advanced', kind: 'bool' },
  { path: 'server.token_created', label: 'Token created', group: 'Server', level: 'advanced', kind: 'time', read_only: true },
  { path: 'engines.idle_ttl_sec', label: 'Unload after idle', help: 'A loaded model is unloaded after this long.', group: 'Models', level: 'basic', kind: 'duration-seconds', unit: 's', min: 0, default: 600 },
  { path: 'engines.llamacpp.parallel', label: 'llama.cpp slots', help: 'Concurrent requests.', group: 'Models', level: 'advanced', kind: 'int', min: 1, max: 64, restart: true },
  { path: 'monitor.psu_efficiency', label: 'Power supply efficiency', help: 'Share of wall power.', group: 'Monitor & energy', level: 'advanced', kind: 'float', min: 0.5, max: 1 },
  { path: 'agent.approve', label: 'Approve mode', help: 'ask or auto', group: 'Agents', level: 'basic', kind: 'enum', enum: ['ask', 'auto'] },
  { path: 'mystery', label: 'Mystery', help: '', group: '', level: 'advanced', kind: 'string' },
  {
    path: 'providers', label: 'API providers', help: 'Cloud APIs.', group: 'Gateway & spend', level: 'advanced', kind: 'list', restart: true,
    items: [
      { path: 'providers[].name', label: 'Name', kind: 'string', required: true },
      { path: 'providers[].base_url', label: 'Base URL', kind: 'string', format: 'url' },
      { path: 'providers[].enabled', label: 'On', kind: 'bool' },
      { path: 'providers[].key_secret', label: 'Key secret', kind: 'secret-ref' },
      { path: 'providers[].models', label: 'Models', kind: 'list', items: [{ path: 'providers[].models[]', label: 'Models', kind: 'string' }] },
    ],
  },
  { path: 'repo_roots', label: 'Repository folders', help: 'Folders.', group: 'Sessions', level: 'advanced', kind: 'list', items: [{ path: 'repo_roots[]', label: 'Folder', kind: 'path' }] },
  {
    path: 'prices', label: 'Prices', help: 'USD per million tokens.', group: 'Gateway & spend', level: 'advanced', kind: 'map',
    items: [{ path: 'prices.*.in', label: 'Input', kind: 'float' }, { path: 'prices.*.out', label: 'Output', kind: 'float' }],
  },
  { path: 'budgets', label: 'Budgets', help: 'Spend caps.', group: 'Gateway & spend', level: 'basic', kind: 'list',
    items: [{ path: 'budgets[].provider', label: 'Provider', kind: 'string' }, { path: 'budgets[].period', label: 'Period', kind: 'enum', enum: ['day', 'month'] }, { path: 'budgets[].soft_usd', label: 'Warn at', kind: 'float' }] },
  { path: 'tariff.tiers', label: 'Tiers', help: 'Monthly price steps.', group: 'Monitor & energy', level: 'basic', kind: 'list',
    items: [{ path: 'tariff.tiers[].name', label: 'Name', kind: 'string' }, { path: 'tariff.tiers[].up_to_kwh', label: 'Up to', kind: 'float', unit: 'kWh' }] },
];
const S = normSchema(RAW);
const F = (p) => S.find((f) => f.path === p);

test('parseRoute: settings and its groups; security still parses (app.js redirects it)', () => {
  assert.deepEqual(parseRoute('#/settings'), { view: 'settings', group: '' });
  assert.deepEqual(parseRoute('#/settings/gateway-spend'), { view: 'settings', group: 'gateway-spend' });
  assert.deepEqual(parseRoute('#/settings/security'), { view: 'settings', group: 'security' });
  assert.deepEqual(parseRoute('#/security'), { view: 'security' });
});

test('slug', () => {
  assert.equal(slug('Gateway & spend'), 'gateway-spend');
  assert.equal(slug('Monitor & energy'), 'monitor-energy');
  assert.equal(slug('Server'), 'server');
});

test('normField: Go names, defaults, read-only time, nested items', () => {
  const f = normField({ Path: 'a.b', Label: 'A', Kind: 'int', Level: 'basic', Min: 1, Unit: 'MiB', Restart: true, ReadOnly: true });
  assert.equal(f.path, 'a.b');
  assert.equal(f.level, 'basic');
  assert.equal(f.min, 1);
  assert.equal(f.max, null);
  assert.equal(f.readOnly, true);
  assert.equal(normField({ path: 't', kind: 'time' }).readOnly, true);
  assert.equal(normField({ path: 'x' }).kind, 'string');
  assert.equal(normField({ path: 'x', level: 'weird' }).level, 'advanced');
  assert.equal(F('providers').items.length, 5);
  assert.deepEqual(normSchema({ fields: RAW.slice(0, 1) }).map((x) => x.path), ['server.listen']);
  assert.deepEqual(normSchema(null), []);
});

test('groupSchema: display order, basic/advanced split, hidden fields, ungrouped → Advanced', () => {
  const g = groupSchema(S);
  const names = g.map((x) => x.group);
  assert.deepEqual(names, ['Server', 'Models', 'Gateway & spend', 'Sessions', 'Agents', 'Monitor & energy', 'Advanced']);
  for (let i = 1; i < names.length; i++) assert.ok(GROUP_ORDER.indexOf(names[i - 1]) < GROUP_ORDER.indexOf(names[i]));
  const server = g[0];
  assert.deepEqual(server.basic.map((f) => f.path), ['server.listen']);
  assert.deepEqual(server.advanced.map((f) => f.path), ['server.token_created']); // no token, no init_done
  assert.equal(visible(F('server.token')), false);
  assert.equal(visible(F('server.init_done')), false);
  assert.deepEqual(g.find((x) => x.group === 'Advanced').advanced.map((f) => f.path), ['mystery']);
  assert.equal(g.find((x) => x.group === 'Gateway & spend').slug, 'gateway-spend');
});

test('getAt / setAt / clone', () => {
  const o = { a: { b: 1 } };
  assert.equal(getAt(o, 'a.b'), 1);
  assert.equal(getAt(o, 'a.x.y'), undefined);
  assert.equal(getAt(null, 'a'), undefined);
  setAt(o, 'a.c.d', 2);
  assert.deepEqual(o, { a: { b: 1, c: { d: 2 } } });
  const c = clone(o);
  c.a.b = 9;
  assert.equal(o.a.b, 1);
  assert.equal(clone(undefined), undefined);
});

test('itemRel, scalarItem, mapKeyPath', () => {
  const p = F('providers');
  assert.equal(itemRel(p, p.items[0]), 'name');
  assert.equal(itemRel(F('prices'), F('prices').items[1]), 'out');
  assert.equal(itemRel(F('repo_roots'), F('repo_roots').items[0]), '');
  assert.equal(itemRel({ path: 'repos' }, { path: 'repos[].preview.dir' }), 'preview.dir');
  assert.equal(scalarItem(F('repo_roots')).kind, 'path');
  assert.equal(scalarItem(p), null);
  assert.equal(mapKeyPath('prices', 'gpt-4o'), 'prices.gpt-4o');
  assert.equal(mapKeyPath('prices', 'qwen2.5'), 'prices["qwen2.5"]');
  assert.equal(mapKeyPath('prices', 'openrouter/*'), 'prices["openrouter/*"]');
});

test('same: empty values are equal; deep compare', () => {
  assert.ok(same(null, []));
  assert.ok(same(undefined, {}));
  assert.ok(same([], {}));
  assert.ok(same({ a: [1, { b: 2 }] }, { a: [1, { b: 2 }] }));
  assert.ok(same({ a: 1, b: null }, { a: 1 }));
  assert.ok(!same([1, 2], [2, 1]));
  assert.ok(!same(0, ''));
  assert.ok(!same(false, null));
  assert.ok(!same('a', ['a']));
});

test('dirtyDiff: one entry per top-level setting, never hidden or read-only', () => {
  const orig = {
    server: { listen: '127.0.0.1:7340', token_created: '2026-01-01T00:00:00Z', init_done: false },
    engines: { idle_ttl_sec: 600 }, providers: null, prices: {},
  };
  const draft = clone(orig);
  assert.deepEqual(dirtyDiff(orig, draft, S), {});
  draft.engines.idle_ttl_sec = 300;
  draft.server.token_created = 'x';
  draft.server.init_done = true;
  draft.providers = [];
  draft.prices = { 'gpt-4o': { in: 2.5, out: 10 } };
  const d = dirtyDiff(orig, draft, S);
  assert.deepEqual(d, { 'engines.idle_ttl_sec': 300, prices: { 'gpt-4o': { in: 2.5, out: 10 } } });
  assert.equal(changeCount(d), 2);
  assert.equal(changeCount(null), 0);
  // The diff holds copies.
  draft.prices['gpt-4o'].in = 1;
  assert.equal(d.prices['gpt-4o'].in, 2.5);
  // Removing a setting sends null.
  assert.deepEqual(dirtyDiff({ mystery: 'x' }, {}, S), { mystery: null });
});

test('needsRestart', () => {
  assert.equal(needsRestart({ 'engines.idle_ttl_sec': 1 }, S), false);
  assert.equal(needsRestart({ 'engines.idle_ttl_sec': 1, providers: [] }, S), true);
  assert.equal(needsRestart({}, S), false);
});

test('parseDuration', () => {
  assert.equal(parseDuration('90'), 90);
  assert.equal(parseDuration('10m'), 600);
  assert.equal(parseDuration('1h30m'), 5400);
  assert.equal(parseDuration('1h 30 min'), 5400);
  assert.equal(parseDuration('1.5h'), 5400);
  assert.equal(parseDuration('2 days'), 172800);
  assert.equal(parseDuration(' 45S '), 45);
  assert.equal(parseDuration(''), null);
  assert.equal(parseDuration('10 parsecs'), null);
  assert.equal(parseDuration('m10'), null);
  assert.equal(parseDuration('10m x'), null);
  assert.equal(parseDuration('-5'), null);
});

test('formatDuration and humanDuration', () => {
  assert.equal(formatDuration(0), '0');
  assert.equal(formatDuration(45), '45s');
  assert.equal(formatDuration(600), '10m');
  assert.equal(formatDuration(5400), '1h30m');
  assert.equal(formatDuration(90061), '1d1h1m1s');
  assert.equal(parseDuration(formatDuration(90061)), 90061);
  assert.equal(humanDuration(0), 'off');
  assert.equal(humanDuration(0, 'never'), 'never');
  assert.equal(humanDuration(1), '1 second');
  assert.equal(humanDuration(600), '10 minutes');
  assert.equal(humanDuration(5400), '1 hour 30 minutes');
  assert.equal(humanDuration(90061), '1 day 1 hour'); // two parts at most
});

test('formatValue by kind', () => {
  assert.equal(formatValue(F('engines.idle_ttl_sec'), 600), '10m');
  assert.equal(formatValue(F('engines.llamacpp.parallel'), 4), '4');
  assert.equal(formatValue(F('server.listen'), null), '');
  assert.equal(formatValue({ kind: 'bool' }, true), 'on');
  assert.equal(formatValue(F('server.token_created'), '0001-01-01T00:00:00Z'), '');
  assert.equal(formatValue(F('server.token_created'), '2026-11-01T00:00:00Z'), '2026-11-01T00:00:00Z');
  assert.equal(formatValue({ kind: 'list' }, ['a']), '["a"]');
});

test('parseValue: int with range', () => {
  const f = F('engines.llamacpp.parallel');
  assert.deepEqual(parseValue(f, '4'), { value: 4 });
  assert.deepEqual(parseValue(f, ' 64 '), { value: 64 });
  assert.match(parseValue(f, '65').error, /At most 64/);
  assert.match(parseValue(f, '0').error, /At least 1/);
  assert.match(parseValue(f, '4.5').error, /whole number/);
  assert.match(parseValue(f, 'four').error, /whole number/);
  assert.match(parseValue(f, '').error, /At least 1/); // empty is 0
});

test('parseValue: float, comma decimals, range', () => {
  const f = F('monitor.psu_efficiency');
  assert.deepEqual(parseValue(f, '0.9'), { value: 0.9 });
  assert.deepEqual(parseValue(f, '0,9'), { value: 0.9 });
  assert.deepEqual(parseValue(f, '.75'), { value: 0.75 });
  assert.match(parseValue(f, '1.5').error, /At most 1/);
  assert.match(parseValue(f, '1e3').error, /A number/);
  assert.match(parseValue(f, 'abc').error, /A number/);
});

test('parseValue: duration, bool, enum, strings', () => {
  const d = F('engines.idle_ttl_sec');
  assert.deepEqual(parseValue(d, '10m'), { value: 600 });
  assert.deepEqual(parseValue(d, '0'), { value: 0 });
  assert.deepEqual(parseValue(d, ''), { value: 0 });
  assert.match(parseValue(d, 'soon').error, /duration/);
  assert.match(parseValue({ kind: 'duration-seconds', min: 5, max: 3600 }, '2').error, /At least 5 seconds/);
  assert.match(parseValue({ kind: 'duration-seconds', min: 5, max: 3600 }, '2h').error, /At most 1 hour/);
  assert.deepEqual(parseValue({ kind: 'bool' }, true), { value: true });
  assert.deepEqual(parseValue({ kind: 'bool' }, 'off'), { value: false });
  assert.ok(parseValue({ kind: 'bool' }, 'maybe').error);
  const e = F('agent.approve');
  assert.deepEqual(parseValue(e, 'auto'), { value: 'auto' });
  assert.match(parseValue(e, 'never').error, /ask, auto/);
  const l = F('server.listen');
  assert.deepEqual(parseValue(l, ' 0.0.0.0:7340 '), { value: '0.0.0.0:7340' });
  assert.match(parseValue(l, '').error, /Required/);
  assert.deepEqual(parseValue(F('mystery'), ''), { value: '' });
});

test('unitLabel: duration shows a friendly reading instead of "s"', () => {
  assert.equal(unitLabel(F('engines.idle_ttl_sec')), '');
  assert.equal(unitLabel({ kind: 'int', unit: 'MiB' }), 'MiB');
});

test('filterFields: every word, label/help/path/group/items, hidden never', () => {
  assert.deepEqual(filterFields(S, ''), []);
  assert.deepEqual(filterFields(S, 'unload').map((f) => f.path), ['engines.idle_ttl_sec']);
  assert.deepEqual(filterFields(S, 'LISTEN web').map((f) => f.path), ['server.listen']);
  assert.deepEqual(filterFields(S, 'base url').map((f) => f.path), ['providers']); // an item label
  assert.deepEqual(filterFields(S, 'energy tiers').map((f) => f.path), ['tariff.tiers']); // group + label
  assert.deepEqual(filterFields(S, 'token secret'), []); // the sensitive token
  assert.deepEqual(filterFields(S, 'listen nothing-matches'), []);
});

test('normProblems / problemsAt / problemsUnder / fieldForProblem / problemCounts', () => {
  const ps = normProblems([
    { Path: 'providers[1].base_url', Message: 'not a URL', Level: 'error' },
    { path: 'repo_roots[0]', message: 'does not exist', level: 'warn' },
    { path: 'prices["qwen2.5"].in', message: 'negative', level: 'error' },
    { path: 'server.listen', message: 'bad', level: 'error' },
    { path: 'nowhere', message: 'odd', level: 'warn' },
    { path: 'x', message: '' },
  ]);
  assert.equal(ps.length, 5);
  assert.equal(ps[1].level, 'warn');
  assert.deepEqual(problemsAt(ps, 'providers[1].base_url').map((p) => p.message), ['not a URL']);
  assert.deepEqual(problemsUnder(ps, 'providers').map((p) => p.message), ['not a URL']);
  assert.deepEqual(problemsUnder(ps, 'prices').map((p) => p.message), ['negative']);
  assert.deepEqual(problemsUnder(ps, 'server').map((p) => p.message), ['bad']);
  assert.equal(fieldForProblem(S, 'providers[1].base_url').path, 'providers');
  assert.equal(fieldForProblem(S, 'prices["qwen2.5"].in').path, 'prices');
  assert.equal(fieldForProblem(S, 'server.listen').path, 'server.listen');
  assert.equal(fieldForProblem(S, 'server.listenx'), null);
  assert.deepEqual(problemCounts(S, ps), {
    'Gateway & spend': { error: 2, warn: 0 }, Sessions: { error: 0, warn: 1 }, Server: { error: 1, warn: 0 }, Advanced: { error: 0, warn: 1 },
  });
  assert.deepEqual(normProblems(null), []);
});

test('newItem: zero values plus list starters', () => {
  assert.deepEqual(newItem(F('providers')), { name: '', base_url: '', enabled: true, key_secret: '', models: [] });
  assert.deepEqual(newItem(F('budgets')), { provider: '*', period: 'month', soft_usd: 0 });
  assert.deepEqual(newItem(F('tariff.tiers')), { name: '', up_to_kwh: 0 });
  assert.equal(newItem(F('repo_roots')), '');
  assert.deepEqual(newItem(F('prices')), { in: 0, out: 0 });
  assert.deepEqual(newItem({ path: 'repos', items: [{ path: 'repos[].preview.dir', kind: 'string' }] }), { preview: { dir: '' } });
});

test('moveItem / removeItem', () => {
  assert.deepEqual(moveItem(['a', 'b', 'c'], 0, 1), ['b', 'a', 'c']);
  assert.deepEqual(moveItem(['a', 'b', 'c'], 2, -1), ['a', 'c', 'b']);
  assert.deepEqual(moveItem(['a', 'b'], 0, -1), ['a', 'b']);
  assert.deepEqual(moveItem(['a', 'b'], 1, 1), ['a', 'b']);
  assert.deepEqual(moveItem(null, 0, 1), []);
  const src = ['a', 'b'];
  moveItem(src, 0, 1);
  assert.deepEqual(src, ['a', 'b']); // a copy
  assert.deepEqual(removeItem(['a', 'b', 'c'], 1), ['a', 'c']);
  assert.deepEqual(removeItem(undefined, 0), []);
});

test('itemTitle', () => {
  assert.equal(itemTitle({ name: 'openrouter' }, 0), 'openrouter');
  assert.equal(itemTitle({ id: 'qwen' }, 0), 'qwen');
  assert.equal(itemTitle({ provider: '*', period: 'month' }, 0), '* / month');
  assert.equal(itemTitle({ name: '', up_to_kwh: 150 }, 0), 'up to 150 kWh');
  assert.equal(itemTitle({ name: '', up_to_kwh: 0 }, 2), 'the rest');
  assert.equal(itemTitle({}, 2), 'Item 3');
  assert.equal(itemTitle('x', 0), 'Item 1');
});

test('mapEntries / entriesToMap', () => {
  assert.deepEqual(mapEntries({ b: 1, a: 2 }), [['a', 2], ['b', 1]]);
  assert.deepEqual(mapEntries(null), []);
  assert.deepEqual(entriesToMap([['A', 1], [' B ', 2], ['', 3]]), { map: { A: 1, B: 2 } });
  const dup = entriesToMap([['A', 1], ['A', 2]]);
  assert.match(dup.error, /appears twice/);
});

test('secretChoices keeps a missing current value', () => {
  assert.deepEqual(secretChoices(['b', 'a', 'a'], 'a'), [{ name: 'a', missing: false }, { name: 'b', missing: false }]);
  assert.deepEqual(secretChoices(['a'], 'gone'), [{ name: 'gone', missing: true }, { name: 'a', missing: false }]);
  assert.deepEqual(secretChoices(null, ''), []);
});

test('normConfigResponse / initDone', () => {
  const r = normConfigResponse({ config: { server: { init_done: false } }, problems: [{ path: 'a', message: 'm', level: 'warn' }], restart_pending: true });
  assert.equal(r.restartPending, true);
  assert.equal(r.problems.length, 1);
  assert.equal(initDone(r.config), false);
  assert.equal(initDone({ server: { init_done: true } }), true);
  assert.equal(initDone({ server: {} }), true); // older daemons: no welcome
  assert.deepEqual(normConfigResponse(null), { config: null, problems: [], restartPending: false });
});

test('portOf / welcomeAddress', () => {
  assert.equal(portOf('127.0.0.1:7340'), '7340');
  assert.equal(portOf('[::1]:8080'), '8080');
  assert.equal(portOf(''), '7340');
  assert.equal(welcomeAddress('192.168.1.20', '127.0.0.1:7381'), '192.168.1.20:7381');
  assert.equal(welcomeAddress('100.64.0.1:9000', '127.0.0.1:7340'), '100.64.0.1:9000');
  assert.equal(welcomeAddress('http://desk.local/', '127.0.0.1:7340'), 'desk.local:7340');
  assert.equal(welcomeAddress('fd7a:115c::1', '127.0.0.1:7340'), '[fd7a:115c::1]:7340');
  assert.equal(welcomeAddress('[fd7a::1]', '127.0.0.1:7340'), '[fd7a::1]:7340');
  assert.equal(welcomeAddress('  ', '127.0.0.1:7340'), '');
});

test('welcomeChanges: only what differs', () => {
  const cfg = { server: { listen: '127.0.0.1:7340', extra_listen: null }, engines: { default_model: '' }, notify: { ntfy_url: '' }, tariff: { household_kwh_month: 0 } };
  assert.deepEqual(welcomeChanges({ phone: 'none' }, cfg), {}); // null ≡ []
  assert.deepEqual(welcomeChanges({ model: 'qwen', phone: 'tailscale', address: '100.64.0.1', ntfy: 'my-topic', kwh: '250,5' }, cfg), {
    'engines.default_model': 'qwen',
    'server.extra_listen': ['100.64.0.1:7340'],
    'notify.ntfy_url': 'https://ntfy.sh/my-topic',
    'tariff.household_kwh_month': 250.5,
  });
  // An address already there is not added twice; "keep" changes nothing.
  const cfg2 = { ...cfg, server: { listen: '127.0.0.1:7340', extra_listen: ['100.64.0.1:7340'] } };
  assert.deepEqual(welcomeChanges({ phone: 'tailscale', address: '100.64.0.1' }, cfg2), {});
  assert.deepEqual(welcomeChanges({ phone: 'keep' }, cfg2), {});
  assert.deepEqual(welcomeChanges({ phone: 'none' }, cfg2), { 'server.extra_listen': [] });
  // LAN without an address, bad kWh and empty answers change nothing.
  assert.deepEqual(welcomeChanges({ phone: 'lan', address: '', kwh: 'lots', ntfy: '  ' }, cfg), {});
  assert.deepEqual(welcomeChanges({ ntfy: 'https://ntfy.example/t' }, cfg), { 'notify.ntfy_url': 'https://ntfy.example/t' });
  assert.deepEqual(welcomeChanges(null, cfg), {});
});

test('ntfyTopic', () => {
  assert.equal(ntfyTopic(new Uint8Array([0, 15, 255])), 'https://ntfy.sh/cerberus-000fff');
});

test('restarted / reconnectDelay', () => {
  assert.equal(restarted(1000, 5, 1004), false); // the old process, still up
  assert.equal(restarted(1000, 5, 1), true);
  assert.equal(restarted(3, 4, 5), false); // old process with a short uptime
  assert.equal(restarted(3, 10, 2), true);
  assert.equal(restarted(null, 5, 1), false);
  assert.equal(restarted(1000, 5, undefined), false);
  assert.equal(reconnectDelay(0), 1000);
  assert.equal(reconnectDelay(1), 1500);
  assert.equal(reconnectDelay(20), 5000);
});

test('welcomePhoneError: home network and Tailscale need an address', () => {
  const L = '127.0.0.1:7340';
  assert.equal(welcomePhoneError({ phone: 'none', address: '' }, L), '');
  assert.equal(welcomePhoneError({ phone: 'keep', address: '' }, L), '');
  assert.match(welcomePhoneError({ phone: 'tailscale', address: '' }, L), /tailscale ip -4/);
  assert.match(welcomePhoneError({ phone: 'lan', address: '   ' }, L), /LAN IP/);
  assert.equal(welcomePhoneError({ phone: 'lan', address: '192.168.1.20' }, L), '');
  assert.equal(welcomePhoneError({ phone: 'tailscale', address: '100.101.102.103:7340' }, L), '');
  assert.equal(welcomePhoneError({ phone: 'lan', address: 'fe80::1' }, L), '');
  assert.equal(welcomePhoneError({ phone: 'lan', address: 'box.lan' }, L), '');
  assert.match(welcomePhoneError({ phone: 'lan', address: 'my pc' }, L), /not an address/);
  assert.match(welcomePhoneError({ phone: 'lan', address: '10.0.0.1:99999' }, L), /not an address/);
});
