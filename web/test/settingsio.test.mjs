import test from 'node:test';
import assert from 'node:assert/strict';
import {
  SECTIONS, exportURL, readBundle, valueText, changeRows, downloadRequest, modelState, modelCounts, sizeText,
  summaryText, dropModels, markRegistry, sectionLabel, refreshModels,
} from '../dist/assets/js/settingsio.js';

test('sections match the server bundle sections', () => {
  const ids = SECTIONS.map(([id]) => id).sort();
  assert.deepEqual(ids, ['agents', 'budgets', 'files', 'limits', 'models', 'monitor', 'policy', 'prices', 'providers', 'repos', 'sandbox', 'tariff']);
  assert.equal(sectionLabel('models'), 'Models');
  assert.equal(sectionLabel('odd'), 'odd');
});

test('exportURL', () => {
  assert.equal(exportURL(), '/api/config/export');
  assert.equal(exportURL(SECTIONS.map(([id]) => id), true), '/api/config/export?secret_names=1');
  assert.equal(exportURL(['repos', 'models', 'bogus']), '/api/config/export?sections=models%2Crepos');
  assert.equal(exportURL([]), '/api/config/export');
});

test('readBundle checks the file', () => {
  assert.ok(readBundle('{"version":1,"kind":"cerberus-profile"}').bundle);
  assert.ok(readBundle('{"version":1}').bundle);
  assert.match(readBundle('nope').error, /not JSON/);
  assert.match(readBundle('[]').error, /not a Cerberus/);
  assert.match(readBundle('{"version":1,"kind":"topo"}').error, /"topo"/);
  assert.match(readBundle('{"kind":"cerberus-profile"}').error, /no settings version/);
  assert.match(readBundle('{"version":7}').error, /newer Cerberus/);
});

test('valueText and changeRows', () => {
  assert.equal(valueText(null), '(none)');
  assert.equal(valueText([]), '(none)');
  assert.equal(valueText(42), '42');
  assert.equal(valueText([{ id: 'a' }, { id: 'b' }]), '2 · a, b');
  assert.equal(valueText('x'.repeat(100), 10), `${'x'.repeat(9)}…`);
  const rows = changeRows([{ path: 'models', old: [], new: [{ id: 'm' }], restart: true }, { path: 'vault.key', sensitive: true }]);
  assert.deepEqual(rows, [
    { path: 'models', from: '(none)', to: '1 · m', restart: true },
    { path: 'vault.key', from: '(hidden)', to: '(hidden)', restart: false },
  ]);
  assert.deepEqual(changeRows(null), []);
});

test('model states and download requests', () => {
  const ms = [
    { id: 'a', backend: 'ollama', ref: 'a', present: true },
    { id: 'b', backend: 'llamacpp', ref: 'https://huggingface.co/x/y/resolve/main/b.gguf', registered: true },
    { id: 'c', backend: 'lmstudio', ref: 'c' },
    { id: 'd', backend: 'llamacpp', registered: true },
    { id: 'e', backend: 'openai', ref: 'e' },
  ];
  assert.deepEqual(ms.map(modelState), ['present', 'missing', 'missing', 'orphan', 'orphan']);
  assert.deepEqual(downloadRequest(ms[1]), { engine: 'llamacpp', ref: ms[1].ref });
  assert.deepEqual(downloadRequest(ms[2]), { engine: 'lmstudio', ref: 'c' });
  assert.equal(downloadRequest(ms[3]), null);
  assert.equal(downloadRequest(null), null);
  assert.deepEqual(modelCounts(ms), { total: 5, present: 1, missing: 2, orphan: 2, registered: 2 });
  assert.deepEqual(dropModels(ms, ['a', 'b']).map((m) => m.id), ['c', 'd', 'e']);
  assert.equal(markRegistry(ms, ['b']).find((m) => m.id === 'b').registered, false);
  assert.equal(ms[1].registered, true, 'markRegistry copies');
});

test('sizeText and summaryText', () => {
  assert.equal(sizeText(0), '');
  assert.equal(sizeText(512), '512 MB');
  assert.equal(sizeText(4813), '4.7 GB');
  assert.equal(sizeText(20480), '20 GB');
  assert.equal(summaryText({ sections: ['models', 'agents'], changes: [] }), 'Holds Models, Agents. Your settings already match it.');
  assert.equal(summaryText({ sections: ['models'], changes: [{}] }), 'Holds Models. 1 setting changes.');
  assert.equal(summaryText({ sections: [], changes: [{}, {}] }), 'Holds nothing. 2 settings change.');
});

test('refreshModels keeps removed rows gone', () => {
  const rows = [{ id: 'a', present: false, registered: true }, { id: 'c', present: false }];
  const fresh = [{ id: 'a', present: true, registered: true }, { id: 'b', present: false, registered: true }, { id: 'c', present: false }];
  assert.deepEqual(refreshModels(rows, fresh), [{ id: 'a', present: true, registered: true }, { id: 'c', present: false, registered: false }]);
});
