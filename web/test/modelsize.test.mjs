// Model size hints of the flow editor (web/dist/assets/js/modelsize.js),
// the mirror of internal/flows/modelsize.go.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { paramsB, isSmallModel, smallRole, pickerLabel } from '../dist/assets/js/modelsize.js';

test('paramsB reads the size in a model id', () => {
  const cases = {
    'lfm3-1.7b': 1.7,
    'qwen/qwen3.8-27b': 27,
    'gemma-4-E4B': 4,
    'google/gemma-4-e4b-it': 4,
    'qwen3-0.6b': 0.6,
    'llama-3.1-8b-instruct': 8,
    'qwen3-30b-a3b': 30,
    'mixtral-8x7b': 7,
    'smollm-360m': 0.36,
    'lfm3-1.7b-q8_0.gguf': 1.7,
    'MiniMax-M2.7': 0,
    'GLM-4.7-Flash': 0,
    'qwen3.8': 0,
    default: 0,
    '': 0,
  };
  for (const [id, want] of Object.entries(cases)) assert.equal(paramsB(id), want, id);
  assert.equal(paramsB(undefined), 0);
});

test('isSmallModel: the name first, then the file size', () => {
  assert.equal(isSmallModel('lfm3-1.7b'), true);
  assert.equal(isSmallModel({ ID: 'gemma-4-e4b', SizeMiB: 5000 }), true);
  assert.equal(isSmallModel({ ID: 'qwen/qwen3.8-27b', SizeMiB: 1000 }), false);
  assert.equal(isSmallModel({ ID: 'tiny-gguf', SizeMiB: 900 }), true);
  assert.equal(isSmallModel({ ID: 'MiniMax-M2.7', SizeMiB: 120000 }), false);
  assert.equal(isSmallModel({ ID: 'mystery' }), false);
  assert.equal(isSmallModel(null), false);
});

test('smallRole and pickerLabel mark small models for plan and judge steps', () => {
  assert.equal(smallRole({ kind: 'plan' }, 'model'), 'planning');
  assert.equal(smallRole({ kind: 'judge' }, 'model'), 'judging');
  assert.equal(smallRole({ kind: 'session' }, 'model'), 'agent work');
  assert.equal(smallRole({ kind: 'prompt' }, 'model'), '');
  assert.equal(smallRole({ kind: 'plan' }, 'template'), '');
  assert.equal(smallRole(null, 'model'), '');
  assert.equal(pickerLabel('lfm3-1.7b · local', { ID: 'lfm3-1.7b' }, 'planning'), 'lfm3-1.7b · local · small for planning');
  assert.equal(pickerLabel('qwen/qwen3.8-27b · local', { ID: 'qwen/qwen3.8-27b' }, 'planning'), 'qwen/qwen3.8-27b · local');
  assert.equal(pickerLabel('lfm3-1.7b · local', { ID: 'lfm3-1.7b' }, ''), 'lfm3-1.7b · local');
});
