// Form pieces shared by the New task and New fleet task dialogs: labelled
// fields and the agent / model / escalate pickers.

import { api } from '../api.js';
import { h } from '../dom.js';
import { store } from '../store.js';

export function field(label, control) {
  return h('label', { class: 'field' }, h('span', { class: 'field-label', text: label }), control);
}

export function modelLabel(m) {
  const where = m.Local ? (m.Loaded ? 'loaded' : 'local') : 'cloud';
  return `${m.ID} · ${where}`;
}

// loadChoices fetches what the pickers offer: models and agents (from the
// cached status when there is one).
export async function loadChoices() {
  let st = store.status();
  let models;
  [models, st] = await Promise.all([
    api('/api/models').catch(() => []),
    st ? Promise.resolve(st) : store.refreshStatus(),
  ]);
  return { models: models || [], agents: (st && st.agents) || [], defaultAgent: (st && st.default_agent) || '' };
}

// agentPickers builds the agent, model and escalate selects. The model field
// hides for agents that pick their own model (needs_model false); hint shows
// the agent's description. model() is "" when hidden.
export function agentPickers({ agents, models, defaultAgent = '' }) {
  const firstAvail = agents.find((a) => a.available && a.name === defaultAgent) || agents.find((a) => a.available);
  const agent = h('select', { class: 'select' }, agents.map((a) =>
    h('option', { value: a.name, disabled: !a.available, selected: a === firstAvail, text: a.available ? a.name : `${a.name} (not installed)` })));
  const model = h('select', { class: 'select' },
    h('option', { value: '', text: 'Default model' }),
    models.map((m) => h('option', { value: m.ID, text: modelLabel(m) })));
  const escalate = h('select', { class: 'select' },
    h('option', { value: '', text: 'None' }),
    agents.filter((a) => a.available).map((a) => h('option', { value: a.name, text: a.name })));
  const agentField = field('Agent', agent);
  const modelField = field('Model', model);
  const escalateField = field('Escalate to', escalate);
  const hint = h('p', { class: 'field-hint' });
  const sync = () => {
    const a = agents.find((x) => x.name === agent.value);
    modelField.hidden = a ? a.needs_model === false : false;
    hint.textContent = (a && a.description) || '';
  };
  agent.addEventListener('change', sync);
  sync();
  return {
    agentField, modelField, escalateField, hint,
    agent: () => agent.value,
    model: () => (modelField.hidden ? '' : model.value),
    escalate: () => escalate.value,
  };
}

// textWithMic wraps a textarea or input with the mic control (when voice is
// available) in a field: label row with the button, then the control, then
// the explanation shown when the microphone cannot be used.
export function textWithMic(label, control, mic) {
  return h('div', { class: 'field' },
    h('div', { class: 'field-top' }, h('span', { class: 'field-label', text: label }), mic.el),
    control, mic.note);
}
