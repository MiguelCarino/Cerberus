package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
)

// Configuration commands (day phase 4):
//
//	cerberus config path                  where config.json lives
//	cerberus config get [PATH] [--json]   one setting (or the whole config, token removed)
//	cerberus config set PATH VALUE        validated; applied live by a running daemon
//	cerberus config reset PATH            back to the default
//	cerberus config edit                  $EDITOR, then validate (re-open or keep the old file)
//	cerberus config validate [--json]     problems; exit 1 on an error
//	cerberus config schema [--json]       every setting with its help
//	cerberus export [--secrets-names]     profile bundle (JSON) on stdout or -o FILE
//	cerberus import FILE [--yes]          show the diff, ask, apply
//
// Changes go through the daemon when it runs (PATCH /api/config: one atomic,
// audited change that applies live where it can) and to the file directly
// when it does not, recorded in the audit log as actor "cli".

// loadConfigFile reads path (the default when ""). A missing file is not
// created: it reads as the defaults, with exists false.
func loadConfigFile(path string) (*config.Config, bool, error) {
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return nil, false, err
		}
		path = p
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		c := config.Defaults()
		c.SetPath(path)
		c.Server.InitDone = false
		return c, false, nil
	}
	c, err := config.Load(path)
	return c, err == nil, err
}

// configFlags parses -config (anywhere) plus extra flags.
func configFlags(name string, args []string, stderr io.Writer, extra func(*flag.FlagSet)) (string, []string, bool) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	setUsage(set)
	path := set.String("config", "", "config file (default ~/.config/cerberus/config.json)")
	if extra != nil {
		extra(set)
	}
	pos, err := parseAnywhere(set, args)
	if err != nil {
		return "", nil, false
	}
	return *path, pos, true
}

// errDaemonDown: the daemon did not answer, so the change goes to the file.
var errDaemonDown = errors.New("daemon not running")

// patchResult is a PATCH /api/config outcome: the view on success, the
// problems on a refusal.
type patchResult struct {
	View     api.ConfigView
	Problems []config.Problem
}

// patchDaemon sends a settings change to the running daemon. It returns
// errDaemonDown when nothing answers.
func patchDaemon(cfg *config.Config, set map[string]any) (patchResult, error) {
	var res patchResult
	tok := cfg.TokenSnapshot()
	if tok == "" {
		return res, errDaemonDown
	}
	c := newClient(cfg)
	raw, err := json.Marshal(api.ConfigPatch{Set: set})
	if err != nil {
		return res, err
	}
	req, err := http.NewRequestWithContext(context.Background(), "PATCH", c.base+"/api/config", bytes.NewReader(raw))
	if err != nil {
		return res, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Cerberus-Actor", audit.ActorCLI)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// A timeout means something answered on the address but did not
		// reply: a busy daemon. Writing the file then would be overwritten
		// by (or overwrite) the daemon, so it is an error, not "down".
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return res, fmt.Errorf("the daemon at %s did not answer in time; nothing was changed: %w", c.base, err)
		}
		return res, errDaemonDown
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		// The daemon saves every token rotation to its file, so one that
		// refuses this file's token is not running this config (another
		// install on the same address): the change belongs in the file.
		return res, errDaemonDown
	}
	if resp.StatusCode == http.StatusOK {
		return res, json.Unmarshal(body, &res.View)
	}
	var e struct {
		Error    string           `json:"error"`
		Problems []config.Problem `json:"problems"`
	}
	_ = json.Unmarshal(body, &e)
	res.Problems = e.Problems
	return res, fmt.Errorf("%s", errorText(resp.Status, body))
}

// applySettings changes settings through the daemon, else in the file. It
// prints the outcome and returns the exit code.
func applySettings(cfg *config.Config, set map[string]any, what string, stdout, stderr io.Writer) int {
	res, err := patchDaemon(cfg, set)
	switch {
	case err == nil:
		for _, ch := range res.View.Changed {
			how := "applied now"
			if !ch.Live {
				how = "applies after a restart (cerberus config restart, or restart the service)"
			}
			fmt.Fprintf(stdout, "%s: %s\n", ch.Path, how)
		}
		if len(res.View.Changed) == 0 {
			fmt.Fprintln(stdout, "No change (same values).")
		}
		printProblems(stderr, res.View.Problems, config.LevelWarn)
		return 0
	case !errors.Is(err, errDaemonDown):
		fmt.Fprintln(stderr, "cerberus config:", err)
		printProblems(stderr, res.Problems, "")
		return 1
	}
	old := cfg.Clone()
	probs, err := cfg.SetAll(set)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		printProblems(stderr, probs, "")
		return 1
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	var paths []string
	for _, ch := range config.Diff(old, cfg) {
		if !ch.Sensitive {
			paths = append(paths, ch.Path)
		}
	}
	cliAudit(stderr, "config."+what, "changed "+orNone(strings.Join(paths, ", "))+" (daemon not running)", paths...)
	if len(paths) == 0 {
		fmt.Fprintln(stdout, "No change (same values).")
	} else {
		fmt.Fprintf(stdout, "Saved %s to %s; it applies when cerberus starts.\n", strings.Join(paths, ", "), cfg.Path())
	}
	printProblems(stderr, probs, config.LevelWarn)
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

// printProblems lists problems (only those of level, when level is set).
func printProblems(w io.Writer, probs []config.Problem, level string) {
	for _, p := range probs {
		if level != "" && p.Level != level {
			continue
		}
		tag := "error"
		if p.Level == config.LevelWarn {
			tag = "warning"
		}
		fmt.Fprintf(w, "  %s: %s: %s\n", tag, p.Path, p.Message)
	}
}

func configCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: cerberus config get|set|reset|edit|validate|schema|path|restart …")
		return 2
	}
	sub, rest := args[0], args[1:]
	if isHelpArg(sub) {
		printHelp(stdout, "config")
		return 0
	}
	switch sub {
	case "path":
		path, _, ok := configFlags("config path", rest, stderr, nil)
		if !ok {
			return 2
		}
		if path == "" {
			p, err := config.DefaultPath()
			if err != nil {
				fmt.Fprintln(stderr, "cerberus config:", err)
				return 1
			}
			path = p
		}
		fmt.Fprintln(stdout, path)
		return 0
	case "get":
		return configGet(rest, stdout, stderr)
	case "set":
		path, pos, ok := configFlags("config set", rest, stderr, nil)
		if !ok {
			return 2
		}
		if len(pos) != 2 {
			fmt.Fprintln(stderr, "usage: cerberus config set PATH VALUE   (lists and objects as JSON)")
			return 2
		}
		cfg, _, err := loadConfigFile(path)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
		return applySettings(cfg, map[string]any{pos[0]: pos[1]}, "set", stdout, stderr)
	case "reset":
		path, pos, ok := configFlags("config reset", rest, stderr, nil)
		if !ok {
			return 2
		}
		if len(pos) != 1 {
			fmt.Fprintln(stderr, "usage: cerberus config reset PATH")
			return 2
		}
		cfg, _, err := loadConfigFile(path)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
		n := cfg.Clone()
		if err := n.Reset(pos[0]); err != nil {
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
		v, err := n.Get(pos[0])
		if err != nil {
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
		return applySettings(cfg, map[string]any{pos[0]: v}, "reset", stdout, stderr)
	case "validate":
		return configValidate(rest, stdout, stderr)
	case "schema":
		return configSchema(rest, stdout, stderr)
	case "edit":
		return configEdit(rest, stdin, stdout, stderr)
	case "restart":
		return configRestart(rest, stdout, stderr)
	}
	fmt.Fprintf(stderr, "cerberus config: unknown subcommand %q\n", sub)
	return 2
}

func configGet(args []string, stdout, stderr io.Writer) int {
	var asJSON bool
	path, pos, ok := configFlags("config get", args, stderr, func(set *flag.FlagSet) {
		set.BoolVar(&asJSON, "json", false, "print the value as JSON")
	})
	if !ok {
		return 2
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "usage: cerberus config get [PATH] [--json]")
		return 2
	}
	cfg, _, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	var v any = cfg.Redacted()
	if len(pos) == 1 {
		if v, err = cfg.Get(pos[0]); err != nil {
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
	}
	if s, isStr := v.(string); isStr && !asJSON {
		fmt.Fprintln(stdout, s)
		return 0
	}
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(stdout, string(raw))
	return 0
}

func configValidate(args []string, stdout, stderr io.Writer) int {
	var asJSON bool
	path, _, ok := configFlags("config validate", args, stderr, func(set *flag.FlagSet) {
		set.BoolVar(&asJSON, "json", false, "print the problems as JSON")
	})
	if !ok {
		return 2
	}
	cfg, exists, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	probs := cfg.Validate()
	if asJSON {
		if probs == nil {
			probs = []config.Problem{}
		}
		raw, _ := json.MarshalIndent(probs, "", "  ")
		fmt.Fprintln(stdout, string(raw))
	} else {
		if !exists {
			fmt.Fprintf(stdout, "No config at %s yet; checking the defaults.\n", cfg.Path())
		}
		if len(probs) == 0 {
			fmt.Fprintln(stdout, "Config is valid.")
		}
		printProblems(stdout, probs, "")
	}
	if config.HasErrors(probs) {
		return 1
	}
	return 0
}

func configSchema(args []string, stdout, stderr io.Writer) int {
	var asJSON, basic bool
	_, pos, ok := configFlags("config schema", args, stderr, func(set *flag.FlagSet) {
		set.BoolVar(&asJSON, "json", false, "print the schema as JSON")
		set.BoolVar(&basic, "basic", false, "only the basic settings")
	})
	if !ok {
		return 2
	}
	fields := config.Schema()
	if asJSON {
		raw, _ := json.MarshalIndent(api.ConfigSchema{Groups: config.Groups(), Fields: fields}, "", "  ")
		fmt.Fprintln(stdout, string(raw))
		return 0
	}
	filter := ""
	if len(pos) > 0 {
		filter = strings.ToLower(strings.Join(pos, " "))
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	for _, g := range config.Groups() {
		var rows []config.Field
		for _, f := range fields {
			if f.Group != g || (basic && f.Level != config.LevelBasic) {
				continue
			}
			if filter != "" && !strings.Contains(strings.ToLower(f.Path+" "+f.Label+" "+f.Help), filter) {
				continue
			}
			rows = append(rows, f)
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s\n", g)
		for _, f := range rows {
			kind := f.Kind
			if len(f.Enum) > 0 {
				kind = "enum[" + strings.Join(f.Enum, "|") + "]"
			}
			if f.Unit != "" {
				kind += " (" + f.Unit + ")"
			}
			flags := ""
			if f.Restart {
				flags = " [restart]"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s%s\n", f.Path, kind, f.Label, flags)
			if f.Help != "" {
				fmt.Fprintf(tw, "  \t\t%s\n", f.Help)
			}
		}
	}
	return boolCode(tw.Flush() == nil)
}

func boolCode(ok bool) int {
	if ok {
		return 0
	}
	return 1
}

// editorArgv is $VISUAL or $EDITOR (split on spaces), else a platform default.
func editorArgv() []string {
	for _, k := range []string{"VISUAL", "EDITOR"} {
		if v := strings.Fields(os.Getenv(k)); len(v) > 0 {
			return v
		}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	return []string{"vi"}
}

// configEdit opens a copy of the config in the editor, validates it, and
// re-opens it on an error (or keeps the old file). A valid edit goes to the
// daemon as one atomic change when it runs, else replaces the file.
func configEdit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, _, ok := configFlags("config edit", args, stderr, nil)
	if !ok {
		return 2
	}
	cfg, exists, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	if !exists {
		if err := cfg.Save(); err != nil { // first run: a file to edit, with a token
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
	}
	raw, err := os.ReadFile(cfg.Path())
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	tmp, err := os.CreateTemp(filepath.Dir(cfg.Path()), ".config-edit-*.json")
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_, werr := tmp.Write(raw)
	if cerr := tmp.Close(); werr != nil || cerr != nil {
		fmt.Fprintln(stderr, "cerberus config: cannot write the copy to edit")
		return 1
	}
	answers := newLineReader(stdin)
	for {
		argv := append(editorArgv(), tmpName)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(stderr, "cerberus config: editor:", err, "(the config is unchanged)")
			return 1
		}
		edited, err := os.ReadFile(tmpName)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus config:", err)
			return 1
		}
		set, probs, perr := editedChanges(cfg, edited)
		if perr == nil {
			if len(set) == 0 {
				fmt.Fprintln(stdout, "No change.")
				return 0
			}
			return applySettings(cfg, set, "edit", stdout, stderr)
		}
		fmt.Fprintln(stderr, "cerberus config:", perr)
		printProblems(stderr, probs, "")
		if !answers.yesNo(stdout, "Open the editor again? (no keeps the old file)", true) {
			fmt.Fprintln(stdout, "Kept the old config.")
			return 1
		}
	}
}

// editedChanges parses an edited config and returns the changed settings
// (by top-level schema path), refusing JSON errors and new validation
// errors. The token and read-only settings are never taken from the edit.
func editedChanges(cur *config.Config, edited []byte) (map[string]any, []config.Problem, error) {
	n := config.Defaults()
	dec := json.NewDecoder(bytes.NewReader(edited))
	dec.DisallowUnknownFields()
	if err := dec.Decode(n); err != nil {
		return nil, nil, fmt.Errorf("not valid config JSON: %w", err)
	}
	// Load's migration: a file without server.init_done (written before day
	// phase 4) is an install already set up; without this an untouched edit
	// would reset it and bring the first-run welcome back.
	if !config.HasInitDone(edited) {
		n.Server.InitDone = cur.Server.InitDone
	}
	set := map[string]any{}
	for _, ch := range config.Diff(cur, n) {
		if ch.Sensitive {
			continue
		}
		if f, ok := config.SchemaField(ch.Path); ok && f.ReadOnly {
			continue
		}
		set[ch.Path] = ch.New
	}
	if len(set) == 0 {
		return set, nil, nil
	}
	probs, err := cur.Clone().SetAll(set)
	if err != nil {
		return nil, probs, err
	}
	return set, nil, nil
}

// configRestart asks the daemon to restart (POST /api/config/restart).
func configRestart(args []string, stdout, stderr io.Writer) int {
	path, _, ok := configFlags("config restart", args, stderr, nil)
	if !ok {
		return 2
	}
	cfg, _, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus config:", err)
		return 1
	}
	if err := newClient(cfg).do(context.Background(), "POST", "/api/config/restart", nil, nil); err != nil {
		fmt.Fprintln(stderr, "cerberus config restart:", err)
		return 1
	}
	fmt.Fprintln(stdout, "Cerberus is restarting.")
	return 0
}

// ---------- export / import ----------

func exportCmd(args []string, stdout, stderr io.Writer) int {
	var names bool
	var sections, out string
	path, pos, ok := configFlags("export", args, stderr, func(set *flag.FlagSet) {
		set.BoolVar(&names, "secrets-names", false, "keep the names of the vault secrets settings refer to (never their values)")
		set.StringVar(&sections, "sections", "", "comma-separated sections (default all: "+strings.Join(config.BundleSections(), ",")+")")
		set.StringVar(&out, "o", "", "write to this file instead of stdout")
	})
	if !ok || len(pos) > 0 {
		if ok {
			fmt.Fprintln(stderr, "usage: cerberus export [--secrets-names] [--sections a,b] [-o FILE]")
		}
		return 2
	}
	cfg, _, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus export:", err)
		return 1
	}
	opts := config.ExportOptions{SecretNames: names}
	if sections != "" {
		opts.Sections = strings.Split(sections, ",")
	}
	raw, err := cfg.Export(opts)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus export:", err)
		return 1
	}
	raw = append(raw, '\n')
	if out == "" {
		_, err = stdout.Write(raw)
	} else {
		err = os.WriteFile(out, raw, 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "cerberus export:", err)
		return 1
	}
	return 0
}

func importCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var yes bool
	path, pos, ok := configFlags("import", args, stderr, func(set *flag.FlagSet) {
		set.BoolVar(&yes, "yes", false, "apply without asking")
	})
	if !ok || len(pos) != 1 {
		if ok {
			fmt.Fprintln(stderr, "usage: cerberus import FILE [--yes]")
		}
		return 2
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "cerberus import:", err)
		return 1
	}
	cfg, _, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus import:", err)
		return 1
	}
	changes, _, err := cfg.ImportWith(data, config.ImportOptions{Place: placeModel(cfg)})
	if err != nil {
		fmt.Fprintln(stderr, "cerberus import:", err)
		return 1
	}
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "The bundle matches this config; nothing to change.")
		printBundleModels(stdout, data)
		return 0
	}
	fmt.Fprintln(stdout, "The import changes:")
	printDiff(stdout, changes)
	if b, err := config.ParseBundle(data); err == nil && len(b.SecretNames) > 0 {
		fmt.Fprintf(stdout, "Secrets it refers to (add them with cerberus secret set NAME): %s\n", strings.Join(b.SecretNames, ", "))
	}
	if !yes && !newLineReader(stdin).yesNo(stdout, "Apply these changes?", false) {
		fmt.Fprintln(stdout, "Nothing changed.")
		return 1
	}
	set := map[string]any{}
	for _, ch := range changes {
		if !ch.Sensitive {
			set[ch.Path] = ch.New
		}
	}
	code := applySettings(cfg, set, "import", stdout, stderr)
	if code == 0 {
		printBundleModels(stdout, data)
	}
	return code
}

// placeModel is where a bundle model's download lands under the first
// llama.cpp models folder ("" unknown).
func placeModel(cfg *config.Config) func(config.BundleModel) string {
	return func(m config.BundleModel) string {
		dirs := cfg.Engines.LlamaCpp.ModelDirs
		if m.Ref == "" || len(dirs) == 0 {
			return ""
		}
		dir := dirs[0]
		if strings.HasPrefix(dir, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				dir = home + strings.TrimPrefix(dir, "~")
			}
		}
		p, err := downloads.HFDest(dir, m.Ref)
		if err != nil {
			return ""
		}
		return p
	}
}

// printBundleModels lists the models a bundle names and how to download
// each one; nothing is downloaded by an import.
func printBundleModels(w io.Writer, data []byte) {
	b, err := config.ParseBundle(data)
	if err != nil || len(b.Models) == 0 {
		return
	}
	fmt.Fprintf(w, "The bundle names %d local models. Nothing was downloaded; get the ones this machine lacks with\n", len(b.Models))
	fmt.Fprintln(w, "cerberus pull, or in Settings → Import & export, which can also drop them from the registry:")
	for _, m := range b.Models {
		switch {
		case m.Ref == "":
			fmt.Fprintf(w, "  %-28s %s (no download link known)\n", m.ID, m.Backend)
		case m.Backend == "ollama":
			fmt.Fprintf(w, "  cerberus pull ollama:%s\n", m.Ref)
		case m.Backend == "lmstudio":
			fmt.Fprintf(w, "  cerberus pull lms:%s\n", m.Ref)
		default:
			fmt.Fprintf(w, "  cerberus pull %s\n", m.Ref)
		}
	}
}

// printDiff shows each changed setting with its old and new value as JSON,
// in full: the import prompt approves exactly what is shown, and a command
// or policy change past any cut-off would be approved unseen. A long value
// is indented over several lines.
func printDiff(w io.Writer, changes []config.PathChange) {
	short := func(v any) string {
		raw, _ := json.Marshal(v)
		s := string(raw)
		if s == "" || s == "null" {
			return "(none)"
		}
		if len(s) > 100 {
			if ind, err := json.MarshalIndent(v, "      ", "  "); err == nil {
				lines := strings.Split(string(ind), "\n")
				for i := range lines {
					lines[i] = clean(lines[i])
				}
				return strings.Join(lines, "\n")
			}
		}
		return clean(s)
	}
	for _, ch := range changes {
		tag := ""
		if ch.Restart {
			tag = "  [restart]"
		}
		fmt.Fprintf(w, "  %s%s\n    - %s\n    + %s\n", ch.Path, tag, short(ch.Old), short(ch.New))
	}
}

// ---------- answers from stdin ----------

// lineReader reads answers line by line from one buffered reader (several
// questions must not lose buffered input between them).
type lineReader struct {
	src io.Reader
	buf []byte
	eof bool
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{src: r} }

// line returns the next line without its ending; io.EOF when input ended.
func (l *lineReader) line() (string, error) {
	for {
		if i := bytes.IndexByte(l.buf, '\n'); i >= 0 {
			s := string(l.buf[:i])
			l.buf = l.buf[i+1:]
			return strings.TrimSuffix(s, "\r"), nil
		}
		if l.eof {
			if len(l.buf) == 0 {
				return "", io.EOF
			}
			s := string(l.buf)
			l.buf = nil
			return s, nil
		}
		if len(l.buf) > 4096 {
			return "", errors.New("answer too long")
		}
		chunk := make([]byte, 512)
		n, err := l.src.Read(chunk)
		l.buf = append(l.buf, chunk[:n]...)
		if err != nil {
			l.eof = true
		}
	}
}

// ask prints a question and returns the trimmed answer, or def when the
// answer is empty or input has ended.
func (l *lineReader) ask(w io.Writer, question, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s] ", question, def)
	} else {
		fmt.Fprintf(w, "%s ", question)
	}
	s, err := l.line()
	if err != nil {
		fmt.Fprintln(w)
		return def
	}
	if s = strings.TrimSpace(s); s == "" {
		return def
	}
	return s
}

// yesNo asks a yes/no question.
func (l *lineReader) yesNo(w io.Writer, question string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(w, "%s [%s] ", question, hint)
	s, err := l.line()
	if err != nil {
		fmt.Fprintln(w)
		return def
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "s", "si", "sí":
		return true
	case "n", "no":
		return false
	}
	return def
}
