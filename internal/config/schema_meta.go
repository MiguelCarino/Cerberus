package config

import "fmt"

// The hand-written words of the schema: one entry per setting, keyed by
// schema path. TestSchemaMetadataCoverage fails when a Config field has no
// entry (or an entry names no field), so new settings cannot ship
// undocumented. Scalar list items ("server.extra_listen[]") may have an
// entry of their own for a kind or format; otherwise they take their list's.

type meta struct {
	label, help, group, level string
	kind, format, unit        string
	enum, aliases             []string
	min, max                  *float64
	restart, sensitive        bool
	readOnly, required        bool
	mustExist                 bool // kind path: warn when it does not exist
}

type opt func(*meta)

var (
	basic     opt = func(m *meta) { m.level = LevelBasic }
	restart   opt = func(m *meta) { m.restart = true }
	sensitive opt = func(m *meta) { m.sensitive = true }
	readOnly  opt = func(m *meta) { m.readOnly = true }
	required  opt = func(m *meta) { m.required = true }
	isPath    opt = func(m *meta) { m.kind = KindPath }
	mustExist opt = func(m *meta) { m.kind, m.mustExist = KindPath, true }
	secretRef opt = func(m *meta) { m.kind = KindSecretRef }
	seconds   opt = func(m *meta) { m.kind, m.unit = KindDuration, "s"; m.min = f64(0) }
	address   opt = func(m *meta) { m.format = FormatAddress }
	urlFmt    opt = func(m *meta) { m.format = FormatURL }
	command   opt = func(m *meta) { m.format = FormatCommand }
	glob      opt = func(m *meta) { m.format = FormatGlob }
	nonNeg    opt = func(m *meta) { m.min = f64(0) }
)

func f64(v float64) *float64 { return &v }

func enum(v ...string) opt       { return func(m *meta) { m.enum = v } }
func aliases(v ...string) opt    { return func(m *meta) { m.aliases = v } }
func unit(u string) opt          { return func(m *meta) { m.unit = u } }
func atLeast(lo float64) opt     { return func(m *meta) { m.min = f64(lo) } }
func between(lo, hi float64) opt { return func(m *meta) { m.min, m.max = f64(lo), f64(hi) } }
func format(f string) opt        { return func(m *meta) { m.format = f } }

var metaTable = map[string]meta{}

// d adds one entry; a repeated path is a programming error.
func d(group, path, label, help string, opts ...opt) {
	if _, dup := metaTable[path]; dup {
		panic(fmt.Sprintf("config schema: duplicate metadata for %q", path))
	}
	m := meta{label: label, help: help, group: group, level: LevelAdvanced}
	for _, o := range opts {
		o(&m)
	}
	metaTable[path] = m
}

func init() {
	sandboxModes := []opt{enum("", "none", "bwrap"), aliases("off", "false", "no")}

	// ---- Server
	g := GroupServer
	d(g, "server.listen", "Listen address",
		"Where the web UI and API listen, as host:port. Keep 127.0.0.1 unless you know you need more; add phone access below.",
		basic, restart, address, required)
	d(g, "server.extra_listen", "Extra addresses",
		"More host:port addresses to listen on, such as your Tailscale IP, so a phone can reach Cerberus.",
		basic, restart)
	d(g, "server.extra_listen[]", "Address", "One extra host:port address.", address, required)
	d(g, "server.token", "Admin token",
		"The secret that pairs devices and opens the API. Never shown here; rotate it from Security.",
		sensitive, restart)
	d(g, "server.local_no_auth", "No token from this machine",
		"Let desktop programs on this computer use the /v1 gateway without the token.", restart)
	d(g, "server.public_url", "Public URL",
		"The address phones use to reach Cerberus, put into notification links, e.g. https://desk.tailnet.ts.net:7340.",
		basic, urlFmt)
	d(g, "server.tls_cert", "HTTPS certificate",
		"A PEM certificate file (for example from `tailscale cert`). With the key, the extra addresses serve HTTPS.",
		restart, mustExist)
	d(g, "server.tls_key", "HTTPS key", "The PEM private key that goes with the certificate.", restart, mustExist)
	d(g, "server.token_created", "Token created",
		"When the admin token was made or last rotated.", readOnly)
	d(g, "server.preview_ports", "Preview ports",
		"Ports for work previews, e.g. 7350-7369: each open preview (a static site or a live dev server of a session) gets its own origin on one of them, on the same hosts as the listen addresses. Empty keeps static previews under /preview/ and offers no live servers.",
		restart)
	d(g, "server.init_done", "Setup finished",
		"Set once first-run setup is done (`cerberus init` or the web welcome). While off, the web UI shows the welcome.")

	// ---- Safety (server side)
	g = GroupSafety
	d(g, "server.allow_public", "Allow public addresses",
		"Allow listening on an address reachable from the internet. Off keeps Cerberus on private networks only.", restart)
	d(g, "server.allowed_hosts", "Allowed host names",
		"Extra host names the browser may use to reach Cerberus (a guard against DNS rebinding).", restart)

	// ---- Advanced: GPU sampling
	g = GroupAdvanced
	d(g, "gpu.backend", "GPU reader",
		"How GPU memory and load are read: auto-detect, NVIDIA (nvidia-smi), AMD (sysfs) or none.",
		restart, enum("auto", "nvidia", "amd", "none"))
	d(g, "gpu.active_interval_ms", "GPU sampling while busy",
		"How often the GPU is read while a model is loaded or a panel is open.", restart, unit("ms"), atLeast(100))
	d(g, "gpu.idle_interval_ms", "GPU sampling while idle",
		"How often the GPU is read when nothing is happening. Longer saves energy.", restart, unit("ms"), atLeast(1000))
	d(g, "gpu.nvidia_smi", "nvidia-smi command", "The nvidia-smi program to run.", restart, isPath)

	// ---- Models
	g = GroupModels
	d(g, "engines.default_model", "Default model",
		"The model used when a request or session names none.", basic)
	d(g, "engines.idle_ttl_sec", "Unload after idle",
		"A loaded model is unloaded after this long without requests, freeing GPU memory. 0 keeps it loaded.",
		basic, seconds)
	d(g, "engines.load_timeout_sec", "Load timeout",
		"How long to wait for a model to load before giving up.", seconds, atLeast(1))
	d(g, "engines.allow_overcommit", "Allow over-commit",
		"Load a model even when it looks too big for free GPU memory. It may spill to system memory and run slowly.")
	d(g, "engines.stall_sec", "Stall check after",
		"A local request that receives nothing for this long gets its engine checked; an engine that does not answer twice is restarted and its requests fail with a clear error. Nothing is checked while no request is running. 0 turns it off.",
		seconds)
	d(g, "engines.wait_for_room_sec", "Wait for room",
		"When a model does not fit because another loaded model is answering, wait this long for it to finish, then swap. 0 loads at once and lets the engine spill to system memory.",
		seconds)
	d(g, "engines.llamacpp.enabled", "llama.cpp on",
		"Run local GGUF models with llama-server. Harmless when it is not installed.", basic, restart)
	d(g, "engines.llamacpp.binary", "llama-server program", "The llama-server program to start.", restart, isPath)
	d(g, "engines.llamacpp.model_dirs", "GGUF folders", "Folders scanned for .gguf model files.", restart)
	d(g, "engines.llamacpp.model_dirs[]", "Folder", "A folder holding .gguf files.", mustExist)
	d(g, "engines.llamacpp.ctx", "llama.cpp context",
		"Context length in tokens for llama.cpp models, unless a model sets its own.", restart, unit("tokens"), atLeast(512))
	d(g, "engines.llamacpp.parallel", "llama.cpp slots",
		"How many requests one model serves at once; concurrent agents share it.", restart, between(1, 64))
	d(g, "engines.llamacpp.args", "llama.cpp extra arguments", "Extra arguments given to every llama-server.", restart)
	d(g, "engines.llamacpp.port_base", "llama.cpp first port",
		"llama-server processes listen on loopback ports counting up from here.", restart, between(1024, 65000))
	d(g, "engines.lmstudio.enabled", "LM Studio on",
		"Use models from LM Studio through its lms tool. Harmless when it is not installed.", basic, restart)
	d(g, "engines.lmstudio.binary", "lms program", "The LM Studio command-line tool.", restart, isPath)
	d(g, "engines.lmstudio.base_url", "LM Studio URL", "LM Studio's OpenAI-compatible server address.", restart, urlFmt)
	d(g, "engines.lmstudio.ctx", "LM Studio context", "Context length in tokens when LM Studio loads a model.",
		restart, unit("tokens"), atLeast(512))
	d(g, "engines.lmstudio.gpu", "LM Studio GPU offload",
		"How much of a model LM Studio puts on the GPU: empty lets Cerberus pick a split when the model does not fit (else LM Studio decides), \"max\", \"off\", or a fraction such as 0.75.")
	d(g, "engines.ollama.enabled", "Ollama on",
		"Use models from a running Ollama. Harmless when it is not running.", basic, restart)
	d(g, "engines.ollama.url", "Ollama URL", "Where Ollama listens.", restart, urlFmt)

	d(g, "models", "Model entries",
		"Local models to add (llama.cpp .gguf files) or settings for discovered ones, matched by ID.", restart)
	d(g, "models[].id", "Model ID", "The name clients use for this model.", required)
	d(g, "models[].backend", "Engine", "Which engine runs it; empty means llama.cpp.",
		enum("", "llamacpp", "lmstudio", "ollama"))
	d(g, "models[].path", "GGUF file", "For llama.cpp: the .gguf file.", mustExist)
	d(g, "models[].name", "Engine name", "The engine's own name for the model, if different from the ID.")
	d(g, "models[].ctx", "Context", "Context length in tokens (0 = engine default).", unit("tokens"), nonNeg)
	d(g, "models[].parallel", "Slots", "Concurrent requests (0 = engine default).", nonNeg)
	d(g, "models[].ttl_sec", "Unload after idle", "Idle time before unloading this model (0 = engine default).", seconds)
	d(g, "models[].size_mib", "Memory estimate", "GPU memory this model needs, if the automatic estimate is wrong (0 = estimate).",
		unit("MiB"), nonNeg)
	d(g, "models[].args", "Extra arguments", "Extra llama-server arguments for this model only.")
	d(g, "models[].vision", "Vision", "The model accepts images.")
	d(g, "models[].draft", "Draft model", "For llama.cpp: a small .gguf used for speculative decoding.", mustExist)
	d(g, "models[].max_parallel", "Requests at once",
		"How many requests Cerberus sends this model at the same time; the rest wait their turn (0 = automatic: 1 for a model over half the GPU memory, else the engine's slots).",
		nonNeg)
	d(g, "models[].defaults.reasoning_effort", "Reasoning effort",
		"How hard the model thinks when a request does not say (empty = the model's own default). Agent tools usually send none, so this is what they run at.",
		enum("", "none", "minimal", "low", "medium", "high", "xhigh"))
	d(g, "models[].defaults.enable_thinking", "Thinking",
		"Turn the model's thinking on or off when a request does not say (unset = the model's own default).")
	d(g, "models[].defaults.max_tokens", "Default token budget",
		"Tokens a reply may use, thinking included, when a request sends no budget (0 = the engine's default).",
		unit("tokens"), nonNeg)
	d(g, "models[].defaults.max_tokens_cap", "Largest token budget",
		"A request asking for more (or for -1, the most allowed) is clipped to this (0 = no cap).", unit("tokens"), nonNeg)
	d(g, "models[].defaults.temperature", "Temperature", "Sampling temperature when a request does not set one.", nonNeg)
	d(g, "models[].defaults.top_p", "Top P", "Nucleus sampling when a request does not set it.", nonNeg)
	d(g, "models[].defaults.top_k", "Top K", "Top-K sampling when a request does not set it (0 = no default).", nonNeg)

	// ---- Gateway & spend
	g = GroupGateway
	d(g, "providers", "API providers",
		"Cloud or remote OpenAI-compatible APIs reached through the gateway with your API keys. Keys live in the vault.", restart)
	d(g, "providers[].name", "Name", "Short id, also the model prefix (e.g. openrouter/...).", required)
	d(g, "providers[].base_url", "Base URL", "The provider's OpenAI-compatible root, usually ending in /v1.", required, urlFmt)
	d(g, "providers[].key_env", "Key variable", "An environment variable holding the key, instead of the vault.")
	d(g, "providers[].models", "Models", "Models offered; empty asks the provider's model list.")
	d(g, "providers[].headers", "Extra headers", "Extra HTTP headers sent with every request.")
	d(g, "providers[].enabled", "On", "Offer this provider's models.")
	d(g, "providers[].key_secret", "Key secret", "The vault secret holding the key (default: the provider name).", secretRef)
	d(g, "providers[].apis", "Native APIs",
		"Other APIs this provider serves itself, passed through as they are: \"messages\" (Anthropic) or \"responses\" (OpenAI). Others are translated to chat completions.")
	d(g, "providers[].apis[]", "API", "An API the provider serves natively.", enum("messages", "responses"))
	d(g, "gateway.records.enabled", "Request records",
		"Keep one line per gateway request: which client sent it, its size and settings, the tools it called and how it ended. For seeing what your agents actually send.", restart)
	d(g, "gateway.records.text", "Text in records",
		"How much of the conversation the records keep: none (counts only), user (user and assistant text) or all (also system prompts and tool results). The text is personal data; known secrets are masked.",
		enum("", "none", "user", "all"), restart)
	d(g, "gateway.records.dir", "Records folder", "Where the records go (empty = records/ in the data folder).", isPath, restart)
	d(g, "gateway.records.retain_days", "Keep records for", "Days of records kept (0 = 14).", unit("days"), nonNeg, restart)
	d(g, "gateway.records.max_mib", "Records size cap", "The oldest days go once the records pass this size (0 = 1024).", unit("MiB"), nonNeg, restart)
	d(g, "gateway.guard.mode", "Agent guard",
		"A small local model reads each tool result an agent sends back, looking for text aimed at the agent (prompt injection). watch reports what it finds; hold also stops a tool call that acts outside the conversation in a turn with a flagged result, and says why. It is a second pair of eyes, not a security boundary.",
		enum("", "off", "watch", "hold"))
	d(g, "gateway.guard.model", "Guard model",
		"The local model that reads tool results: a classifier that answers in Qwen3Guard-Gen's format (Safety: ... Categories: ...), such as Qwen3Guard-Gen-0.6B.")
	d(g, "gateway.guard.threshold", "Guard threshold", "Score from 0 to 1 at which a tool result is flagged (0 = 0.5).", nonNeg)
	d(g, "gateway.guard.wait_ms", "Guard wait",
		"Longest a tool call waits for the guard's verdicts; after that it goes out unscreened and is logged (0 = 10000).", unit("ms"), nonNeg)
	d(g, "gateway.guard.window_tokens", "Guard window",
		"Long tool results are read in overlapping windows of about this many tokens and scored by the worst (0 = 512).", unit("tokens"), nonNeg)
	d(g, "gateway.guard.side_effect_tools", "Tools that act",
		"Extra tool name patterns (such as deploy_* or mcp__*__send*) that act outside the conversation. Built-in patterns cover shells, writes, edits, sends and deletes; a tool that matches no pattern is treated as acting.")
	d(g, "gateway.guard.side_effect_tools[]", "Pattern", "A tool name pattern (* matches anything).", glob)
	d(g, "gateway.guard.read_only_tools", "Tools that only read",
		"Extra tool name patterns that only read and are never held.")
	d(g, "gateway.guard.read_only_tools[]", "Pattern", "A tool name pattern (* matches anything).", glob)
	d(g, "gateway.model_map", "Model name map",
		"Model names that clients hard-code (a pattern such as claude-* or gpt-*) and the Cerberus model to use when such a name matches no model, e.g. claude-* → default.")

	d(g, "prices", "Prices",
		"USD per million tokens, by model ID or provider/* for a whole provider. Used to count spend.")
	d(g, "prices.*.in", "Input", "USD per million input tokens.", unit("USD/Mtok"), nonNeg)
	d(g, "prices.*.out", "Output", "USD per million output tokens.", unit("USD/Mtok"), nonNeg)
	d(g, "prices.*.cached_in", "Cached input", "USD per million cached input tokens.", unit("USD/Mtok"), nonNeg)

	d(g, "budgets", "Budgets",
		"Spend caps per provider (* for all) and period. Past the soft cap you are warned; past the hard cap requests are refused.",
		basic)
	d(g, "budgets[].provider", "Provider", "A provider name, or * for all cloud providers.", required)
	d(g, "budgets[].period", "Period", "The budget resets every day or every month.", enum("day", "month"))
	d(g, "budgets[].soft_usd", "Warn at", "Warn when spend reaches this.", unit("USD"), nonNeg)
	d(g, "budgets[].hard_usd", "Stop at", "Refuse requests past this (0 = no hard cap).", unit("USD"), nonNeg)

	d(g, "subscriptions", "Subscriptions",
		"Consumer plans used through their own official tools. Cerberus only watches usage; limits are your estimates.")
	d(g, "subscriptions[].name", "Name", "A name for the plan.", required)
	d(g, "subscriptions[].kind", "Kind", "How usage is read.", enum("claude-code"))
	d(g, "subscriptions[].agents", "Agents", "Agent profiles that use this plan.")
	d(g, "subscriptions[].data_dir", "Usage folder", "Where the tool keeps its local usage records.", mustExist)
	d(g, "subscriptions[].window_5h_limit", "5-hour limit", "Your estimate of tokens per rolling 5 hours (0 = unknown).",
		unit("tokens"), nonNeg)
	d(g, "subscriptions[].weekly_limit", "Weekly limit", "Your estimate of tokens per rolling 7 days (0 = unknown).",
		unit("tokens"), nonNeg)
	d(g, "subscriptions[].all_sessions", "Count all sessions",
		"Count every session of the tool on this machine, not only those Cerberus started.")

	// ---- Agents
	g = GroupAgents
	d(g, "agents", "Agent profiles",
		"How each coding agent is started. Entries replace built-in profiles of the same name; new names are added.")
	d(g, "agents[].name", "Name", "The profile name shown when starting a session.", required)
	d(g, "agents[].command", "Command",
		"The program and arguments. Placeholders: {task} {task_file} {model} {base_url} {cwd} {session} {self}.",
		required, command)
	d(g, "agents[].env", "Environment", "Extra environment variables for its sessions.")
	d(g, "agents[].needs_model", "Needs a model", "Show the model picker for this agent.")
	d(g, "agents[].interactive", "Interactive", "The agent stays open after its task (waiting for you) instead of exiting.")
	d(g, "agents[].description", "Description", "One line shown in the agent picker.")
	d(g, "agents[].resume_command", "Resume command",
		"How to continue a finished session in place; adds the {resume_file} placeholder.", command)
	d(g, "agents[].secret_env", "Secret environment", "Environment variable to vault secret name, injected into its sessions.")
	d(g, "agents[].secret_env.*", "Secret", "A vault secret name.", secretRef)
	d(g, "agents[].sandbox", "Sandbox", "Run its sessions in a sandbox; empty uses the safety default.", sandboxModes...)
	d(g, "agents[].sandbox_rw", "Writable folders", "Extra folders it may write inside the sandbox (e.g. ~/.claude).")
	d(g, "default_agent", "Default agent", "The agent preselected for new sessions.", basic, required)

	d(g, "agent.approve", "Approve mode",
		"For the built-in agent: ask before running commands and editing files, or do it automatically.",
		basic, enum("ask", "auto"))
	d(g, "agent.max_steps", "Max steps", "The built-in agent stops after this many tool steps.", between(1, 10000))
	d(g, "agent.command_timeout_s", "Command timeout", "The longest a command run by the built-in agent may take.",
		seconds, atLeast(1))
	d(g, "agent.spawn.enabled", "Agents may start sessions", "Let agents start helper sessions of their own.")
	d(g, "agent.spawn.max_depth", "Max depth", "How many levels below your own session helpers may go.", between(0, 10))
	d(g, "agent.spawn.max_children", "Max helpers per session", "Open helper sessions one session may have.", nonNeg)
	d(g, "agent.spawn.max_total", "Max helpers per tree", "Open helper sessions under one session of yours, all levels.", nonNeg)
	d(g, "agent.spawn.require_approval", "Approve helpers",
		"Helper sessions wait in the queue until you approve them.")

	// ---- Safety (agent side)
	g = GroupSafety
	d(g, "agent.deny_paths", "Denied paths",
		"Files and folders the built-in agent may never read or write (glob patterns).")
	d(g, "agent.deny_paths[]", "Pattern", "A glob such as **/.ssh/**.", glob)
	d(g, "agent.policy.allow", "Always allowed commands",
		"Command prefixes the built-in agent runs without asking, even in ask mode.")
	d(g, "agent.policy.deny", "Forbidden commands", "Command prefixes never run, not even when asked.")
	d(g, "agent.policy.ask_always", "Always ask", "Command prefixes that always ask first, even in auto mode.")

	d(g, "sandbox.default", "Sandbox by default",
		"Run agent sessions inside a bubblewrap sandbox unless a profile or repo says otherwise.",
		basic, enum("none", "bwrap"), aliases("off", "false", "no", ""))
	d(g, "sandbox.strict", "Strict sandbox", "Refuse to start a session when the sandbox is wanted but bwrap is missing.")
	d(g, "sandbox.network", "Network in sandbox", "Keep network access inside the sandbox; agents need it to reach the gateway.")
	d(g, "sandbox.read_only_home", "Read-only home", "Your home folder is read-only in the sandbox, except the workspace.")
	d(g, "sandbox.extra_ro", "Extra read-only folders", "More folders visible read-only in the sandbox.")
	d(g, "sandbox.extra_rw", "Extra writable folders", "More folders writable in the sandbox.")
	d(g, "sandbox.hide_home", "Hidden folders", "Folders replaced with an empty one in the sandbox (keys, browsers, Cerberus' own config).")

	d(g, "users.session_idle_hours", "Sign-in idle limit",
		"A browser sign-in that is not used for this long ends.", unit("hours"), between(1, 8760))
	d(g, "users.session_max_days", "Sign-in lifetime", "A browser sign-in ends this long after it started.", unit("days"), between(1, 365))
	d(g, "users.lockout_failures", "Lock after failures",
		"Wrong passwords for one user name before the name is locked.", between(1, 100))
	d(g, "users.lockout_minutes", "Lockout window", "Failures count within this window, and a locked name waits this long.",
		unit("minutes"), between(1, 1440))

	d(g, "vault.backend", "Secret store",
		"Where secrets live: auto (the desktop keyring when available, else an encrypted file), keyring only, or file only.",
		restart, enum("auto", "secret-service", "file"))

	// ---- Advanced: context for the built-in agent
	g = GroupAdvanced
	d(g, "context.files", "Context files", "Extra files given to the built-in agent as background, such as a notes index.")
	d(g, "context.files[]", "File", "A file to include.", mustExist)
	d(g, "context.max_bytes_per_file", "Context size per file", "Longer context files are cut to this.",
		unit("bytes"), atLeast(0))

	// ---- Sessions
	g = GroupSessions
	d(g, "repos", "Repositories", "Git repositories offered when starting a session, with their checks.")
	d(g, "repos[].name", "Name", "Short name shown in the repo picker.", required)
	d(g, "repos[].path", "Folder", "The repository folder.", required, mustExist)
	d(g, "repos[].checks", "Checks", "Commands run in the worktree before you accept (tests, lint).")
	d(g, "repos[].preview.dir", "Preview folder", "A folder of the worktree served as a web preview; . is the root.")
	d(g, "repos[].preview.command", "Preview command",
		"Starts a preview server for Run preview, e.g. npm run dev -- --port {port} --host 127.0.0.1. Empty: detected from package.json.")
	d(g, "repos[].push", "Push on accept", "Push by default when you accept a session's work.")
	d(g, "repos[].secret_env", "Secret environment", "Environment variable to vault secret name, for sessions on this repo.")
	d(g, "repos[].secret_env.*", "Secret", "A vault secret name.", secretRef)
	d(g, "repos[].sandbox", "Sandbox", "Overrides the agent's sandbox for this repo.", sandboxModes...)
	d(g, "repo_roots", "Repository folders", "Folders whose git repositories are offered automatically.")
	d(g, "repo_roots[]", "Folder", "A folder holding git repositories.", mustExist)

	d(g, "sessions.max_attempts", "Attempts", "How many times a failing session is retried with the check output.", between(1, 20))
	d(g, "sessions.checks_timeout_sec", "Checks timeout", "The longest the checks may run.", seconds, atLeast(1))
	d(g, "sessions.idle_wait_sec", "Idle before waiting", "Quiet time before an interactive session counts as waiting for you.",
		seconds, atLeast(1))
	d(g, "sessions.scrollback_mib", "Scrollback kept", "Terminal output kept per session.", unit("MiB"), atLeast(1))
	d(g, "sessions.scrollback_tail_kib", "Scrollback sent", "Terminal output sent when you open a session.", unit("KiB"), atLeast(1))
	d(g, "sessions.default_cols", "Terminal columns", "Terminal width before a screen attaches.", between(20, 1000))
	d(g, "sessions.default_rows", "Terminal rows", "Terminal height before a screen attaches.", between(5, 500))
	d(g, "sessions.retention_days", "Keep finished sessions", "Finished sessions older than this are removed (0 keeps all). Unaccepted work is kept.",
		unit("days"), nonNeg)
	d(g, "sessions.preview_idle_min", "Preview idle stop",
		"A preview server started with Run preview, or a preview port, that nobody looked at for this long is stopped.",
		unit("min"), atLeast(1))
	d(g, "sessions.record_max_mib", "Recording per run",
		"Each run is also recorded for replay (output only, secrets masked) up to this size; 0 turns recording off.",
		unit("MiB"), between(0, 1024))

	d(g, "guard.max_sessions", "Max running sessions", "Sessions beyond this wait in the queue.", between(1, 100))
	d(g, "guard.min_free_mib", "Keep memory free", "New sessions wait while free memory is below this.", unit("MiB"), nonNeg)
	d(g, "guard.systemd_slice", "systemd slice", "The slice sessions run in when systemd-run is available.")
	d(g, "guard.session_memory_max", "Memory per session", "The most memory one session may use (systemd size, e.g. 6G).",
		format(FormatSize))

	d(g, "git.name", "Commit name", "The name on commits made when you accept; empty uses the repo's git config.")
	d(g, "git.email", "Commit email", "The email on commits made when you accept; empty uses the repo's git config.",
		format(FormatEmail))

	// ---- Notifications
	g = GroupNotifications
	d(g, "notify.command", "Desktop command",
		"A program run for each notification, with {title} {body} {urgency} placeholders.", command)
	d(g, "notify.ntfy_url", "ntfy topic",
		"An ntfy topic URL (e.g. https://ntfy.sh/your-secret-topic) to get notifications on your phone.", basic, urlFmt)
	d(g, "notify.disabled", "Notifications off",
		"Send no desktop or phone notifications. The in-app inbox still lists them.")

	// ---- Voice
	g = GroupVoice
	d(g, "voice.enabled", "Voice input", "Turn recorded speech into task text, on this machine.", restart)
	d(g, "voice.backend", "Speech engine",
		"auto picks the first available; command runs your program; openai sends audio to the URL below.",
		restart, enum("auto", "command", "openai", "off"))
	d(g, "voice.command", "Speech command", "Program and arguments; {wav} is the recording. It prints the text.", restart, command)
	d(g, "voice.url", "Speech URL", "An OpenAI-compatible base URL with /audio/transcriptions.", restart, urlFmt)
	d(g, "voice.model", "Speech model", "The transcription model for the URL engine, e.g. whisper-1.", restart)
	d(g, "voice.language", "Language", "Spoken language code such as es or en; empty detects it.", restart)
	d(g, "voice.ffmpeg", "ffmpeg program", "Used to convert recordings.", restart, isPath)
	d(g, "voice.max_seconds", "Longest recording", "Recordings are cut at this length.", restart, seconds, atLeast(1))

	// ---- Monitor & energy
	g = GroupMonitor
	d(g, "monitor.enabled", "Monitor on", "Sample CPU, memory, disks and power for the monitor and the energy history.", restart)
	d(g, "monitor.active_interval_ms", "Sampling while watched", "How often to sample while a monitor panel is open.",
		restart, unit("ms"), atLeast(250))
	d(g, "monitor.idle_interval_sec", "Background sampling", "How often to sample for the energy history when nobody watches.",
		restart, seconds, atLeast(5))
	d(g, "monitor.cpu_power", "CPU power source",
		"auto reads the CPU's power meter (RAPL) when allowed, else estimates from load with the watts below.",
		restart, enum("auto", "rapl", "model"))
	d(g, "monitor.cpu_idle_w", "CPU idle watts", "Estimated CPU package power at idle.", unit("W"), nonNeg)
	d(g, "monitor.cpu_max_w", "CPU full-load watts", "Estimated CPU package power at full load.", unit("W"), nonNeg)
	d(g, "monitor.baseline_w", "Rest of the PC", "Board, memory, drives and fans that no sensor measures.", unit("W"), nonNeg)
	d(g, "monitor.psu_efficiency", "Power supply efficiency", "Share of wall power that reaches the parts (0.90 = 90%).",
		between(0.5, 1))
	d(g, "monitor.extra_w", "Always-on extras", "Devices you want counted too, such as a screen or router.", unit("W"), nonNeg)
	d(g, "monitor.history_months", "History kept", "Months of energy history kept.", restart, unit("months"), between(1, 120))
	d(g, "monitor.alerts.disk_pct", "Disk full alert", "Alert when a disk is this full.", unit("%"), between(1, 100))
	d(g, "monitor.alerts.cpu_temp_c", "CPU temperature alert", "Alert above this CPU temperature.", unit("°C"), between(30, 120))
	d(g, "monitor.alerts.gpu_temp_c", "GPU temperature alert", "Alert above this GPU temperature.", unit("°C"), between(30, 120))
	d(g, "monitor.alerts.nvme_temp_c", "NVMe temperature alert", "Alert above this SSD temperature.", unit("°C"), between(30, 120))
	d(g, "monitor.alerts.high_power_w", "High power alert", "Alert when wall power stays above this…", unit("W"), nonNeg)
	d(g, "monitor.alerts.high_power_min", "…for this long", "…for this many minutes.", unit("min"), atLeast(1))
	d(g, "monitor.alerts.month_bill_mxn", "Monthly cost alert",
		"Alert when this PC's projected extra cost this month passes this (0 = off).", unit("MXN"), nonNeg)
	d(g, "monitor.alerts.dac_warn", "High-use tariff warning",
		"Warn when the PC pushes the household toward the high-use (DAC) tariff.")
	d(g, "monitor.protected", "Protected processes", "Process names the monitor never offers to stop or slow down.")

	d(g, "tariff.name", "Tariff", "The electricity tariff's name.")
	d(g, "tariff.currency", "Currency", "Currency of the prices below.")
	d(g, "tariff.tiers", "Tiers",
		"Monthly price steps, ascending. Each tier ends at its kWh; the last one (0) covers the rest.", basic)
	d(g, "tariff.tiers[].name", "Name", "The tier's name on your bill.")
	d(g, "tariff.tiers[].up_to_kwh", "Up to", "Monthly kWh where this tier ends (0 = the rest).", unit("kWh"), nonNeg)
	d(g, "tariff.tiers[].per_kwh", "Price", "Price per kWh in this tier, before tax.", unit("per kWh"), nonNeg)
	d(g, "tariff.vat_pct", "Tax", "Value-added tax on the bill.", unit("%"), between(0, 100))
	d(g, "tariff.fixed_monthly", "Fixed charge", "Fixed charges per month.", nonNeg)
	d(g, "tariff.household_kwh_month", "Household use",
		"Your whole home's usual kWh per month, including this PC (half a bimonthly bill). Lets Cerberus price the PC's share.",
		basic, unit("kWh"), nonNeg)
	d(g, "tariff.dac_limit_kwh_month", "High-use limit", "The 12-month average that moves a home to the high-use (DAC) tariff.",
		unit("kWh"), nonNeg)
	d(g, "tariff.dac_per_kwh", "High-use price", "Price per kWh on the high-use tariff.", unit("per kWh"), nonNeg)
	d(g, "tariff.dac_fixed_monthly", "High-use fixed charge", "Fixed charge per month on the high-use tariff.", nonNeg)
	d(g, "tariff.verified", "Checked against my bill",
		"Turn on once you have checked these numbers against a real bill; until then costs say approximate.", basic)

	// ---- Files
	g = GroupFiles
	d(g, "files.upload_dir", "Upload folder", "The folder inside each workspace that receives uploaded files.", required)
	d(g, "files.max_file_mib", "Largest file", "The biggest single upload.", unit("MiB"), atLeast(1))
	d(g, "files.max_total_mib", "Uploads per session", "The most uploaded data per session.", unit("MiB"), atLeast(1))
	d(g, "files.allow_exec", "Keep executable bit", "Keep uploaded files executable. Off never marks uploads executable.")
	d(g, "files.deny_ext", "Refused extensions", "File extensions refused on upload, such as .exe.")
	d(g, "files.keep_names", "Keep file names", "Keep the original (cleaned) names of uploads.")

	// ---- Chat (day phase 5), shown with the agents
	g = GroupAgents
	d(g, "chat.enabled", "LLM chat", "Show the Chat view: talk to any model, with tools that run on this computer.", basic)
	d(g, "chat.quick_model", "Quick answer model", "The model for quick answers. Empty uses the default model.")
	d(g, "chat.default_mode", "Chat permissions",
		"What a new chat may do without asking: ask before every change, auto (read-only commands go), accept edits, or plan (read only).",
		basic, enum(ChatModes...), aliases("acceptedits", "accept-edits", "default"))
	d(g, "chat.max_steps", "Chat tool steps", "Tool rounds per message before the model must answer.", between(1, 200))
	d(g, "chat.result_clip", "Chat tool result size", "Characters of one tool result the model gets.",
		unit("chars"), between(500, 1000000))
	d(g, "chat.command_timeout_s", "Chat command timeout",
		"How long a chat command may run unless the model asks for more (at most 600 s).", seconds, between(1, 600))
	d(g, "chat.atrium_import", "Atrium chats", "List the Atrium desktop chat's sessions so they can be continued here.")

	// ---- Delegation to local models, shown with the agents
	d(g, "delegate.enabled", "Delegate to local models",
		"Let agents list local models and send them small jobs (cerberus_models, cerberus_ask), so a cloud model can hand off bulk work.", basic)
	d(g, "delegate.models", "Models for delegation", "Local models agents may use. Empty allows every local model.")
	d(g, "delegate.tags", "Model tags", "Short tags per model that agents see, such as code, fast or vision.")
	d(g, "delegate.tags.*", "Tags", "Tags for this model.")
	d(g, "delegate.tags.*[]", "Tag", "One tag, such as code or fast.")
	d(g, "delegate.load", "Load on demand",
		"Let a delegated job load a model that is not in memory. Off answers \"not loaded\" at once, so the agent does the job itself.")
	d(g, "delegate.max_queue", "Busy after", "Local requests already running before a delegated job is told \"busy\".", between(1, 64))
	d(g, "delegate.max_tokens", "Longest answer", "The most tokens one delegated job may answer with.", unit("tokens"), between(16, 32768))
	d(g, "delegate.max_file_kib", "File size per job", "How much of one workspace file is sent with a delegated job.", unit("KiB"), between(1, 4096))
	d(g, "delegate.guidance", "Guidance for agents", "What agents read about which jobs to send to local models.")
	d(g, "delegate.reference_model", "Compare with",
		"A cloud model whose price is used to show what delegated jobs saved. Empty shows tokens and energy only.")

	// ---- Fine-tuning examples
	g = GroupAdvanced
	d(g, "finetune.capture", "Collect training examples",
		"Keep examples for fine-tuning a local model: off, delegated jobs and their ratings, or also every cloud chat answer through the gateway. Examples contain prompts and answers.",
		enum(CaptureOff, CaptureDelegated, CaptureCloud))
	d(g, "finetune.dir", "Examples folder", "Where examples are kept. Empty uses the data folder.", isPath)
	d(g, "finetune.max_row_kib", "Largest example", "Bigger examples are skipped.", unit("KiB"), between(4, 65536))
	d(g, "finetune.train_command", "Train command",
		"The trainer `cerberus finetune train` runs, as a command line. {dataset}, {base_model} and {out} are filled in.", command)
	d(g, "finetune.train_command[]", "Argument", "One part of the command line.")
	d(g, "finetune.base_model", "Base model", "The model to train from, passed to the trainer as {base_model}.")

	// ---- Flows (designed, scheduled and triggered tasks)
	d(g, "flows.enabled", "Flows",
		"Let flows run, by hand or on their schedule. Off refuses new runs and fires no schedules; runs already going finish.")
	d(g, "flows.max_runs", "Flow runs at once", "How many flow runs execute at the same time; more wait in a queue.", between(1, 16))
}
