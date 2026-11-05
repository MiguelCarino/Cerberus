// Saved permissions ("Always allow"): pure helpers for the approval menu,
// the chat card and Settings → Permissions. No DOM, no network, so node
// --test can import them (web/test/permits.test.mjs). The server is the
// authority: it works out the options again and validates every save.

const str = (v) => (v == null ? '' : String(v));

// normAlways turns a server list of options ({text, tool, content} from a
// chat request, or {Text, Tool, Content} from a session's WaitingAlways)
// into [{text, tool, content}], dropping malformed entries.
export function normAlways(list) {
  if (!Array.isArray(list)) return [];
  return list.map((o) => (o && typeof o === 'object' ? {
    text: str(o.text ?? o.Text), tool: str(o.tool ?? o.Tool), content: str(o.content ?? o.Content),
  } : null)).filter((o) => o && o.tool);
}

// ruleWords says a rule for people: a Bash command as itself ("wc -l
// *.go"), a prefix as "any “go test …” command", a folder as "Read in
// ~/notes" (the server's text), a host as "WebFetch on docs.python.org",
// a tool-wide rule as "any cerberus_spawn".
export function ruleWords(o) {
  const r = o || {};
  const tool = str(r.tool);
  const c = str(r.content);
  if (tool === 'Bash' && c) {
    if (c.endsWith(':*')) return `any “${c.slice(0, -2).trim()} …” command`;
    return c;
  }
  if (c.startsWith('domain:')) return `${tool} on ${c.slice(7)} (and below)`;
  if (!c) return `any ${tool}`;
  return str(r.text) || `${tool} ${c}`;
}

// alwaysLabel is the menu entry of a saved option.
export const alwaysLabel = (o) => `Every time: ${ruleWords(o)}`;

// approvalMenuItems are the entries of a session approval's "Always ▾":
// first the old run-only answer ("a"), then one "save" per option.
export function approvalMenuItems(tool, always) {
  const items = [{ kind: 'run', answer: 'a', label: `This run: any ${tool || 'call of this tool'}`, title: 'Stop asking for this tool until the agent finishes (not saved)' }];
  normAlways(always).forEach((o, i) => {
    items.push({ kind: 'save', answer: 'save', option: i, label: alwaysLabel(o), title: `Save ${o.text || ruleWords(o)}: it runs without asking from now on, in every session and chat (remove it in Settings → Permissions)` });
  });
  return items;
}

// chatMenuItems are the entries of a chat card's "Always allow ▾".
export function chatMenuItems(always) {
  return normAlways(always).map((o, i) => ({ kind: 'save', option: i, label: alwaysLabel(o), title: `Save ${o.text || ruleWords(o)} for good (remove it in Settings → Permissions)` }));
}

// SOURCE_LABEL says where a saved rule came from.
export const SOURCE_LABEL = { chat: 'LLM chat', session: 'Agent session', settings: 'Added here' };
export const sourceLabel = (s) => SOURCE_LABEL[s] || str(s) || '—';

// The tools the Add form offers, with a placeholder each.
export const TOOL_CHOICES = [
  ['Bash', 'go test:*  or  npm run build'],
  ['Read', 'dir:~/notes'],
  ['Edit', 'dir:~/projects/site'],
  ['Write', 'dir:~/projects/site'],
  ['Glob', 'dir:~/projects'],
  ['Grep', 'dir:~/projects'],
  ['LS', 'dir:~/projects'],
  ['WebFetch', 'domain:docs.python.org'],
];

export function placeholderFor(tool) {
  const hit = TOOL_CHOICES.find(([t]) => t === tool);
  return hit ? hit[1] : '';
}

// normPermits turns GET /api/permits into {rules, policy, link}.
export function normPermits(raw) {
  const b = raw && typeof raw === 'object' ? raw : {};
  const rules = (Array.isArray(b.rules) ? b.rules : []).map((r) => ({
    id: str(r.id), tool: str(r.tool), content: str(r.content), text: str(r.text), source: str(r.source),
    created: str(r.created), lastUsed: str(r.last_used), uses: Number.isFinite(+r.uses) ? +r.uses : 0,
  })).filter((r) => r.id);
  const p = b.policy && typeof b.policy === 'object' ? b.policy : {};
  const list = (v) => (Array.isArray(v) ? v.map(str).filter(Boolean) : []);
  return { rules, policy: { allow: list(p.allow), askAlways: list(p.ask_always), deny: list(p.deny) }, link: str(b.policy_link) };
}

// shortDate is "3 Nov 2026" for an ISO time ("" for none or a zero time).
export function shortDate(iso) {
  const s = str(iso);
  if (!s || s.startsWith('0001-')) return '';
  const d = new Date(s);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleDateString('en-GB', { day: 'numeric', month: 'short', year: 'numeric' });
}
