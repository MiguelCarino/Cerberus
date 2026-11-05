// About (Settings → About): the version and build of this daemon, what
// Cerberus is, and where to read more. The navbar's About window carries the
// fleet's standard tabs; this page keeps the daemon's own facts.

import { h } from '../dom.js';
import { store } from '../store.js';
import { versionLabel, fmtUptime } from '../util.js';

const REPO = 'https://github.com/MiguelCarino/Cerberus';

// aboutPanel fills box and returns its cleanup.
export function aboutPanel(box) {
  const facts = h('dl', { class: 'about-facts' });
  const paint = (st) => {
    const rows = [
      ['Version', st && st.version ? versionLabel(st.version) : '…'],
      ['Build', (st && st.build) || '–'],
      ['Host', (st && st.host) || '–'],
      ['Running for', st && st.uptime != null ? (typeof st.uptime === 'number' ? fmtUptime(st.uptime) : String(st.uptime)) : '–'],
    ];
    facts.replaceChildren(...rows.flatMap(([k, v]) => [h('dt', { text: k }), h('dd', { class: 'mono', text: v })]));
  };
  paint(store.status());
  store.refreshStatus().then(paint).catch(() => {});

  const link = (href, text) => h('a', { href, target: '_blank', rel: 'noopener', text });
  box.append(h('div', { class: 'about' },
    h('div', { class: 'about-card' },
      h('img', { class: 'about-mark', src: 'assets/icons/cerberus.svg', alt: '', width: '48', height: '48' }),
      h('div', {}, h('p', { class: 'about-name', text: 'Carino Cerberus' }),
        h('p', { class: 'about-line', text: 'Your own LLMs and coding agents, steered from your phone.' }))),
    facts,
    h('p', { class: 'field-hint', text: 'Pre-release: versions are 0.0.<number of commits> until releases start. The build is the commit it was made from; "-dirty" means it had uncommitted changes.' }),
    h('h3', { class: 'group-title', text: 'What is here' }),
    h('ul', { class: 'about-list' },
      h('li', { text: 'Chat: the LLM chat from the Atrium desktop, with any model (or the Claude CLI when installed) and tools that run on this host and ask before they change anything. A chat keeps running when you close the tab.' }),
      h('li', { text: 'Sessions: coding agents in a repo worktree, a folder of yours or a scratch folder, steered from any browser.' }),
      h('li', { text: 'Drop, paste or pick files to give them to an agent; other Carino pages can hand files over through the Carino Bridge.' }),
      h('li', { text: 'The bell keeps every notification. Over HTTPS (the Tailscale listener) Cerberus installs as an app and opens its shell offline.' }),
      h('li', {}, 'Press ', h('kbd', { text: '?' }), ' on a keyboard for shortcuts.')),
    h('p', { class: 'field-hint', text: 'Runs on your own host. This page talks only to that host; the links below are the only way out.' }),
    h('p', { class: 'about-links' },
      link(REPO, 'GitHub project'), ' · ',
      link(`${REPO}#readme`, 'README'), ' · ',
      link(`${REPO}/blob/main/docs/SECURITY.md`, 'Threat model'), ' · ',
      link('https://carino.systems/legal.html', 'Legal and privacy'), ' · ',
      link('https://cerberus.carino.systems/', 'cerberus.carino.systems'))));
  return () => {};
}
