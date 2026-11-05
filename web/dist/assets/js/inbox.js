// Pure helpers for the notification centre (day phase 6): normalising
// GET /api/inbox, applying inbox events from /api/events, grouping by day,
// safe in-app routes and the desktop-notification gate. No DOM, so
// node --test imports them directly.

// Levels an item can have, mapped to the UI tones.
export const LEVEL_TONE = { info: 'info', warn: 'warn', crit: 'err' };

// levelTone is the tone class for an item's level (info when unknown).
export const levelTone = (level) => LEVEL_TONE[level] || 'info';

// normItem fills an inbox item from the API (Go field names), or returns
// null for something that is not one.
export function normItem(raw) {
  if (!raw || typeof raw !== 'object' || !raw.ID) return null;
  return {
    ID: String(raw.ID),
    Time: raw.Time || '',
    Kind: raw.Kind || '',
    Level: LEVEL_TONE[raw.Level] ? raw.Level : 'info',
    Title: raw.Title || '',
    Body: raw.Body || '',
    Link: raw.Link || '',
    Read: !!raw.Read,
  };
}

const timeOf = (it) => Date.parse(it.Time) || 0;

// sortItems orders items newest first (ties by id, so the order is stable).
export function sortItems(items) {
  return [...items].sort((a, b) => (timeOf(b) - timeOf(a)) || (a.ID < b.ID ? 1 : a.ID > b.ID ? -1 : 0));
}

// normInbox turns a GET /api/inbox reply (a list, or {items, unread}) into
// {items (newest first), unread}. A reply without a count counts the
// unread items it holds.
export function normInbox(reply) {
  const list = Array.isArray(reply) ? reply : (reply && (reply.items || reply.Items)) || [];
  const items = sortItems(list.map(normItem).filter(Boolean));
  let unread = reply && !Array.isArray(reply) ? (reply.unread ?? reply.Unread) : undefined;
  if (!Number.isFinite(unread)) unread = items.filter((i) => !i.Read).length;
  return { items, unread };
}

// applyEvent applies one inbox event ({Type: added|read|deleted, Item,
// IDs, All, Unread}) to state {items, unread} and returns the new state
// (state itself is not changed). keep caps the list.
export function applyEvent(state, ev, keep = 1000) {
  if (!ev || typeof ev !== 'object') return state;
  let items = state.items;
  switch (ev.Type) {
    case 'added': {
      const it = normItem(ev.Item);
      if (!it) return state;
      items = sortItems([it, ...items.filter((x) => x.ID !== it.ID)]).slice(0, keep);
      break;
    }
    case 'read': {
      const ids = new Set(ev.IDs || []);
      items = items.map((x) => (ev.All || ids.has(x.ID) ? (x.Read ? x : { ...x, Read: true }) : x));
      break;
    }
    case 'deleted': {
      const ids = new Set(ev.IDs || []);
      items = items.filter((x) => !ids.has(x.ID));
      break;
    }
    default:
      return state;
  }
  const unread = Number.isFinite(ev.Unread) ? ev.Unread : items.filter((x) => !x.Read).length;
  return { items, unread };
}

// markRead marks ids (or every item when all) read locally, before the
// server's event confirms it.
export function markRead(state, ids, all = false) {
  return applyEvent(state, { Type: 'read', IDs: ids, All: all, Unread: undefined });
}

const pad = (n) => String(n).padStart(2, '0');

// dayKey is the local calendar day of a time, "2026-11-01".
export function dayKey(t) {
  const d = new Date(t);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

// dayLabel names a day key relative to now: "Today", "Yesterday", or
// "Mon 27 Oct" (with the year when it is not this year's).
export function dayLabel(key, now = Date.now()) {
  if (!key) return 'Earlier';
  if (key === dayKey(now)) return 'Today';
  const y = new Date(now);
  y.setDate(y.getDate() - 1);
  if (key === dayKey(y)) return 'Yesterday';
  const [yy, mm, dd] = key.split('-').map(Number);
  const d = new Date(yy, mm - 1, dd);
  const year = yy !== new Date(now).getFullYear() ? ` ${yy}` : '';
  return `${DAYS[d.getDay()]} ${dd} ${MONTHS[mm - 1]}${year}`;
}

// groupByDay splits items into [{key, label, items}] in newest-first order.
export function groupByDay(items, now = Date.now()) {
  const groups = [];
  let cur = null;
  for (const it of sortItems(items)) {
    const key = dayKey(it.Time);
    if (!cur || cur.key !== key) {
      cur = { key, label: dayLabel(key, now), items: [] };
      groups.push(cur);
    }
    cur.items.push(it);
  }
  return groups;
}

// clockTime is the local "14:05" of an item.
export function clockTime(t) {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? '' : `${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// safeRoute accepts only in-app hash routes ("#/s/abc"); anything else
// (an absolute URL, "javascript:", a path) is refused with null, so an item
// can never send the browser off the host.
export function safeRoute(link) {
  if (typeof link !== 'string') return null;
  const l = link.trim();
  if (!/^#\/[^\s]*$/.test(l)) return null;
  return l;
}

// flowApprovalRun is the run id of a flow approval item (Kind
// "flow-approve", Link "#/flows/runs/<run>"), or "" for any other item:
// such an item gets Approve / Reject buttons in the list.
export function flowApprovalRun(it) {
  if (!it || it.Kind !== 'flow-approve') return '';
  const m = /^#\/flows\/runs\/([A-Za-z0-9._-]+)$/.exec(safeRoute(it.Link) || '');
  return m ? m[1] : '';
}

// badgeText is the unread badge: "" for none, "99+" past 99.
export function badgeText(n) {
  if (!Number.isFinite(n) || n <= 0) return '';
  return n > 99 ? '99+' : String(Math.floor(n));
}

// shouldNotify gates a desktop notification: the user opted in, the
// browser granted it, the tab is hidden, and the item is new and unread.
export function shouldNotify({ optedIn, permission, hidden, item }) {
  return !!(optedIn && permission === 'granted' && hidden && item && !item.Read);
}

// keepStreamWhenHidden: the event stream normally closes while the tab is
// hidden (energy rule); it stays open only for opted-in, granted desktop
// notifications, which can only fire while hidden.
export function keepStreamWhenHidden(optedIn, permission) {
  return !!optedIn && permission === 'granted';
}

// normAsks keeps the well-formed open chat permission requests from GET
// /api/chats/asks ([{chat, title, model, req}]), each with key
// "chat/req" (the events stream's "asks" keys use the same form).
export function normAsks(list) {
  if (!Array.isArray(list)) return [];
  const out = [];
  for (const a of list) {
    if (!a || typeof a.chat !== 'string' || !a.chat || !a.req || typeof a.req.id !== 'string' || !a.req.id) continue;
    out.push({ key: a.chat + '/' + a.req.id, chat: a.chat, title: String(a.title || 'LLM chat'), model: String(a.model || ''), req: a.req });
  }
  return out;
}
