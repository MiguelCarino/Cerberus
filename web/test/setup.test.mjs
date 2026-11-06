// Setup snapshot helpers (evals phase E3).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { setupFields, shownFields, diffSetups, setupSummary, cleanSessionId } from '../dist/assets/js/setup.js';

const base = {
  agent: { name: 'cerberus', binary: '/usr/bin/cerberus', command: ['{self}', 'agent', '--model', '{model}'], env_keys: ['A', 'B'],
    version: 'cerberus 1.2.0', sandbox: 'bwrap' },
  model: { requested: 'default', id: 'qwen/qwen3-8b', engine: 'llamacpp', path: '/m/qwen3-8b-Q4_K_M.gguf', quantization: 'Q4_K_M', context: 32768 },
  mcp_servers: [{ name: 'cerberus', command: ['/usr/bin/cerberus', 'mcp'] }],
  workspace: 'repo', base_commit: 'abc123', cerberus_version: '1.2.0 (dev)',
};

test('setupFields flattens in the Go order', () => {
  const f = setupFields(base);
  assert.equal(f.length, 18);
  assert.equal(f[0].field, 'agent.name');
  assert.equal(f[2].value, '{self} agent --model {model}');
  assert.equal(f.find((x) => x.field === 'agent.env_keys').value, 'A,B');
  assert.equal(f.find((x) => x.field === 'model.context').value, '32768');
  assert.equal(f.find((x) => x.field === 'mcp_servers').value, 'cerberus=/usr/bin/cerberus mcp');
  assert.equal(setupFields(null).length, 18);
});

test('shownFields drops empty and false fields', () => {
  const f = shownFields(base).map((x) => x.field);
  assert.ok(!f.includes('agent.resume'));
  assert.ok(!f.includes('agent.interactive'));
  assert.ok(f.includes('base_commit'));
});

test('diffSetups lists only differing fields', () => {
  assert.deepEqual(diffSetups(base, base), []);
  const other = structuredClone(base);
  other.model.id = 'qwen/qwen3-27b';
  other.agent.version = '';
  const d = diffSetups(base, other);
  assert.deepEqual(d.map((x) => x.field), ['agent.version', 'model.id']);
  assert.equal(d[1].b, 'qwen/qwen3-27b');
});

test('setupSummary and cleanSessionId', () => {
  assert.equal(setupSummary(null), 'Setup');
  assert.equal(setupSummary({ hash: 'abc', verified: true }), 'Setup · abc');
  assert.match(setupSummary({ hash: 'abc', recorded_hash: 'def', verified: false }), /changed/);
  assert.equal(cleanSessionId(' s-20261105-abc '), 's-20261105-abc');
  assert.equal(cleanSessionId('https://h/#/s/s1?x=1'), 's1');
  assert.equal(cleanSessionId('../etc'), null);
  assert.equal(cleanSessionId(''), null);
});
