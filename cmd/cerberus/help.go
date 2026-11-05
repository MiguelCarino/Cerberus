package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// cmdHelp is the -h text of one subcommand: its synopsis (after
// "cerberus ") and a few plain lines on what it does. The flags follow,
// printed by the flag package.
type cmdHelp struct {
	usage string
	about string
}

// helps holds the -h text for every subcommand, keyed by flag set name.
// printUsage is the one-screen overview; keep the two in step.
var helps = map[string]cmdHelp{
	"serve": {"serve [-config PATH] [-debug]", `Run the daemon: GPU monitor, model manager, OpenAI-compatible gateway (/v1),
agent sessions, chats and the web UI. It listens on server.listen
(127.0.0.1:7340 by default) plus server.extra_listen. Without a config file
the first run writes the defaults with a fresh token. SIGTERM shuts down in
order: HTTP, sessions, spawned model servers. SIGHUP reloads the TLS
certificate.`},
	"version": {"version", `Print the version, the platform and the Go version it was built with.
Also: cerberus --version, cerberus -v.`},
	"status": {"status [-config PATH]", `One-screen summary of the running daemon: GPU, models, sessions, the guard.`},
	"task": {`task [flags] "task"`, `Start an agent session and print its UI link. Flags come before the task.
Inside a git repo with no workspace flag it is a repo session for that repo
(a detached worktree behind the test gate); outside one it is a scratch
session in an empty folder. --dir picks a folder: a git repo top level gets
a worktree, any other folder is edited in place.`},
	"url": {"url [-config PATH]", `Print sign-in links with ?token= for public_url and every listen address.
A wildcard bind expands to loopback plus every LAN and Tailscale address.
Opening a link signs the browser in. Use "cerberus pair" to give a phone a
token of its own.`},
	"secret": {"secret list | set NAME [--note TEXT] | rm NAME", `Manage the vault. Values are write-only: nothing prints them. set reads the
value from stdin (cerberus secret set OPENAI < keyfile), or asks for it with
echo off on a terminal. With the daemon running the change goes through it.`},
	"secret list": {"secret list [-config PATH]", `List vault secrets: names, notes and lengths, never values.`},
	"secret set": {"secret set NAME [--note TEXT] [-config PATH]", `Store a secret. The value is read from stdin, so it stays out of shell
history and argv: cerberus secret set OPENAI < keyfile. On a terminal it is
asked for with echo off.`},
	"secret rm": {"secret rm NAME [-config PATH]", `Remove a secret from the vault.`},
	"search":    {`search [flags] "words"`, `Search session titles, tasks and transcripts. Every word must match.`},
	"resume": {`resume [-config PATH] ID ["what to do next"]`, `Start a new attempt of a finished session (passed, failed, stopped or
error) in its workspace. With no text the agent is told to continue where it
left off.`},
	"fleet": {`fleet --has FILE | --name GLOB [flags] "task"`, `Start one session per matching repo under repo_roots. {name} and {repo} in
the task expand per repo. Lists the repos and asks before starting unless
--yes.`},
	"pull": {"pull [flags] REF\n       cerberus pull --cancel ID", `Download a local model through the daemon. It never loads the model.
REF is an Ollama name (qwen3:8b or ollama:qwen3:8b), an LM Studio name
(lms:qwen3-8b, fetched with "lms get NAME --yes") or a Hugging Face https
URL of a .gguf file (saved into the first llama.cpp models folder). A bare
name goes to LM Studio when lms is installed, else to Ollama. Progress is
followed until the download ends; Ctrl-C stops following, not the download.`},
	"prune": {"prune [-config PATH]", `Delete finished sessions older than sessions.retention_days now. It also
runs daily. Repo worktrees with unaccepted changes are kept.`},
	"top": {"top [-config PATH] [--once] [--every 2s]", `Live terminal view: CPU, memory, GPU, power, cost, drives, top processes
and sessions. q quits. While it is open the monitor samples actively.`},
	"bench": {"bench [--model ID] [--repeat N] [--kinds K,K] [--prefill] [--label TEXT] [--json] [--force]", `Time a model through the gateway on Cerberus's fixed prompt set (code,
edit, proof, procedure, chat, prose; with --prefill also 8k, 32k and ctx/2
prompts) at temperature 0 with thinking off: time to first token, decode and
prefill speed and energy per request (medians of --repeat runs). It refuses
while agent sessions are running (--force runs anyway). Runs are saved in the
data folder's bench/; "bench list" lists them and "bench compare A B" lines
two up, kind by kind. The model is loaded on the GPU like any request.`},
	"bench list": {"bench list", `List saved bench runs, newest first.`},
	"bench compare": {"bench compare A B", `Line two bench runs up kind by kind (run ids or .json files): decode, time
to first token and tokens per joule, B against A, with a plain summary.`},
	"energy": {"energy [-config PATH]", `Energy totals (today, yesterday, 7 days, month, 30 days) and the
electricity bill estimate at the household's marginal tariff tier.`},
	"rapl-setup": {"rapl-setup", `Print (never run) the one-time root steps that let Cerberus read the CPU
energy counter (RAPL), explain the trade-off, then check whether it is
readable now. Without it CPU watts are estimated from load.`},
	"pair": {"pair [flags]", `Pair a phone or laptop: create a device token and print its sign-in link
and a QR code, once. Needs the admin token. Revoke it with "cerberus devices
rm".

With --mcp, pair an MCP client instead: an outside agent such as Claude Code
that runs "cerberus mcp --client NAME". Its token (kept in the config folder
under mcp-clients/, mode 0600) opens only the agent scope: delegate to local
models and start and steer the sessions it started.`},
	"devices": {"devices [-config PATH] [list | rm ID|NAME]", `List paired devices, or revoke one. A revoked device's live streams end at
once.`},
	"token": {"token [-config PATH] rotate", `Replace the admin token and save the config. Browsers signed in with the old
token must sign in again; paired devices keep working; running agents keep
/v1 until their session ends.`},
	"audit": {"audit [flags]", `Read the audit log (DATA/audit.jsonl): state-changing calls, auth failures,
rotations, pairing, sandbox fallbacks and process signals. --actor agent
matches every agent; agent:ID one session. --user NAME keeps one user's
records; --user - the records with no user (admin token, system, CLI).`},
	"user": {"user add|list|passwd|role|disable|enable|rm|budget|share [args] [flags]", `Manage user accounts (DATA/users.json). With the daemon running the
changes go through it (audited as "cli"); otherwise the file is edited.

  user add NAME --role ROLE [--display TEXT] [--password-stdin | --generate]
      Roles: admin, user, viewer, monitor, chat. The first account must be an
      admin. Without a flag on a terminal it asks twice with no echo.
      --generate prints a 16-character password once; it must be changed at
      the first sign-in.
  user list [--json]
  user passwd NAME [--password-stdin | --generate]
      Reset a forgotten password. It must be changed at the next sign-in, and
      the user's browsers, devices and MCP clients are signed out.
  user role NAME ROLE
  user disable NAME | user enable NAME
  user rm NAME [--to ADMIN] [--yes]
      The user's sessions, chats and flows move to ADMIN (default: the first
      other enabled admin). Nothing is deleted.
  user budget NAME [--usd N] [--tokens N]     monthly caps; 0 = none
  user share NAME REPO [--accept] | user share NAME --unshare REPO
      --accept lets the user fast-forward the repo's main.

The last enabled admin can never be removed, disabled or demoted (exit 2).`},
	"finetune": {"finetune stats | export | train", `Training examples for a local model. Turn collecting on with
finetune.capture: "delegated" keeps cerberus_ask jobs and the ratings agents
give them; "cloud" also keeps cloud chat answers that went through the
gateway. Examples are private files in DATA/finetune with vault values
masked. Cerberus ships no trainer: train runs finetune.train_command.`},
	"finetune stats": {"finetune stats [-config PATH]", `How many examples are collected, rated and ready to use.`},
	"finetune export": {"finetune export [flags]", `Write the examples as chat fine-tuning JSONL ({"messages": [...]} per
line): cloud answers as they are, delegated answers rated good, and the
better answer where an agent gave one. Answers rated bad are left out.`},
	"finetune train": {"finetune train [flags]", `Export the examples into a run folder, then run finetune.train_command
with {dataset}, {base_model} and {out} filled in. --dry-run prints the
command without running it.`},
	"flow": {"flow list | show ID | run ID [--input TEXT] [--wait] | runs [ID] | cancel RUN | approve RUN [--reject]", `Flows are saved recipes: a trigger (manual or a schedule) and steps that
hand work to models (local or cloud, through the gateway) and agent sessions,
with approvals and outputs. Design them in the web UI (Flows); these commands
list, run and steer them through the running daemon.

Commands:
  list               every flow: trigger in words, next scheduled run, last run
  show ID            one flow: trigger, input hint, steps and validation problems
  run ID             start a run; --input gives {input}, --wait follows it
  runs [ID]          recent runs, newest first, of one flow or of all
  cancel RUN         stop a queued, running or waiting run
  approve RUN        decide a run waiting at an approve step (--reject ends it)

Run "cerberus flow <command> -h" for its flags.`},
	"flow list": {"flow list [-config PATH]", `List flows: id, schedule on/off, trigger in words, next scheduled run and
the last run's state. A scheduled run that was skipped (guard busy, the
previous run still going) says why.`},
	"flow show": {"flow show ID [-config PATH]", `Show one flow: name, version, trigger, input hint, each step with its model
or agent, and validation problems as checked now (a model no longer installed
is a warning).`},
	"flow run": {`flow run ID [--input "text"] [--wait] [-config PATH]`, `Start a run of a saved flow (it need not be enabled) and print its link.
--wait follows it on the event stream until it ends or waits for an approval,
then prints the result; the exit code is 0 for passed or waiting, 1 for any
other end. Ctrl-C stops following, not the run.`},
	"flow runs": {"flow runs [ID] [--limit N] [-config PATH]", `Recent runs, newest first: id, state, trigger (manual, schedule or test),
start time, flow, and the reason or note when there is one.`},
	"flow cancel": {"flow cancel RUN [-config PATH]", `Stop a run. A queued or waiting run ends at once; a running one as soon as
its step notices (agent sessions it started are stopped).`},
	"flow approve": {`flow approve RUN [--reject] [--note "text"] [-config PATH]`, `Decide a run waiting at an approve step: approve lets it continue, --reject
ends it as rejected. The decision and the note are in the audit log.`},
	"doctor": {"doctor [-config PATH] [--json]", `The safety and health checklist: each check is ok, warn or fail, with a fix.
Exits 1 when a check fails. It never creates a config.`},
	"init": {"init [--yes] [--json] [-config PATH]", `First-run setup. Detects engines and models, the GPU, agents, Tailscale,
the notify script, voice, bwrap and RAPL; asks at most 6 questions (default
model, phone access, HTTPS certificate, ntfy topic, household kWh, install
the service); writes the config; prints a summary, the sign-in link with a
QR code and the doctor. --yes keeps phone access off. It prints the
"tailscale cert" command and never runs it.`},
	"config": {"config <command> [args]", `Read and change settings by path, such as engines.idle_ttl_sec,
agents[0].command or prices["qwen2.5"].in. With the daemon running, changes
go through it and apply live where they can; otherwise the file is written.
The token is never read or written by path.

Commands:
  path               where config.json lives
  get [PATH]         one setting, or the whole config without the token (--json)
  set PATH VALUE     change a setting; VALUE is JSON or plain text (10m, yes)
  reset PATH         put a setting back to its default
  edit               edit in $EDITOR, validate, then re-open or keep the old file
  validate           check the config (--json); exit 1 on an error
  schema [WORD]      every setting with its help (--json, --basic, or a search word)
  restart            restart the daemon to apply settings that need it

Run "cerberus config <command> -h" for its flags.`},
	"config path":     {"config path [-config PATH]", `Print where config.json lives.`},
	"config get":      {"config get [PATH] [--json] [-config PATH]", `Print one setting, or the whole config without the token.`},
	"config set":      {"config set PATH VALUE [-config PATH]", `Change a setting. VALUE is JSON (lists and objects) or plain text (10m, yes). It is validated first and saved all or nothing. A running daemon applies it live when it can, or marks it "needs restart".`},
	"config reset":    {"config reset PATH [-config PATH]", `Put a setting back to its default.`},
	"config edit":     {"config edit [-config PATH]", `Open config.json in $EDITOR, then validate. On an error you can open it again or keep the old file.`},
	"config validate": {"config validate [--json] [-config PATH]", `Check the config. Errors exit 1; warnings (such as a missing folder) do not.`},
	"config schema":   {"config schema [--json] [--basic] [WORD]", `Print every setting with its label, help, kind, limits, default and restart flag. WORD filters by path, label or help.`},
	"config restart":  {"config restart [-config PATH]", `Restart the running daemon to apply pending settings. Under systemd it exits 75 and the unit starts it again; run by hand it restarts in place. A saved config that would not start is refused and the daemon keeps running.`},
	"export": {"export [--secrets-names] [--sections a,b] [-o FILE] [-config PATH]", `Write a profile bundle as JSON: agents, repos, providers (without keys),
prices, budgets, tariff, monitor, limits, sandbox, policy, files and the
models[] entries (the web export adds every local model with its download
link). Secret values never leave; secret names only with --secrets-names.`},
	"import": {"import [--yes] [-config PATH] FILE", `Show what a profile bundle changes, then apply it after you agree (or at
once with --yes). Lists merge by name. An import that would add a
validation error is refused. Models are never downloaded: the import lists
the cerberus pull command for each one.`},
	"install-service": {"install-service", `Linux only. Copy the binary to ~/.local/bin, write
~/.config/systemd/user/cerberus.service, then daemon-reload and enable --now
(restart when the binary changed).`},
	"cerberus mcp": {"mcp [--url U] [--key K] [--session ID] | mcp --client NAME [-config PATH]", `Stdio MCP server that lets an agent start and steer its own child sessions
with cerberus_spawn, cerberus_list, cerberus_read, cerberus_send,
cerberus_wait and cerberus_stop, and hand jobs to local models with
cerberus_models, cerberus_ask and cerberus_rate. Sessions run it; the
defaults come from the session's environment.

--client NAME runs it for an outside agent (Claude Code, say) paired with
"cerberus pair --mcp --name NAME": the daemon's URL comes from the config,
the token from the client's token file, cerberus_ask reads its files on this
machine, and the agent sees only the sessions it started.`},
}

// helpAliases maps alternative flag set names to their help entry.
var helpAliases = map[string]string{
	"secret ls": "secret list", "secret delete": "secret rm", "secret del": "secret rm",
	"flows": "flow", "flow ls": "flow list",
}

// setUsage gives fs the -h text from helps, when there is one.
func setUsage(fs *flag.FlagSet) {
	name := fs.Name()
	if a, ok := helpAliases[name]; ok {
		name = a
	}
	h, ok := helps[name]
	if !ok {
		return
	}
	fs.Usage = func() {
		w := fs.Output()
		writeHelp(w, h)
		n := 0
		fs.VisitAll(func(*flag.Flag) { n++ })
		if n > 0 {
			fmt.Fprintln(w, "\nFlags:")
			fs.PrintDefaults()
		}
	}
}

// printHelp writes the help entry name (without flags) to w and reports
// whether there is one.
func printHelp(w io.Writer, name string) bool {
	h, ok := helps[name]
	if ok {
		writeHelp(w, h)
	}
	return ok
}

func writeHelp(w io.Writer, h cmdHelp) {
	fmt.Fprintf(w, "Usage: cerberus %s\n\n%s\n", h.usage, strings.TrimSpace(h.about))
}

// isHelpArg reports whether s asks for help.
func isHelpArg(s string) bool {
	return s == "-h" || s == "-help" || s == "--help" || s == "help"
}
