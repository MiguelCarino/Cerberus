// First-run welcome: while the config's server.init_done is false (a fresh
// config that `cerberus init` has not set up), a 4-step dialog asks for the
// default model, phone access, phone notifications and household
// electricity use, saves the answers in one PATCH /api/config and marks
// setup done (POST /api/config/init-done). "Skip setup" marks it done
// without changes; closing the dialog asks again on the next visit.
// It loads once at start-up and never polls.

import { api, post, AuthError } from '../api.js';
import { h, clear, openDialog, toast, errText } from '../dom.js';
import { normConfigResponse, initDone, getAt, PHONE_MODES, welcomeChanges, welcomePhoneError, ntfyTopic } from '../settings.js';
import { patchConfig } from './settings.js';
import { store } from '../store.js';

const STEPS = ['Model', 'Phone access', 'Notifications', 'Electricity'];
const PHONE_STEP = 1; // index of 'Phone access'

let shown = false;

// maybeWelcome shows the welcome once per page load when setup is not done.
export async function maybeWelcome() {
  // Setup edits the config: admins only (users and roles).
  if (shown || !store.can('settings')) return;
  let config;
  try {
    config = normConfigResponse(await api('/api/config')).config;
  } catch (e) {
    if (!(e instanceof AuthError)) console.warn('welcome: config', errText(e));
    return;
  }
  if (!config || initDone(config)) return;
  shown = true;
  const models = await api('/api/models').catch(() => []);
  openWelcome(config, Array.isArray(models) ? models : []);
}

function guessAddress() {
  const host = location.hostname;
  return host && host !== 'localhost' && !host.startsWith('127.') && host !== '::1' && host !== '[::1]' ? host : '';
}

function openWelcome(config, models) {
  const answers = {
    model: getAt(config, 'engines.default_model') || '',
    phone: (getAt(config, 'server.extra_listen') || []).length ? 'keep' : 'none',
    address: guessAddress(),
    ntfy: getAt(config, 'notify.ntfy_url') || '',
    kwh: getAt(config, 'tariff.household_kwh_month') || '',
  };
  let step = 0;
  let busy = false;

  return openDialog((close) => {
    const body = h('div', { class: 'wel' });
    const err = h('p', { class: 'set-err wel-err', role: 'alert' });
    const back = h('button', { class: 'btn btn-ghost', type: 'button', onclick: () => { step--; paint(); } }, 'Back');
    const next = h('button', { class: 'btn btn-primary', type: 'button', onclick: () => {
      if (step === PHONE_STEP && phoneProblem()) return;
      if (step < STEPS.length - 1) { step++; paint(); } else finish();
    } }, 'Next');
    const skip = h('button', { class: 'btn btn-ghost wel-skip', type: 'button', onclick: () => markDone(true) }, 'Skip setup');

    // phoneProblem shows (and reports) a missing or unusable address for
    // home network or Tailscale access; nothing would be saved otherwise.
    function phoneProblem() {
      const msg = welcomePhoneError(answers, getAt(config, 'server.listen'));
      // Next to the field: the step's own error line sits below the fold
      // on a phone.
      const at = body.querySelector('.wel-addr-err');
      if (at) at.textContent = msg; else err.textContent = msg;
      if (msg) body.querySelector('.wel-addr input')?.focus();
      return !!msg;
    }

    function dots() {
      return h('ol', { class: 'wel-steps', 'aria-label': `Step ${step + 1} of ${STEPS.length}` }, STEPS.map((s, i) =>
        h('li', { class: i === step ? 'on' : i < step ? 'done' : null, 'aria-current': i === step ? 'step' : null, text: s })));
    }

    function paint() {
      err.textContent = '';
      back.hidden = step === 0;
      next.textContent = step === STEPS.length - 1 ? 'Finish' : 'Next';
      clear(body).append(dots(), [stepModel, stepPhone, stepNotify, stepPower][step](), err);
      body.querySelector('input, select')?.focus();
    }

    function stepModel() {
      const ids = models.map((m) => m.ID ?? m.id).filter(Boolean);
      if (answers.model && !ids.includes(answers.model)) ids.unshift(answers.model);
      return h('div', { class: 'wel-step' },
        h('h3', { class: 'wel-title', text: 'Default model' }),
        h('p', { class: 'dlg-msg', text: 'The model used when a request or session names none. Models load on first use and unload when idle.' }),
        ids.length
          ? h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Model' }),
            h('select', { class: 'select', onchange: (e) => { answers.model = e.target.value; } },
              h('option', { value: '', text: '(decide later)' }),
              ids.map((id) => h('option', { value: id, selected: id === answers.model, text: id }))))
          : h('div', { class: 'alert info', text: 'No local models found yet. Install LM Studio, Ollama or llama.cpp, or add an API provider later in Settings › Gateway & spend.' }));
    }

    function stepPhone() {
      const addr = h('input', {
        class: 'input mono', value: answers.address, placeholder: 'e.g. 192.168.1.20 or 100.101.102.103',
        autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off', 'aria-label': 'This computer’s address',
        oninput: (e) => { answers.address = e.target.value; },
      });
      const addrRow = h('label', { class: 'field wel-addr', hidden: answers.phone !== 'lan' && answers.phone !== 'tailscale' },
        h('span', { class: 'field-label', text: 'This computer’s address' }), addr,
        h('span', { class: 'field-hint', text: 'Its LAN IP (ip addr) or Tailscale IP (tailscale ip -4). The port stays the same.' }),
        h('span', { class: 'set-err wel-addr-err', role: 'alert' }));
      const modes = answers.phone === 'keep'
        ? [['keep', 'Keep as it is', `Already listening on ${(getAt(config, 'server.extra_listen') || []).join(', ')}.`], ...PHONE_MODES]
        : PHONE_MODES;
      return h('div', { class: 'wel-step' },
        h('h3', { class: 'wel-title', text: 'Phone access' }),
        h('p', { class: 'dlg-msg', text: 'Steer sessions from your phone. Each phone gets its own token you can revoke; pair it afterwards in Settings › Security.' }),
        h('div', { class: 'wel-choices', role: 'radiogroup', 'aria-label': 'Phone access' }, modes.map(([v, label, sub]) =>
          h('label', { class: 'wel-choice' },
            h('input', { type: 'radio', name: 'wel-phone', value: v, checked: answers.phone === v, onchange: () => { answers.phone = v; addrRow.hidden = v !== 'lan' && v !== 'tailscale'; addrRow.querySelector('.wel-addr-err').textContent = ''; } }),
            h('span', {}, h('b', { text: label }), h('span', { class: 'field-hint', text: sub }))))),
        addrRow,
        h('p', { class: 'field-hint', text: 'A new address takes effect after a restart (Settings shows the button).' }));
    }

    function stepNotify() {
      const input = h('input', {
        class: 'input mono', value: answers.ntfy, placeholder: 'https://ntfy.sh/your-secret-topic',
        autocapitalize: 'off', spellcheck: 'false', autocomplete: 'off', inputmode: 'url', 'aria-label': 'ntfy topic',
        oninput: (e) => { answers.ntfy = e.target.value; },
      });
      const make = h('button', { class: 'btn', type: 'button', onclick: () => {
        answers.ntfy = ntfyTopic(crypto.getRandomValues(new Uint8Array(8)));
        input.value = answers.ntfy;
      } }, 'Make a private topic');
      return h('div', { class: 'wel-step' },
        h('h3', { class: 'wel-title', text: 'Notifications' }),
        h('p', { class: 'dlg-msg', text: 'Get a push when a session waits for you or finishes. Subscribe to the same topic in the ntfy app. Leave empty to keep desktop notifications only.' }),
        h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'ntfy topic (optional)' }), input),
        h('div', { class: 'form-actions wel-left' }, make),
        h('p', { class: 'field-hint', text: 'Anyone who knows the topic can read it, so keep it hard to guess.' }));
    }

    function stepPower() {
      return h('div', { class: 'wel-step' },
        h('h3', { class: 'wel-title', text: 'Electricity' }),
        h('p', { class: 'dlg-msg', text: 'Cerberus prices what this PC adds to your bill. Your home’s usual monthly use places it on the right tariff step.' }),
        h('label', { class: 'field' }, h('span', { class: 'field-label', text: 'Household kWh per month (optional)' }),
          h('div', { class: 'set-inline' },
            h('input', { class: 'input set-input', inputmode: 'decimal', value: String(answers.kwh ?? ''), placeholder: 'e.g. 250', 'aria-label': 'Household kWh per month', oninput: (e) => { answers.kwh = e.target.value; } }),
            h('span', { class: 'set-unit', text: 'kWh' }))),
        h('p', { class: 'field-hint', text: 'Half of a bimonthly bill. The tariff itself is in Settings › Monitor & energy.' }));
    }

    async function finish() {
      if (busy) return;
      if (welcomePhoneError(answers, getAt(config, 'server.listen'))) {
        step = PHONE_STEP;
        paint();
        phoneProblem();
        return;
      }
      const set = welcomeChanges(answers, config);
      busy = true;
      next.disabled = true;
      try {
        if (Object.keys(set).length) {
          const res = await patchConfig(set);
          if (!res.ok) {
            err.textContent = res.problems.length ? res.problems.map((p) => p.message).join(' ') : res.message || 'Not saved';
            return;
          }
          if (res.restartPending) toast('Saved. Restart from Settings to apply the new address.', 'warn');
        }
        await markDone(false);
      } catch (e) {
        if (!(e instanceof AuthError)) err.textContent = errText(e);
      } finally {
        busy = false;
        next.disabled = false;
      }
    }

    async function markDone(skipped) {
      try {
        await post('/api/config/init-done', {});
        if (!skipped) toast('Setup saved', 'ok');
        close(true);
      } catch (e) {
        if (!(e instanceof AuthError)) err.textContent = errText(e);
      }
    }

    paint();
    // The dialog is not in the page yet, so paint() cannot focus: let
    // showModal() start on Next (not the ✕, and no phone keyboard popping up).
    next.autofocus = true;
    return { title: 'Welcome to Cerberus', sub: 'Four quick questions; everything can change later in Settings', body, foot: [skip, back, next] };
  });
}
