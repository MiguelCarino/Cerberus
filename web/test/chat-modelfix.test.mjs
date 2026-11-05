// The chat's model-problem helpers: which failures get the fix card, and
// when the input warns before a message that could not be answered.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { modelProblem, needsModelPick } from '../dist/assets/js/chat.js';

test('modelProblem names model failures and nothing else', () => {
  const cases = [
    ["the server said 400: No models loaded. Please load a model in the developer page or use the 'lms load' command.", { kind: 'not-loaded' }],
    ['the server said 404: unknown model: default (set engines.default_model, or pick a model)', { kind: 'no-default' }],
    ['unknown model: openrouter/foo', { kind: 'unknown', model: 'openrouter/foo' }],
    ["model 'llama3.2:3b' not found", { kind: 'not-loaded' }],
    ['model_not_found', { kind: 'not-loaded' }],
    ['context length exceeded', null],
    ['the server said 401: bad key', null],
    ['', null],
    [undefined, null],
  ];
  for (const [text, want] of cases) assert.deepEqual(modelProblem(text), want, String(text));
});

const m = (id, local, loaded) => ({ ID: id, Local: local, Loaded: loaded });

test('needsModelPick: only when "default" cannot be guessed', () => {
  // two local models, none loaded, no default set: the next message would fail
  assert.equal(needsModelPick({ default: 'default', models: [m('a', true, false), m('b', true, false)] }, 'default'), true);
  assert.equal(needsModelPick({ default: 'default', models: [m('a', true, false), m('b', true, false)] }, ''), true);
  // one loaded: the gateway picks it
  assert.equal(needsModelPick({ default: 'default', models: [m('a', true, true), m('b', true, false)] }, 'default'), false);
  // two loaded: ambiguous again
  assert.equal(needsModelPick({ default: 'default', models: [m('a', true, true), m('b', true, true)] }, 'default'), true);
  // a single model of any kind is the default
  assert.equal(needsModelPick({ default: 'default', models: [m('p/x', false, false)] }, 'default'), false);
  // a default is set, or the chat names its model
  assert.equal(needsModelPick({ default: 'a', models: [m('a', true, false), m('b', true, false)] }, 'default'), false);
  assert.equal(needsModelPick({ default: 'default', models: [m('a', true, false), m('b', true, false)] }, 'b'), false);
  // nothing known yet
  assert.equal(needsModelPick(null, 'default'), true);
});
