// Command cerberus is Carino Systems' self-hosted LLM runtime: it runs local
// models on demand, serves one OpenAI-compatible gateway and supervises
// coding-agent sessions you can steer from a web UI.
//
//	cerberus init               first-run setup: detect, ask up to 6 questions, write the config
//	cerberus serve              run the daemon
//	cerberus install-service    install and start the systemd user unit (Linux)
//	cerberus url                sign-in links with the token
//	cerberus status             summary of the running daemon
//	cerberus doctor [--json]    safety and health checklist
//	cerberus version
//	cerberus task "…"           start an agent session
//	cerberus search "…"         search session history
//	cerberus resume ID ["…"]    new attempt of a finished session in its workspace
//	cerberus fleet --has F "…"  one session per matching repo
//	cerberus prune              delete old finished sessions now
//	cerberus flow …             flows: list, show, run (--wait), runs, cancel, approve
//	cerberus eval …             evals: list, show, validate, import, export, run, runs, report, compare, cancel, rejudge
//	cerberus sessions …         per-session reads: setup, trajectory (ATIF or JSONL)
//	cerberus guard bench        red-team the agent guard's classifier; list, compare
//	cerberus pull REF           download a model (never loads it)
//	cerberus top                live system view
//	cerberus energy             energy totals and the bill estimate
//	cerberus rapl-setup         how to let Cerberus read CPU watts
//	cerberus secret …           list, set (value on stdin) or remove vault secrets
//	cerberus pair [--name N]    pair a device: token link + QR code
//	cerberus devices [rm ID]    list or revoke paired devices
//	cerberus token rotate       replace the admin token
//	cerberus audit              the audit log
//	cerberus config …           get, set, reset, edit, validate, schema, path, restart
//	cerberus export / import    profile bundles (agents, repos, prices, …; never secrets)
//	cerberus agent …            the built-in coding agent (run by sessions)
//	cerberus mcp                stdio MCP server: agents start and steer child sessions
//
// "cerberus help COMMAND" prints one command's help; the texts live in help.go.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	"carino.systems/cerberus/internal/agent"
)

// version and build are set at build time (Makefile): version is
// 0.0.<commits> until releases start; build is the short commit, with
// "-dirty" when the tree had uncommitted changes.
var (
	version = "0.0.0"
	build   = "dev"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns the exit code. Asking for help
// ("cerberus CMD -h", "cerberus config set --help") exits 0: the flag
// package reports -h as a usage error (2) after printing the help.
//
// An unknown command keeps its exit 2 even with -h ("cerberus bogus -h",
// "cerberus help bogus"), so a typo never looks like success.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	code, known := dispatch(args, stdin, stdout, stderr)
	if code == 2 && known && len(args) > 1 && isHelpFlag(args[len(args)-1]) {
		return 0
	}
	return code
}

// helpWords is the command, plus its subcommand when "CMD SUB" has a help
// entry of its own, from the words after "cerberus help".
func helpWords(rest []string) []string {
	if len(rest) >= 2 {
		sub := rest[0] + " " + rest[1]
		if a, ok := helpAliases[sub]; ok {
			sub = a
		}
		if _, ok := helps[sub]; ok {
			return []string{rest[0], rest[1]}
		}
	}
	return []string{rest[0]}
}

func isHelpFlag(s string) bool { return s == "-h" || s == "-help" || s == "--help" }

// dispatch runs one subcommand and returns its exit code; known is false
// for an unknown command.
func dispatch(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int, known bool) {
	if len(args) == 0 {
		printUsage(stderr)
		return 2, true
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "serve":
		return serveCmd(rest, stderr), true
	case "agent":
		return agent.Main(rest, stdin, stdout), true
	case "mcp":
		return mcpCmd(rest, stdin, stdout, stderr), true
	case "status":
		return statusCmd(rest, stdout, stderr), true
	case "task":
		return taskCmd(rest, stdout, stderr), true
	case "url":
		return urlCmd(rest, stdout, stderr), true
	case "secret", "secrets":
		return secretCmd(rest, stdin, stdout, stderr), true
	case "search":
		return searchCmd(rest, stdout, stderr), true
	case "resume":
		return resumeCmd(rest, stdout, stderr), true
	case "sessions", "session":
		return sessionsCmd(rest, stdout, stderr), true
	case "fleet":
		return fleetCmd(rest, stdin, stdout, stderr), true
	case "prune":
		return pruneCmd(rest, stdout, stderr), true
	case "top":
		return topCmd(rest, stdin, stdout, stderr), true
	case "energy":
		return energyCmd(rest, stdout, stderr), true
	case "bench":
		return benchCmd(rest, stdout, stderr), true
	case "guard":
		return guardCmd(rest, stdout, stderr), true
	case "rapl-setup":
		return raplSetupCmd(rest, stdout, stderr), true
	case "audit":
		return auditCmd(rest, stdout, stderr), true
	case "user", "users":
		return userCmd(rest, stdin, stdout, stderr), true
	case "finetune":
		return finetuneCmd(rest, stdout, stderr), true
	case "flow", "flows":
		return flowCmd(rest, stdout, stderr), true
	case "eval", "evals":
		return evalCmd(rest, stdout, stderr), true
	case "pair":
		return pairCmd(rest, stdout, stderr), true
	case "devices", "device":
		return devicesCmd(rest, stdout, stderr), true
	case "token":
		return tokenCmd(rest, stdout, stderr), true
	case "doctor":
		return doctorCmd(rest, stdout, stderr), true
	case "init":
		return initCmd(rest, stdin, stdout, stderr), true
	case "config":
		return configCmd(rest, stdin, stdout, stderr), true
	case "export":
		return exportCmd(rest, stdout, stderr), true
	case "import":
		return importCmd(rest, stdin, stdout, stderr), true
	case "install-service":
		return installCmd(rest, stdout, stderr), true
	case "pull":
		return pullCmd(rest, stdout, stderr), true
	case "version", "--version", "-v":
		if len(rest) > 0 && isHelpFlag(rest[0]) {
			printHelp(stdout, "version")
			return 0, true
		}
		fmt.Fprintf(stdout, "cerberus %s (%s) %s/%s %s\n", version, build, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return 0, true
	case "help", "-h", "--help":
		// "cerberus help CMD [SUB]" is "cerberus CMD [SUB] -h". Only the
		// command words are passed on: anything after them is dropped, so
		// a command whose flags stop at its first argument (task, resume,
		// pull…) can never take "-h" as part of a task and run it.
		if len(rest) > 0 && rest[0] != "help" && rest[0] != "-h" && rest[0] != "--help" {
			return run(append(helpWords(rest), "-h"), stdin, stdout, stderr), true
		}
		printUsage(stdout)
		return 0, true
	}
	fmt.Fprintf(stderr, "cerberus: unknown command %q\n\n", cmd)
	printUsage(stderr)
	return 2, false
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: cerberus <command> [flags] [args]

Cerberus is a self-hosted LLM runtime: local models, one OpenAI-compatible
gateway and agent sessions you steer from a web UI.

Start:
  init               first-run setup: detect engines, GPU, agents, Tailscale; ask up
                       to 6 questions; write the config (--yes takes the defaults)
  serve              run the daemon (models, gateway, sessions, chats, web UI)
  install-service    install and start the systemd user unit (Linux)
  url                sign-in links with the token for every listen address
  status             short summary of the running daemon
  doctor             safety and health checklist (--json); exit 1 on a failed check
  version            print the version

Agent sessions:
  task "…"           start a session (--dir D | --repo R | --scratch, --agent --model
                       --escalate --count --title)
  search "…"         search session history (--repo --agent --state --since
                       --until --limit)
  resume ID ["…"]    start a new attempt of a finished session in its workspace
  fleet "…"          one session per repo under repo_roots (--has FILE, --name GLOB,
                       --yes); {repo} and {name} in the task expand per repo
  prune              delete finished sessions past sessions.retention_days now
  sessions setup ID  the setup a session ran with: agent, version, model, MCP servers;
                       with a second ID, the fields that differ (--json)
  sessions trajectory ID
                     the session's model calls as an ATIF trajectory (--format atif|jsonl)

Flows (saved recipes of model calls and agent sessions):
  flow list          flows with their trigger, next run and last run
  flow show ID       one flow: trigger, steps and problems
  flow run ID        start a run (--input TEXT, --wait to follow it to the end)
  flow runs [ID]     recent runs, of one flow or all (--limit N)
  flow cancel RUN    stop a run
  flow approve RUN   approve a run waiting at an approve step (--reject, --note)

Evals (agent work judged against criteria by a judge model):
  eval list          evals with their last run
  eval show ID       one eval: gates, criteria, setups, problems
  eval validate FILE check a definition; eval import FILE, eval export ID
  eval run ID        one session per setup, judged when it ends (--setup, --wait)
  eval runs [ID]     recent runs; eval report RUN prints one
  eval compare A B   gates and criteria that flipped, and the setup fields that differ
  eval cancel RUN    stop a run; eval rejudge RUN judges its sessions again (a new judge)

Models and the machine:
  pull REF           download a model, never load it: an Ollama name, lms:NAME or a
                       Hugging Face .gguf URL (--engine --sha256 --detach, --cancel ID)
  top                live CPU, memory, GPU, power, cost, processes, sessions (--once)
  energy             energy totals and the electricity bill estimate
  bench              time a model through the gateway on a fixed prompt set
                       (--model --repeat --kinds --prefill --label); bench list,
                       bench compare A B
  guard bench        red-team the agent guard's classifier over a fixed case set:
                       detection and false-alarm rates per category (--model
                       --threshold); guard bench list, guard bench compare A B
  rapl-setup         print the one-time root steps that let Cerberus read CPU watts

Secrets and access:
  secret list        vault secrets: names, notes and lengths, never values
  secret set NAME    store a secret read from stdin (--note TEXT)
  secret rm NAME     remove a secret
  pair               pair a phone or laptop: its own token, a link and a QR code
                       (--name N --url BASE --light-terminal)
  devices [rm ID]    list paired devices, or revoke one (its streams end at once)
  token rotate       replace the admin token (paired devices keep working)
  audit              the audit log (--since 24h --actor X --user NAME --limit N --json)

User accounts (see docs/USERS.md):
  user add NAME      add an account (--role admin|user|viewer|monitor|chat, --display,
                       --password-stdin or --generate); the first must be an admin
  user list          accounts with role, status, last sign-in and budget (--json)
  user passwd NAME   reset a password and end the user's sign-ins and devices
  user role NAME R   change a role (the last enabled admin stays an admin)
  user disable NAME  disable an account at once (enable NAME turns it back on)
  user rm NAME       remove an account; its data moves to an admin (--to ADMIN --yes)
  user budget NAME   monthly cap (--usd N --tokens N; 0 = none)
  user share NAME R  share a repo with a user (--accept; --unshare REPO)

Fine-tuning:
  finetune [stats]   what is collected for training a local model
  finetune export    the examples as chat JSONL (--out F --kind K --unrated)
  finetune train     export, then run finetune.train_command (--dry-run --base M)

Settings:
  config path        where config.json lives
  config get [PATH]  a setting, or the whole config without the token (--json)
  config set PATH V  change a setting; a running daemon applies it live when it can
  config reset PATH  put a setting back to its default
  config edit        edit in $EDITOR, then validate
  config validate    check the config (--json); exit 1 on an error
  config schema      every setting with its help (--json, --basic, or a search word)
  config restart     restart the daemon to apply settings that need it
  export             profile bundle as JSON, never secret values
                       (--secrets-names --sections a,b -o FILE)
  import FILE        show what a bundle changes, then apply it (--yes)

Run by sessions, not people:
  agent              the built-in coding agent
  mcp                stdio MCP server: cerberus_* tools to start and steer child sessions

Every command that reads the config takes -config PATH
(default ~/.config/cerberus/config.json).
Run "cerberus help COMMAND" or "cerberus COMMAND -h" for details.
`)
}
