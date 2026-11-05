package main

import (
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
	"slices"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/api/qr"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/platform"
)

// `cerberus init` (day phase 4): one command from a fresh machine to a
// working setup. It looks at the machine (engines and their models, the GPU,
// installed coding agents, Tailscale, the Atrium notify script, voice
// backends, bwrap, RAPL), asks at most six questions (default model, phone
// access, HTTPS certificate, ntfy topic, household electricity, install the
// service), writes the config with server.init_done set, and prints a
// summary, the pairing link with a QR code and the doctor's result.
//
// Every outside fact goes through a machine, so tests run init against a
// fixture: fake binaries on a temp PATH, fake HTTP answers and temp folders.
// Init never requests a certificate: it only checks whether `tailscale cert`
// would work (HTTPS enabled for the tailnet) and prints the command.

// machine is what init may ask of the computer.
type machine struct {
	goos     string
	home     string
	lookPath func(name string) (string, error)
	// output runs a command (bounded) and returns its stdout.
	output func(ctx context.Context, name string, args ...string) ([]byte, error)
	// httpGet fetches a local URL (engine probes); an error means not running.
	httpGet  func(ctx context.Context, url string) ([]byte, error)
	stat     func(path string) (fs.FileInfo, error)
	readFile func(path string) ([]byte, error)
	readable func(path string) bool
	ifaceIPs func() []string
	// install installs and starts the service (cerberus install-service).
	install func(out io.Writer) error
	// doctor adjusts the doctor's environment (tests).
	doctor func(doctor.Env) doctor.Env
}

const probeTimeout = 3 * time.Second

func realMachine(stderr io.Writer) machine {
	home, _ := os.UserHomeDir()
	client := &http.Client{Timeout: probeTimeout}
	return machine{
		goos: runtime.GOOS, home: home, lookPath: exec.LookPath,
		output: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, name, args...).Output()
		},
		httpGet: func(ctx context.Context, url string) ([]byte, error) {
			req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
			if err != nil {
				return nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return nil, fmt.Errorf("%s: %s", url, resp.Status)
			}
			return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		},
		stat: os.Stat, readFile: os.ReadFile,
		readable: func(p string) bool {
			f, err := os.Open(p)
			if err != nil {
				return false
			}
			defer f.Close()
			_, err = f.Read(make([]byte, 1))
			return err == nil
		},
		ifaceIPs: interfaceIPs,
		install: func(out io.Writer) error {
			in, err := defaultInstaller(out)
			if err != nil {
				return err
			}
			in.audit = func(event, detail string, targets ...string) { cliAudit(stderr, event, detail, targets...) }
			return in.install()
		},
		doctor: func(e doctor.Env) doctor.Env { return doctorDeps(e) },
	}
}

// Detected is what init found (printed with --json).
type Detected struct {
	Engines   []EngineFound `json:"engines"`
	Models    []string      `json:"models"` // every model ID, in engine order
	GPU       string        `json:"gpu"`    // "" when none was found
	Agents    []string      `json:"agents"` // installed coding agents (claude, opencode, aider)
	Tailscale TailscaleInfo `json:"tailscale"`
	LANIP     string        `json:"lan_ip,omitempty"`
	Notify    string        `json:"notify_script,omitempty"` // Atrium's notify script
	Voice     []string      `json:"voice"`                   // transcription backends found
	FFmpeg    bool          `json:"ffmpeg"`
	Bwrap     bool          `json:"bwrap"`
	RAPL      string        `json:"rapl"` // "readable", "root-only" or "absent"
}

// EngineFound is one model engine.
type EngineFound struct {
	Name    string   `json:"name"` // lmstudio, ollama, llamacpp
	Found   bool     `json:"found"`
	Detail  string   `json:"detail"`
	Running bool     `json:"running"`
	Models  []string `json:"models"`
}

// TailscaleInfo is the tailnet view of this machine.
type TailscaleInfo struct {
	Found   bool   `json:"found"`
	IP      string `json:"ip,omitempty"`
	DNSName string `json:"dns_name,omitempty"` // desk.tailnet.ts.net
	CertOK  bool   `json:"cert_ok"`            // HTTPS certificates are enabled for the tailnet
}

const raplPath = "/sys/class/powercap/intel-rapl:0/energy_uj"

// detect looks at the machine. It never fails: what it cannot see is
// reported as absent.
func (m machine) detect(ctx context.Context, cfg *config.Config) Detected {
	var d Detected
	have := func(name string) (string, bool) {
		p, err := m.lookPath(name)
		return p, err == nil
	}

	// LM Studio: the lms tool (also ~/.lmstudio/bin/lms) and its model list.
	lms := EngineFound{Name: "lmstudio"}
	bin, ok := have(orDefault(cfg.Engines.LMStudio.Binary, "lms"))
	if !ok {
		alt := filepath.Join(m.home, ".lmstudio", "bin", "lms")
		if _, err := m.stat(alt); err == nil {
			bin, ok = alt, true
		}
	}
	if ok {
		lms.Found, lms.Detail = true, bin
		if raw, err := m.output(ctx, bin, "ls", "--json"); err == nil {
			lms.Models = lmsModels(raw)
		}
		if _, err := m.httpGet(ctx, strings.TrimRight(cfg.Engines.LMStudio.BaseURL, "/")+"/models"); err == nil {
			lms.Running = true
		}
	}
	d.Engines = append(d.Engines, lms)

	// Ollama: its API answers with the installed models.
	oll := EngineFound{Name: "ollama"}
	if p, ok := have("ollama"); ok {
		oll.Found, oll.Detail = true, p
	}
	if raw, err := m.httpGet(ctx, strings.TrimRight(cfg.Engines.Ollama.URL, "/")+"/api/tags"); err == nil {
		oll.Found, oll.Running = true, true
		if oll.Detail == "" {
			oll.Detail = cfg.Engines.Ollama.URL
		}
		var tags struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.Unmarshal(raw, &tags) == nil {
			for _, t := range tags.Models {
				if t.Name != "" {
					oll.Models = append(oll.Models, t.Name)
				}
			}
		}
	}
	d.Engines = append(d.Engines, oll)

	// llama.cpp: llama-server plus .gguf files in the model folders.
	lc := EngineFound{Name: "llamacpp"}
	if p, ok := have(orDefault(cfg.Engines.LlamaCpp.Binary, "llama-server")); ok {
		lc.Found, lc.Detail = true, p
		lc.Models = scanModels(cfg.Engines.LlamaCpp.ModelDirs)
	}
	d.Engines = append(d.Engines, lc)
	seen := map[string]bool{}
	for _, e := range d.Engines {
		for _, id := range e.Models {
			if !seen[id] {
				seen[id] = true
				d.Models = append(d.Models, id)
			}
		}
	}

	// GPU: nvidia-smi, else an AMD card in sysfs.
	if p, ok := have(orDefault(cfg.GPU.NvidiaSmi, "nvidia-smi")); ok {
		if raw, err := m.output(ctx, p, "--query-gpu=name,memory.total", "--format=csv,noheader"); err == nil {
			d.GPU = strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
		}
	}
	if d.GPU == "" && m.goos == "linux" {
		for i := range 4 {
			v := "/sys/class/drm/card" + strconv.Itoa(i) + "/device/vendor"
			if raw, err := m.readFile(v); err == nil && strings.TrimSpace(string(raw)) == "0x1002" {
				d.GPU = "AMD (card" + strconv.Itoa(i) + ")"
				break
			}
		}
	}

	for _, a := range []string{"claude", "opencode", "aider"} {
		if _, ok := have(a); ok {
			d.Agents = append(d.Agents, a)
		}
	}

	// Tailscale: its IPv4 and whether `tailscale cert` would work (the
	// tailnet has HTTPS on: Self.CertDomains is set). Nothing is requested.
	if p, ok := have("tailscale"); ok {
		d.Tailscale.Found = true
		if raw, err := m.output(ctx, p, "ip", "-4"); err == nil {
			if ip := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0]); net.ParseIP(ip) != nil {
				d.Tailscale.IP = ip
			}
		}
		if raw, err := m.output(ctx, p, "status", "--json"); err == nil {
			var st struct {
				Self struct {
					DNSName string `json:"DNSName"`
				} `json:"Self"`
				CertDomains []string `json:"CertDomains"`
			}
			if json.Unmarshal(raw, &st) == nil {
				d.Tailscale.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
				d.Tailscale.CertOK = len(st.CertDomains) > 0
				if d.Tailscale.CertOK && !slices.Contains(st.CertDomains, d.Tailscale.DNSName) {
					d.Tailscale.DNSName = st.CertDomains[0]
				}
			}
		}
	}
	for _, ip := range m.ifaceIPs() {
		if pip := net.ParseIP(ip); pip != nil && pip.To4() != nil && pip.IsPrivate() && ip != d.Tailscale.IP {
			d.LANIP = ip
			break
		}
	}

	if p := filepath.Join(m.home, ".config", "hypr", "scripts", "notify"); m.isFile(p) {
		d.Notify = p
	}
	if p := filepath.Join(m.home, ".config", "hypr", "scripts", "voice.py"); m.isFile(p) {
		d.Voice = append(d.Voice, "atrium")
	}
	for _, b := range []string{"whisper-cli", "faster-whisper", "whisper"} {
		if _, ok := have(b); ok {
			d.Voice = append(d.Voice, b)
		}
	}
	_, d.FFmpeg = have(orDefault(cfg.Voice.FFmpeg, "ffmpeg"))
	_, d.Bwrap = have("bwrap")
	switch {
	case m.goos != "linux":
		d.RAPL = "absent"
	case m.readable(raplPath):
		d.RAPL = "readable"
	default:
		if _, err := m.stat(raplPath); err == nil {
			d.RAPL = "root-only"
		} else {
			d.RAPL = "absent"
		}
	}
	return d
}

func (m machine) isFile(p string) bool {
	st, err := m.stat(p)
	return err == nil && !st.IsDir()
}

// lmsModels reads `lms ls --json` (a list, or {"models": [...]}) and keeps
// the LLMs (embedding models are skipped).
func lmsModels(raw []byte) []string {
	type entry struct {
		Type     string `json:"type"`
		ModelKey string `json:"modelKey"`
		Path     string `json:"path"`
	}
	var list []entry
	if json.Unmarshal(raw, &list) != nil {
		var wrap struct {
			Models []entry `json:"models"`
		}
		if json.Unmarshal(raw, &wrap) != nil {
			return nil
		}
		list = wrap.Models
	}
	var out []string
	for _, e := range list {
		if e.Type == "embedding" {
			continue
		}
		if id := orDefault(e.ModelKey, e.Path); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// scanModels lists .gguf model files (not projectors) up to three levels
// down, by file name without the extension, as the llama.cpp engine names
// them.
func scanModels(dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, root := range dirs {
		root = filepath.Clean(expandTilde(root))
		_ = filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if de.IsDir() {
				if strings.Count(rel, string(filepath.Separator)) >= 3 {
					return fs.SkipDir
				}
				return nil
			}
			name := de.Name()
			if !strings.EqualFold(filepath.Ext(name), ".gguf") || strings.Contains(strings.ToLower(name), "mmproj") {
				return nil
			}
			id := strings.TrimSuffix(name, filepath.Ext(name))
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
			return nil
		})
	}
	return out
}

func expandTilde(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// InitAnswers are the choices init made (asked, or the defaults).
type InitAnswers struct {
	Model        string  `json:"model"`
	Phone        string  `json:"phone"` // none | lan | tailscale
	HTTPS        bool    `json:"https"`
	Ntfy         string  `json:"ntfy"`
	HouseholdKWh float64 `json:"household_kwh"`
	Service      bool    `json:"service"`
}

// InitResult is init's --json output.
type InitResult struct {
	ConfigPath string           `json:"config_path"`
	Created    bool             `json:"created"`
	Detected   Detected         `json:"detected"`
	Answers    InitAnswers      `json:"answers"`
	Changed    []string         `json:"changed"`
	PairURL    string           `json:"pair_url"`
	CertCmd    string           `json:"cert_command,omitempty"`
	Service    string           `json:"service,omitempty"`
	Problems   []config.Problem `json:"problems"`
	Doctor     doctor.Report    `json:"doctor"`
}

func initCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runInit(args, stdin, stdout, stderr, realMachine(stderr))
}

func runInit(args []string, stdin io.Reader, stdout, stderr io.Writer, m machine) int {
	var yes, asJSON bool
	path, pos, ok := configFlags("init", args, stderr, func(set *flag.FlagSet) {
		set.BoolVar(&yes, "yes", false, "take every default without asking")
		set.BoolVar(&asJSON, "json", false, "print the result as JSON (implies --yes)")
	})
	if !ok || len(pos) > 0 {
		if ok {
			fmt.Fprintln(stderr, "usage: cerberus init [--yes] [--json] [-config PATH]")
		}
		return 2
	}
	if asJSON {
		yes = true
	}
	cfg, exists, err := loadConfigFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus init:", err)
		return 1
	}
	out := stdout
	if asJSON {
		out = io.Discard
	}
	ctx := context.Background()
	fmt.Fprintln(out, "Cerberus setup: looking at this machine…")
	d := m.detect(ctx, cfg)
	printDetected(out, d)

	ans := m.ask(out, newLineReader(stdin), yes, cfg, d)
	set, certCmd := initSettings(cfg, d, ans)

	before := cfg.Clone()
	if !exists && cfg.TokenSnapshot() == "" {
		cfg.SetToken(config.NewToken())
	}
	probs, err := cfg.SetAll(set)
	if err != nil {
		fmt.Fprintln(stderr, "cerberus init:", err)
		printProblems(stderr, probs, "")
		return 1
	}
	// A running daemon would overwrite the file at its next save: send it
	// the change instead (it saves and applies what it can live).
	if _, perr := patchDaemon(before, set); perr != nil {
		if !errors.Is(perr, errDaemonDown) {
			fmt.Fprintln(stderr, "cerberus init: the running daemon refused the settings:", perr)
			return 1
		}
		if err := cfg.Save(); err != nil {
			fmt.Fprintln(stderr, "cerberus init:", err)
			return 1
		}
		cliAudit(stderr, "config.init", "first-run setup written (daemon not running)", cfg.Path())
	}
	res := InitResult{ConfigPath: cfg.Path(), Created: !exists, Detected: d, Answers: ans, CertCmd: certCmd,
		Problems: cfg.Validate()}
	if res.Problems == nil {
		res.Problems = []config.Problem{}
	}
	for _, ch := range config.Diff(before, cfg) {
		if !ch.Sensitive && ch.Path != "server.token_created" {
			res.Changed = append(res.Changed, ch.Path)
		}
	}

	if ans.Service {
		if err := m.install(out); err != nil {
			res.Service = "failed: " + err.Error()
		} else {
			res.Service = "installed and started"
		}
	}

	urls := pairingURLs(cfg.Server, m.ifaceIPs())
	res.PairURL = pickPairURL(urls, "", cfg.TokenSnapshot())
	res.Doctor = m.runDoctor(ctx, cfg)

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return 0
	}
	printInitSummary(stdout, res)
	return 0
}

// ask puts the (at most six) questions, skipping those that do not apply.
// With yes it takes the defaults, except that phone access stays "none" and
// HTTPS off: an unattended run never opens the daemon to the network (the
// interactive default is Tailscale when it is up; see SCOPE.md).
func (m machine) ask(w io.Writer, in *lineReader, yes bool, cfg *config.Config, d Detected) InitAnswers {
	ans := InitAnswers{Model: cfg.Engines.DefaultModel, Phone: "none", HouseholdKWh: cfg.Tariff.HouseholdKWhMonth}
	if ans.Model == "" && len(d.Models) > 0 {
		ans.Model = d.Models[0]
	}
	if yes {
		return ans
	}
	fmt.Fprintln(w)
	// 1. Default model.
	if len(d.Models) > 0 {
		fmt.Fprintln(w, "Models found:")
		def := "1"
		for i, id := range d.Models {
			fmt.Fprintf(w, "  %d. %s\n", i+1, id)
			if id == ans.Model {
				def = strconv.Itoa(i + 1)
			}
		}
		a := in.ask(w, "1/6 Default model (number or name):", def)
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(d.Models) {
			ans.Model = d.Models[n-1]
		} else if a != "" {
			ans.Model = a
		}
	}
	// 2. Phone access.
	opts := []string{"none"}
	if d.LANIP != "" {
		opts = append(opts, "lan")
	}
	if d.Tailscale.IP != "" {
		opts = append(opts, "tailscale")
	}
	if len(opts) > 1 {
		fmt.Fprintln(w, "Phone access: none (this computer only), lan (home network)"+
			map[bool]string{true: ", tailscale (your tailnet, from anywhere)"}[d.Tailscale.IP != ""])
		def := "none"
		if d.Tailscale.IP != "" {
			def = "tailscale" // private by design: the safest way to reach it from a phone
		}
		for {
			a := strings.ToLower(in.ask(w, "2/6 Phone access ("+strings.Join(opts, "/")+"):", def))
			if slices.Contains(opts, a) {
				ans.Phone = a
				break
			}
			fmt.Fprintln(w, "  Please answer one of: "+strings.Join(opts, ", "))
		}
	}
	// 3. HTTPS certificate (Tailscale only, when the tailnet allows it).
	if ans.Phone == "tailscale" && d.Tailscale.CertOK && d.Tailscale.DNSName != "" {
		ans.HTTPS = in.yesNo(w, "3/6 Use an HTTPS certificate from Tailscale for "+d.Tailscale.DNSName+"?", true)
	}
	// 4. ntfy topic.
	ans.Ntfy = in.ask(w, "4/6 ntfy topic for phone notifications (empty for none):", "")
	// 5. Household electricity.
	for {
		def := ""
		if ans.HouseholdKWh > 0 {
			def = strconv.FormatFloat(ans.HouseholdKWh, 'f', -1, 64)
		}
		a := in.ask(w, "5/6 Your home's monthly electricity use in kWh, from the bill (empty to skip):", def)
		if a == "" {
			break
		}
		if f, err := strconv.ParseFloat(strings.TrimSuffix(strings.ToLower(a), "kwh"), 64); err == nil && f >= 0 {
			ans.HouseholdKWh = f
			break
		}
		fmt.Fprintln(w, "  Please give a number, such as 250.")
	}
	// 6. Service.
	if m.goos == "linux" {
		ans.Service = in.yesNo(w, "6/6 Install and start the background service now?", false)
	}
	return ans
}

// initSettings turns the answers into settings, plus the `tailscale cert`
// command to run when HTTPS was chosen ("" otherwise).
func initSettings(cfg *config.Config, d Detected, ans InitAnswers) (map[string]any, string) {
	set := map[string]any{"server.init_done": true}
	if ans.Model != "" {
		set["engines.default_model"] = ans.Model
	}
	_, port, err := net.SplitHostPort(cfg.Server.Listen)
	if err != nil || port == "" {
		port = "7340"
	}
	extra := func(ip string) {
		a := net.JoinHostPort(ip, port)
		if !slices.Contains(cfg.Server.ExtraListen, a) {
			set["server.extra_listen"] = append(slices.Clone(cfg.Server.ExtraListen), a)
		}
	}
	certCmd := ""
	switch ans.Phone {
	case "lan":
		if d.LANIP != "" {
			extra(d.LANIP)
			set["server.public_url"] = "http://" + net.JoinHostPort(d.LANIP, port)
		}
	case "tailscale":
		if d.Tailscale.IP != "" {
			extra(d.Tailscale.IP)
			host := orDefault(d.Tailscale.DNSName, d.Tailscale.IP)
			if d.Tailscale.DNSName != "" {
				set["server.allowed_hosts"] = appendNew(cfg.Server.AllowedHosts, d.Tailscale.DNSName)
			}
			scheme := "http://"
			if ans.HTTPS && d.Tailscale.DNSName != "" {
				scheme = "https://"
				dir := filepath.Join(cfg.Dir(), "tls")
				crt := filepath.Join(dir, d.Tailscale.DNSName+".crt")
				key := filepath.Join(dir, d.Tailscale.DNSName+".key")
				set["server.tls_cert"], set["server.tls_key"] = crt, key
				certCmd = fmt.Sprintf("mkdir -p %q && tailscale cert --cert-file %q --key-file %q %s", dir, crt, key, d.Tailscale.DNSName)
			}
			set["server.public_url"] = scheme + net.JoinHostPort(host, port)
		}
	}
	if t := strings.TrimSpace(ans.Ntfy); t != "" {
		if !strings.Contains(t, "://") {
			t = "https://ntfy.sh/" + strings.Trim(t, "/")
		}
		set["notify.ntfy_url"] = t
	}
	if ans.HouseholdKWh > 0 {
		set["tariff.household_kwh_month"] = ans.HouseholdKWh
	}
	if d.Notify != "" && len(cfg.Notify.Command) == 0 {
		set["notify.command"] = []string{d.Notify, "{title}", "{body}"}
	}
	return set, certCmd
}

func appendNew(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(slices.Clone(list), v)
}

// runDoctor runs the checklist against the new config.
func (m machine) runDoctor(ctx context.Context, cfg *config.Config) doctor.Report {
	env := doctor.Env{Cfg: cfg, ConfigPath: cfg.Path(), Home: m.home, GOOS: m.goos, LookPath: m.lookPath,
		Output: func(ctx context.Context, name string, args ...string) (string, error) {
			raw, err := m.output(ctx, name, args...)
			return string(raw), err
		},
		Reachable: func(ctx context.Context, url string) error { _, err := m.httpGet(ctx, url); return err },
		Stat:      m.stat, Readable: m.readable}
	if data, err := platform.DataDir(); err == nil {
		env.DataDir = data
	}
	if m.doctor != nil {
		env = m.doctor(env)
	}
	return doctor.Run(ctx, env)
}

func printDetected(w io.Writer, d Detected) {
	yn := func(b bool, s string) string {
		if b {
			return s
		}
		return "not found"
	}
	for _, e := range d.Engines {
		line := yn(e.Found, e.Detail)
		if e.Found {
			line += fmt.Sprintf(" (%d models", len(e.Models))
			if e.Running {
				line += ", running"
			}
			line += ")"
		}
		fmt.Fprintf(w, "  %-12s %s\n", engineLabel(e.Name), clean(line))
	}
	fmt.Fprintf(w, "  %-12s %s\n", "GPU", clean(orDefault(d.GPU, "none found")))
	fmt.Fprintf(w, "  %-12s %s\n", "Agents", orDefault(strings.Join(d.Agents, ", "), "none besides the built-in agent"))
	ts := "not found"
	if d.Tailscale.Found {
		ts = orDefault(d.Tailscale.IP, "installed, not connected")
		if d.Tailscale.DNSName != "" {
			ts += " (" + d.Tailscale.DNSName + ")"
		}
		if d.Tailscale.CertOK {
			ts += ", HTTPS certificates available"
		}
	}
	fmt.Fprintf(w, "  %-12s %s\n", "Tailscale", clean(ts))
	fmt.Fprintf(w, "  %-12s %s\n", "Notify", clean(orDefault(d.Notify, "desktop notify-send if present")))
	voice := orDefault(strings.Join(d.Voice, ", "), "none")
	if len(d.Voice) > 0 && !d.FFmpeg {
		voice += " (ffmpeg missing)"
	}
	fmt.Fprintf(w, "  %-12s %s\n", "Voice", voice)
	fmt.Fprintf(w, "  %-12s %s\n", "Sandbox", yn(d.Bwrap, "bwrap"))
	fmt.Fprintf(w, "  %-12s %s\n", "CPU power", map[string]string{"readable": "RAPL readable (measured)",
		"root-only": "RAPL root-only (estimated; cerberus rapl-setup)", "absent": "estimated"}[d.RAPL])
}

func engineLabel(name string) string {
	switch name {
	case "lmstudio":
		return "LM Studio"
	case "ollama":
		return "Ollama"
	case "llamacpp":
		return "llama.cpp"
	}
	return name
}

func printInitSummary(w io.Writer, r InitResult) {
	fmt.Fprintln(w)
	verb := "Updated"
	if r.Created {
		verb = "Created"
	}
	fmt.Fprintf(w, "%s %s\n", verb, r.ConfigPath)
	for _, p := range r.Changed {
		fmt.Fprintf(w, "  set %s\n", p)
	}
	if r.CertCmd != "" {
		fmt.Fprintln(w, "\nGet the HTTPS certificate once (and again before it expires):")
		fmt.Fprintf(w, "  %s\n", r.CertCmd)
	}
	if r.Service != "" {
		fmt.Fprintf(w, "\nService: %s\n", r.Service)
	} else {
		fmt.Fprintln(w, "\nStart it with: cerberus serve   (or cerberus install-service)")
	}
	printProblems(w, r.Problems, "")
	if r.PairURL != "" {
		fmt.Fprintln(w, "\nOpen Cerberus (this link holds the admin token; for a phone, prefer cerberus pair):")
		if code, err := qr.Encode([]byte(r.PairURL)); err == nil {
			fmt.Fprint(w, code.HalfBlocks(2, true))
		}
		fmt.Fprintf(w, "  %s\n\n", r.PairURL)
	}
	printDoctor(w, r.Doctor)
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
