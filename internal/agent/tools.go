package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Limits, as in Atrium's llm_chat_engine.py.
const (
	outputMax     = 12_000     // characters of one tool result the model gets
	readMax       = 256 * 1024 // bytes read_file reads
	listMax       = 400        // entries list_dir lists
	searchHits    = 200        // lines search returns
	searchFiles   = 20_000     // files search looks at
	searchFileMax = 2_000_000  // bytes: a bigger file is not searched
	timeoutS      = 120        // run_command's default
	timeoutMax    = 600
)

var skipDirs = map[string]bool{".git": true, ".hg": true, ".svn": true, "node_modules": true,
	"__pycache__": true, ".venv": true, "venv": true, ".mypy_cache": true, ".pytest_cache": true,
	".tox": true, "dist": true, "build": true, ".cache": true, ".next": true, "target": true}

// toolSpec is an OpenAI "function" tool definition.
type toolSpec struct {
	Type     string  `json:"type"`
	Function toolDef `json:"function"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func fn(name, desc string, props map[string]any, required ...string) toolSpec {
	if required == nil {
		required = []string{}
	}
	return toolSpec{Type: "function", Function: toolDef{Name: name, Description: desc, Parameters: map[string]any{
		"type": "object", "properties": props, "required": required}}}
}

func prop(typ, desc string) map[string]any {
	if desc == "" {
		return map[string]any{"type": typ}
	}
	return map[string]any{"type": typ, "description": desc}
}

var toolSpecs = []toolSpec{
	fn("read_file", "Read a text file. Returns its lines numbered. Use offset/limit for long files.", map[string]any{
		"path":   prop("string", "File path, absolute or relative to the working directory."),
		"offset": prop("integer", "First line to return (1-based, optional)."),
		"limit":  prop("integer", "How many lines (optional)."),
	}, "path"),
	fn("list_dir", "List a folder's entries (folders end with /).", map[string]any{
		"path": prop("string", "Folder path; the working directory when left out."),
	}),
	fn("search", "Search files under a folder: lines matching a regular expression (pattern), "+
		"and/or file names matching a glob (glob, like *.py).", map[string]any{
		"pattern": prop("string", "Regular expression to find in file contents (optional)."),
		"glob":    prop("string", "File name glob, e.g. *.md (optional)."),
		"path":    prop("string", "Folder to search; the working directory when left out."),
	}),
	fn("run_command", "Run a bash command in the working directory and return its output "+
		"(stdout and stderr). Non-interactive: no input can be typed.", map[string]any{
		"command":     prop("string", "The command line."),
		"description": prop("string", "What it does, in a few words (shown to the user)."),
		"timeout": prop("integer", fmt.Sprintf("Seconds before it is stopped (default %d, at most %d).",
			timeoutS, timeoutMax)),
	}, "command"),
	fn("write_file", "Create or replace a whole file with the given content.", map[string]any{
		"path": prop("string", ""), "content": prop("string", ""),
	}, "path", "content"),
	fn("edit_file", "Replace an exact piece of text in a file. old_string must occur exactly once "+
		"(include surrounding lines to make it unique) unless replace_all is true.", map[string]any{
		"path": prop("string", ""), "old_string": prop("string", ""), "new_string": prop("string", ""),
		"replace_all": prop("boolean", ""),
	}, "path", "old_string", "new_string"),
}

// changeTools need approval in ask mode; editTools write files.
var (
	changeTools = map[string]bool{"run_command": true, "write_file": true, "edit_file": true}
	editTools   = map[string]bool{"write_file": true, "edit_file": true}
)

func toolNames() string {
	names := make([]string, len(toolSpecs))
	for i, t := range toolSpecs {
		names[i] = t.Function.Name
	}
	return strings.Join(names, ", ")
}

// toolbox runs tools in one working directory.
type toolbox struct {
	cwd, realCwd string
	origin       string // the repo cwd is a worktree of ("" = none): deny globs see its paths too
	deny         denyList
	private      privateArea
	timeout      time.Duration
	resultMax    int                          // characters of one result (0 = outputMax)
	jail         bool                         // RunOptions.Jail: nothing outside cwd
	wrap         func(argv []string) []string // RunOptions.Wrap: run_command inside a sandbox

	cmdMu       sync.Mutex
	cmdSessions []int // the sessions run_command started (reap)
}

func newToolbox(cwd string, deny []string, timeout time.Duration) *toolbox {
	return &toolbox{cwd: cwd, realCwd: realPath(cwd), deny: newDenyList(deny), timeout: timeout}
}

type args map[string]any

func (a args) str(k string) string {
	switch v := a[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// int accepts numbers and numeric strings (models send both); ok is false when absent or unusable.
func (a args) int(k string) (int, bool) {
	switch v := a[k].(type) {
	case float64:
		return int(v), true
	case int: // arguments built in Go rather than decoded from JSON
		return v, true
	case json.Number:
		n, err := strconv.Atoi(v.String())
		return n, err == nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

func (a args) bool(k string) bool {
	switch v := a[k].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	case float64:
		return v != 0
	}
	return false
}

// run runs one tool: (output, isError), the output clipped for the model.
func (t *toolbox) run(ctx context.Context, name string, a args) (out string, bad bool) {
	defer func() {
		if p := recover(); p != nil {
			out, bad = fmt.Sprintf("%s failed: %v", name, p), true
		}
	}()
	var err error
	switch name {
	case "read_file":
		out, err = t.readFile(a)
	case "list_dir":
		out, err = t.listDir(a)
	case "search":
		out, err = t.search(ctx, a)
	case "run_command":
		out, err = t.runCommand(ctx, a)
	case "write_file":
		out, err = t.writeFile(a)
	case "edit_file":
		out, err = t.editFile(a)
	default:
		return fmt.Sprintf("there is no tool %q: the tools are %s", name, toolNames()), true
	}
	limit := t.resultMax
	if limit <= 0 {
		limit = outputMax
	}
	if err != nil {
		return clip(err.Error(), limit), true
	}
	return clip(out, limit), false
}

// refused says why p may not be touched at all ("" = allowed).
func (t *toolbox) refused(p string) string {
	real := realPath(p)
	if t.jail && (!under(p, t.cwd) && !under(p, t.realCwd) || !under(real, t.realCwd)) {
		// Lexically and after symlinks: a link inside the folder that
		// points out of it is outside.
		return "outside the working folder " + t.cwd + " (this account may use only its own folder)"
	}
	if t.private.hides(p) || t.private.hides(real) {
		return "protected: Cerberus session records (another session's data)"
	}
	for _, q := range []string{p, real, t.inOrigin(p, t.cwd), t.inOrigin(real, t.realCwd)} {
		if q == "" {
			continue
		}
		if pat := t.deny.match(q); pat != "" {
			return "protected by deny_paths (" + pat + ")"
		}
	}
	return ""
}

// inOrigin maps p inside root (the worktree) to the same place in the
// original repo, so a glob naming the repo (say **/Private-Repo/**) also
// protects its worktree, whose path doesn't contain the repo's name.
func (t *toolbox) inOrigin(p, root string) string {
	if t.origin == "" || !under(p, root) {
		return ""
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return ""
	}
	return filepath.Join(t.origin, rel)
}

// writeRefused says why p may not be written ("" = allowed).
func (t *toolbox) writeRefused(p string) string {
	if why := t.refused(p); why != "" {
		return fmt.Sprintf("%s: %s -- not written", p, why)
	}
	if !under(p, t.cwd) || !under(realPath(p), t.realCwd) {
		return fmt.Sprintf("%s is outside the working directory %s: writes are allowed only there", p, t.cwd)
	}
	return ""
}

// precheck refuses a change tool before anyone is asked about it.
func (t *toolbox) precheck(name string, a args) string {
	if !editTools[name] {
		return ""
	}
	return t.writeRefused(resolve(a.str("path"), t.cwd))
}

func (t *toolbox) readFile(a args) (string, error) {
	p := resolve(a.str("path"), t.cwd)
	if why := t.refused(p); why != "" {
		return "", fmt.Errorf("%s: %s", p, why)
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		hint := ""
		if err == nil && st.IsDir() {
			hint = " (it is a folder: use list_dir)"
		}
		return "", fmt.Errorf("there is no file %s%s", p, hint)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", errors.New(errText(err))
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, readMax))
	if err != nil {
		return "", errors.New(errText(err))
	}
	if isBinary(raw) {
		return "", fmt.Errorf("%s is a binary file: not read", p)
	}
	lines := splitLines(strings.ToValidUTF8(string(raw), "�"))
	start := 1
	if n, ok := a.int("offset"); ok && n > 1 {
		start = n
	}
	count := len(lines)
	if n, ok := a.int("limit"); ok && n != 0 {
		count = n
	}
	from := min(start-1, len(lines))
	to := min(from+max(1, count), len(lines))
	var b strings.Builder
	for i, line := range lines[from:to] {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%6d\t%s", from+i+1, line)
	}
	if st.Size() > readMax {
		fmt.Fprintf(&b, "\n[only the first %d KB of %d KB were read]", readMax/1024, st.Size()/1024)
	}
	if b.Len() == 0 {
		return "(empty file)", nil
	}
	return b.String(), nil
}

func (t *toolbox) listDir(a args) (string, error) {
	p := resolve(orStr(a.str("path"), "."), t.cwd)
	if why := t.refused(p); why != "" {
		return "", fmt.Errorf("%s: %s", p, why)
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", fmt.Errorf("there is no folder %s", p)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", errors.New(errText(err))
	}
	type row struct {
		name string
		dir  bool
	}
	rows := make([]row, len(entries))
	for i, e := range entries {
		dir := e.IsDir()
		if e.Type()&fs.ModeSymlink != 0 {
			if st, err := os.Stat(filepath.Join(p, e.Name())); err == nil {
				dir = st.IsDir()
			}
		}
		rows[i] = row{e.Name(), dir}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].dir != rows[j].dir {
			return rows[i].dir
		}
		return strings.ToLower(rows[i].name) < strings.ToLower(rows[j].name)
	})
	var b strings.Builder
	b.WriteString(p + ":")
	for _, r := range rows[:min(len(rows), listMax)] {
		b.WriteString("\n" + r.name)
		if r.dir {
			b.WriteByte('/')
		}
	}
	if len(rows) > listMax {
		fmt.Fprintf(&b, "\n[... %d more]", len(rows)-listMax)
	}
	if len(rows) == 0 {
		b.WriteString("\n(empty)")
	}
	return b.String(), nil
}

func (t *toolbox) search(ctx context.Context, a args) (string, error) {
	pattern, glob := a.str("pattern"), a.str("glob")
	if pattern == "" && glob == "" {
		return "", errors.New("give a pattern (contents) or a glob (file names), or both")
	}
	root := resolve(orStr(a.str("path"), "."), t.cwd)
	if why := t.refused(root); why != "" {
		return "", fmt.Errorf("%s: %s", root, why)
	}
	st, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("there is no %s", root)
	}
	var rx *regexp.Regexp
	if pattern != "" {
		if rx, err = regexp.Compile(pattern); err != nil {
			return "", fmt.Errorf("not a valid regular expression: %v", err)
		}
	}
	files := []string{root}
	if st.IsDir() {
		files = t.walk(ctx, root)
	}
	var hits []string
	for _, p := range files {
		if ctx.Err() != nil || len(hits) >= searchHits {
			break
		}
		if glob != "" && !globMatch(glob, p, root) {
			continue
		}
		shown := p
		if under(p, t.cwd) {
			shown, _ = filepath.Rel(t.cwd, p)
		}
		if rx == nil {
			hits = append(hits, shown)
			continue
		}
		hits = grepFile(p, shown, rx, hits)
	}
	if len(hits) == 0 {
		return "no matches", nil
	}
	out := strings.Join(hits, "\n")
	if len(hits) >= searchHits {
		out += fmt.Sprintf("\n[stopped at %d matches]", searchHits)
	}
	return out, nil
}

// walk lists up to searchFiles files under root, skipping noise folders,
// hidden folders and anything deny_paths protects.
func (t *toolbox) walk(ctx context.Context, root string) []string {
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil || len(files) >= searchFiles {
			return filepath.SkipAll
		}
		if t.deny.match(p) != "" || t.inOriginDenied(p) || t.private.hides(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// A symlink is read through by grepFile, so its target must pass
		// deny_paths too (a link to ~/.ssh/id_ed25519 must not be searched).
		if d.Type()&fs.ModeSymlink != 0 && t.refused(p) != "" {
			return nil
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// inOriginDenied reports whether p's place in the original repo is denied.
func (t *toolbox) inOriginDenied(p string) bool {
	q := t.inOrigin(p, t.cwd)
	return q != "" && t.deny.match(q) != ""
}

func globMatch(glob, p, root string) bool {
	if ok, _ := path.Match(glob, filepath.Base(p)); ok {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	ok, _ := path.Match(glob, filepath.ToSlash(rel))
	return ok
}

// grepFile appends "shown:N: line" for each matching line, up to searchHits.
func grepFile(p, shown string, rx *regexp.Regexp, hits []string) []string {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > searchFileMax {
		return hits
	}
	raw, err := os.ReadFile(p)
	if err != nil || isBinary(raw) {
		return hits
	}
	for n, line := range splitLines(strings.ToValidUTF8(string(raw), "�")) {
		if rx.MatchString(line) {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", shown, n+1, truncRunes(line, 300)))
			if len(hits) >= searchHits {
				break
			}
		}
	}
	return hits
}

func (t *toolbox) writeFile(a args) (string, error) {
	p := resolve(a.str("path"), t.cwd)
	if why := t.writeRefused(p); why != "" {
		return "", errors.New(why)
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return "", fmt.Errorf("%s is a folder", p)
	}
	content := a.str("content")
	_, statErr := os.Stat(p)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", errors.New(errText(err))
	}
	if err := atomicWrite(p, content); err != nil {
		return "", errors.New(errText(err))
	}
	verb := "created"
	if statErr == nil {
		verb = "replaced"
	}
	return fmt.Sprintf("%s %s: %d lines, %d bytes", verb, p, len(splitLines(content)), len(content)), nil
}

func (t *toolbox) editFile(a args) (string, error) {
	p := resolve(a.str("path"), t.cwd)
	if why := t.writeRefused(p); why != "" {
		return "", errors.New(why)
	}
	old, repl, all := a.str("old_string"), a.str("new_string"), a.bool("replace_all")
	if old == "" {
		return "", errors.New("old_string is empty: use write_file for a new file")
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("there is no file %s", p)
	}
	if err != nil {
		return "", fmt.Errorf("could not read %s: %s", p, errText(err))
	}
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("could not read %s: not UTF-8 text", p)
	}
	text := string(raw)
	n := strings.Count(text, old)
	switch {
	case n == 0:
		return "", fmt.Errorf("old_string is not in %s: nothing changed (read the file and copy the text exactly)", p)
	case n > 1 && !all:
		return "", fmt.Errorf("old_string is in %s %d times: nothing changed (add surrounding lines to make it "+
			"unique, or set replace_all)", p, n)
	}
	limit, done := 1, 1
	if all {
		limit, done = -1, n
	}
	if err := atomicWrite(p, strings.Replace(text, old, repl, limit)); err != nil {
		return "", errors.New(errText(err))
	}
	plural := ""
	if done > 1 {
		plural = "s"
	}
	return fmt.Sprintf("edited %s: %d replacement%s", p, done, plural), nil
}

// atomicWrite replaces p through a temp file in the same folder, keeping an
// existing file's mode (new files get 0644).
func atomicWrite(p, text string) error {
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".cerberus-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.WriteString(text)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func isBinary(raw []byte) bool {
	head := raw[:min(len(raw), 4096)]
	return strings.IndexByte(string(head), 0) >= 0
}

// splitLines splits like Python's str.splitlines for \n and \r\n text.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// errText is an OS error without the "open /path:" prefix.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
