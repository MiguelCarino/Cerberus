// Pure helpers for file drop (day phase 2): size formatting, where a drop
// goes, which bridge origins deliver without asking, what kind of preview a
// file gets, workspace breadcrumbs and upload outcomes. No DOM, no network,
// so node --test imports this directly (see web/test/files.test.mjs).

// TEXT_PREVIEW_BYTES is how much of a text file the preview reads.
export const TEXT_PREVIEW_BYTES = 256 * 1024;

// DEFAULT_UPLOAD_DIR mirrors config Files.UploadDir's default.
export const DEFAULT_UPLOAD_DIR = 'inbox';

// fmtBytes renders a byte count in binary units: "512 B", "3.4 KiB", "34 KiB".
export function fmtBytes(n) {
  if (n == null || !Number.isFinite(+n) || +n < 0) return '–';
  n = +n;
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v < 10 ? v.toFixed(1).replace(/\.0$/, '') : Math.round(v)} ${units[i]}`;
}

// ---------- paths inside a workspace ----------

// normRel cleans a workspace-relative path: slash-separated, no leading or
// trailing slash, "." for the root. Anything that climbs ("..") is refused
// with "" so a caller never builds a request for it.
export function normRel(rel) {
  const parts = String(rel ?? '').replace(/\\/g, '/').split('/').filter((p) => p && p !== '.');
  if (parts.some((p) => p === '..')) return '';
  return parts.length ? parts.join('/') : '.';
}

// joinRel appends a name to a folder ("." is the root).
export function joinRel(dir, name) {
  const d = normRel(dir);
  const n = normRel(name);
  if (!d || !n) return '';
  if (n === '.') return d;
  return d === '.' ? n : `${d}/${n}`;
}

// parentRel is the folder above rel; the root's parent is the root.
export function parentRel(rel) {
  const r = normRel(rel);
  if (!r || r === '.') return '.';
  const i = r.lastIndexOf('/');
  return i < 0 ? '.' : r.slice(0, i);
}

// relCrumbs splits a workspace path into clickable crumbs, starting with
// the workspace root (label rootName).
export function relCrumbs(rel, rootName = 'workspace') {
  const crumbs = [{ name: rootName, rel: '.' }];
  const r = normRel(rel);
  if (!r || r === '.') return crumbs;
  let acc = '';
  for (const part of r.split('/')) {
    acc = acc ? `${acc}/${part}` : part;
    crumbs.push({ name: part, rel: acc });
  }
  return crumbs;
}

// ---------- drop destination ----------

// destinationLabel names where dropped files will go, for the overlay and
// the upload toast. dest: { kind: 'session' | 'task', name, dir, uploadDir }.
//   session, no dir → "<name> › inbox"   (the default upload folder)
//   session, dir    → "<name> › docs/img" ("." is the workspace root)
//   task            → "a new task"
export function destinationLabel(dest) {
  if (!dest) return '';
  if (dest.kind === 'task') return 'a new task';
  const name = (dest.name || '').trim() || 'this session';
  let dir = dest.dir == null || dest.dir === '' ? (dest.uploadDir || DEFAULT_UPLOAD_DIR) : normRel(dest.dir);
  if (dir === '.') dir = 'workspace root';
  return `${name} › ${dir}`;
}

// dropPrompt is the overlay's headline for a destination.
export function dropPrompt(dest) {
  if (!dest) return '';
  if (dest.kind === 'chat') return 'Drop images to attach them to the chat';
  return dest.kind === 'task' ? 'Drop to attach to a new task' : `Drop to upload to ${destinationLabel(dest)}`;
}

// ---------- Carino Bridge ----------

// originAllowed says whether a bridge sender may deliver without asking:
// this page's own origin, or an https origin under *.carino.systems on the
// default port. Everything else is asked about (never refused silently,
// never labelled "trusted": the sheet names the origin it came from).
export function originAllowed(origin, selfOrigin) {
  if (!origin || origin === 'null') return false;
  if (selfOrigin && origin === selfOrigin) return true;
  let u;
  try { u = new URL(origin); } catch { return false; }
  if (u.origin !== origin) return false; // a path, credentials or odd form
  if (u.protocol !== 'https:' || u.port !== '') return false;
  const host = u.hostname.toLowerCase();
  return host.endsWith('.carino.systems') && host.length > '.carino.systems'.length;
}

// bridgeTargets is the sessions a bridge handoff may go to, newest first:
// not closed (isClosed(state)) and with a workspace on disk (Worktree), so
// the files can land at once.
export function bridgeTargets(sessions, isClosed) {
  return (sessions || []).filter((s) => s && s.Worktree && !isClosed(s.State))
    .sort((a, b) => (Date.parse(b.Updated) || 0) - (Date.parse(a.Updated) || 0));
}

// originHost is what a handoff is labelled with: the sender's host (with
// its port when it has one), or the raw origin when it cannot be parsed.
export function originHost(origin) {
  try { return new URL(origin).host || String(origin || ''); } catch { return String(origin || 'an unknown page'); }
}

// ---------- previews ----------

const EXT_KIND = {
  image: ['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico', 'svg'],
  pdf: ['pdf'],
  audio: ['mp3', 'wav', 'ogg', 'oga', 'opus', 'm4a', 'aac', 'flac', 'weba'],
  video: ['mp4', 'm4v', 'webm', 'ogv', 'mov'],
  text: ['txt', 'md', 'markdown', 'rst', 'log', 'csv', 'tsv', 'json', 'jsonl', 'yaml', 'yml', 'toml', 'ini', 'cfg', 'conf',
    'xml', 'html', 'htm', 'css', 'scss', 'js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'go', 'mod', 'sum', 'py', 'rb', 'rs', 'c',
    'h', 'cc', 'cpp', 'hpp', 'java', 'kt', 'swift', 'sh', 'bash', 'zsh', 'fish', 'ps1', 'bat', 'sql', 'lua', 'pl', 'php',
    'r', 'tex', 'diff', 'patch', 'env', 'gitignore', 'dockerfile', 'makefile', 'srt', 'vtt', 'lrc'],
};
const KIND_OF_EXT = new Map(Object.entries(EXT_KIND).flatMap(([k, exts]) => exts.map((e) => [e, k])));
const TEXT_NAMES = new Set(['makefile', 'dockerfile', 'license', 'readme', 'changelog', 'authors', 'notice', 'go.mod', 'go.sum']);

// extOf is the lower-case extension without the dot ("" when none); a
// dotfile such as ".env" counts as extension "env".
export function extOf(name) {
  const base = String(name || '').split('/').pop().toLowerCase();
  const i = base.lastIndexOf('.');
  return i < 0 || i === base.length - 1 ? '' : base.slice(i + 1);
}

// previewKind picks the preview for a file: image | text | pdf | audio |
// video | none. entry may be an Entry from the API (Name, Mime, Text, Image,
// Dir, Outside) or { name, type } (a File). The server's Image/Text flags
// win, then the MIME type, then the extension.
export function previewKind(entry) {
  if (!entry || entry.Dir || entry.Outside) return 'none';
  const name = entry.Name ?? entry.name ?? '';
  const mime = String(entry.Mime ?? entry.type ?? '').toLowerCase().split(';')[0].trim();
  if (entry.Image) return 'image';
  if (mime === 'application/pdf') return 'pdf';
  if (mime.startsWith('image/')) return 'image';
  if (mime.startsWith('audio/')) return 'audio';
  if (mime.startsWith('video/')) return 'video';
  if (entry.Text) return 'text';
  if (mime.startsWith('text/') || /(^application\/(json|xml|javascript|x-sh|toml|yaml|x-yaml))|\+json$|\+xml$/.test(mime)) return 'text';
  const k = KIND_OF_EXT.get(extOf(name));
  if (k) return k;
  if (TEXT_NAMES.has(String(name).split('/').pop().toLowerCase())) return 'text';
  return 'none';
}

// ---------- uploads ----------

// isFileDrag reports whether a drag carries files (DataTransfer.types).
export function isFileDrag(types) {
  if (!types) return false;
  for (const t of types) if (t === 'Files') return true;
  return false;
}

// uploadError turns a failed upload's HTTP status and server message into
// the short reason shown next to the file.
export function uploadError(status, message) {
  const msg = String(message || '').trim();
  if (status === 0) return msg || 'network error';
  // The server's 413 texts already start with "too large".
  if (status === 413) return !msg ? 'too large' : /^too large\b/i.test(msg) ? msg : `too large: ${msg}`;
  if (status === 401) return 'sign in required';
  if (status === 403) return msg || 'refused';
  if (status === 400 || status === 422) return msg ? `refused: ${msg}` : 'refused name';
  if (status === 409) return msg || 'already exists';
  return msg || `HTTP ${status}`;
}

// tooLargeFor is the reason a file of size bytes is refused before sending
// under a per-file limit of maxFileMiB ("" when it fits or there is no
// known limit).
export function tooLargeFor(size, maxFileMiB) {
  const max = +maxFileMiB || 0;
  return max > 0 && +size > max * 1024 * 1024 ? `too large (limit ${max} MiB)` : '';
}

// uploadSummary condenses per-file results ({ name, ok, error, cancelled })
// into one toast: { kind, text }. dest is the destination label.
export function uploadSummary(results, dest = '') {
  const list = results || [];
  const ok = list.filter((r) => r.ok).length;
  const cancelled = list.filter((r) => r.cancelled).length;
  const failed = list.filter((r) => !r.ok && !r.cancelled);
  const files = (n) => `${n} file${n === 1 ? '' : 's'}`;
  const where = dest ? ` to ${dest}` : '';
  if (!list.length) return { kind: 'info', text: 'Nothing to upload' };
  if (!failed.length && !cancelled) return { kind: 'ok', text: `Uploaded ${files(ok)}${where}` };
  if (!failed.length) {
    return ok ? { kind: 'warn', text: `Uploaded ${files(ok)}${where}; ${cancelled} cancelled` } : { kind: 'info', text: 'Upload cancelled' };
  }
  const first = `${failed[0].name}: ${failed[0].error || 'failed'}`;
  const more = failed.length > 1 ? ` (+${failed.length - 1} more)` : '';
  return { kind: 'err', text: `${ok ? `Uploaded ${ok} of ${list.length}` : 'Upload failed'}; ${first}${more}` };
}

// fileProgress splits one request's upload progress over the files it
// carries (multipart sends them in order): returns each file's fraction
// 0..1. sizes are the file sizes; loaded/total are the XHR byte counts,
// which include the multipart framing, spread proportionally.
export function fileProgress(sizes, loaded, total) {
  const sum = sizes.reduce((a, b) => a + b, 0);
  if (!total || total <= 0) return sizes.map(() => 0);
  let sent = Math.min(1, Math.max(0, loaded / total)) * sum;
  return sizes.map((s) => {
    if (s <= 0) return loaded >= total ? 1 : 0;
    const take = Math.min(s, Math.max(0, sent));
    sent -= s;
    return take / s;
  });
}

// fileKey identifies a picked file so the same one is not attached twice.
export function fileKey(f) {
  return `${f.name}\u0000${f.size}\u0000${f.lastModified || 0}`;
}

// addFiles merges new files into a list, skipping ones already in it.
export function addFiles(list, more) {
  const seen = new Set(list.map(fileKey));
  const out = list.slice();
  for (const f of more || []) {
    const k = fileKey(f);
    if (!seen.has(k)) { seen.add(k); out.push(f); }
  }
  return out;
}

// pastedName gives a clipboard image a useful name: browsers call every
// pasted screenshot "image.png", so it gets a timestamp instead.
export function pastedName(name, type, now = new Date()) {
  const generic = !name || /^image\.(png|jpe?g|gif|webp)$/i.test(name);
  if (!generic) return name;
  const ext = (String(type || '').split('/')[1] || (name && extOf(name)) || 'png').replace('jpeg', 'jpg').replace(/[^a-z0-9]/gi, '');
  const p = (n) => String(n).padStart(2, '0');
  return `pasted-${now.getFullYear()}${p(now.getMonth() + 1)}${p(now.getDate())}-${p(now.getHours())}${p(now.getMinutes())}${p(now.getSeconds())}.${ext || 'png'}`;
}

// ---------- telling the agent ----------

// sendToAgentText is the follow-up that hands workspace files to the agent.
export function sendToAgentText(rels) {
  const list = (Array.isArray(rels) ? rels : [rels]).map(normRel).filter((r) => r && r !== '.');
  if (!list.length) return '';
  if (list.length === 1) return `Take a look at ./${list[0]}`;
  return `Take a look at these files: ${list.map((r) => `./${r}`).join(', ')}`;
}

// insertedPaths is what the input bar gains after a paste uploaded files:
// their workspace paths, space-separated, appended to the current text.
export function insertedPaths(current, rels) {
  const add = (rels || []).filter(Boolean).join(' ');
  if (!add) return current || '';
  const cur = current || '';
  return cur && !/\s$/.test(cur) ? `${cur} ${add}` : cur + add;
}

// ---------- API paths ----------

const sessionBase = (id) => '/api/sessions/' + encodeURIComponent(id) + '/files';

// filesPath is the folder listing (GET) and upload (POST) URL for dir.
export function filesPath(id, dir) {
  const d = normRel(dir);
  return d && d !== '.' ? sessionBase(id) + '?dir=' + encodeURIComponent(d) : sessionBase(id);
}

// rawPath is a file's download/preview URL; download asks for an attachment.
export function rawPath(id, rel, download = false) {
  return sessionBase(id) + '/raw?path=' + encodeURIComponent(normRel(rel)) + (download ? '&download=1' : '');
}

// deletePath is the DELETE URL for one file or folder; recursive asks for
// a folder with its contents (the server allows that inside the inbox only).
export function deletePath(id, rel, recursive = false) {
  return sessionBase(id) + '?path=' + encodeURIComponent(normRel(rel)) + (recursive ? '&recursive=1' : '');
}

// deletesWithContents says whether the folder rel may be deleted with
// everything in it: it lies inside the inbox (uploadDir), not the inbox
// itself.
export function deletesWithContents(rel, uploadDir = DEFAULT_UPLOAD_DIR) {
  const r = normRel(rel);
  const inbox = normRel(uploadDir || DEFAULT_UPLOAD_DIR);
  return !!r && !!inbox && inbox !== '.' && r.startsWith(inbox + '/');
}

// folderNameError checks a new folder's name ("" when fine).
export function folderNameError(name) {
  const n = String(name || '').trim();
  if (!n) return 'Give the folder a name';
  if (/[\/\\\u0000]/.test(n)) return 'A folder name cannot contain / or \\';
  if (n === '.' || n === '..' || n.startsWith('.')) return 'A folder name cannot start with a dot';
  if (new TextEncoder().encode(n).length > 180) return 'That name is too long';
  return '';
}
