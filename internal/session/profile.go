package session

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"carino.systems/cerberus/internal/config"
)

// placeholders are the values substituted into an AgentProfile's Command.
type placeholders struct {
	Task, TaskFile, Model, BaseURL, Cwd, Session, Self string
	ResumeFile                                         string // sessions/<id>/conversation.json
	ServerURL                                          string // the Cerberus root, no /v1 ({server_url}: ANTHROPIC_BASE_URL)
}

// apiKeyPlaceholder in a profile's env (only there, never in a command line,
// where other users could read it) is this session's gateway key.
const apiKeyPlaceholder = "{api_key}"

// expand substitutes {task} {task_file} {model} {base_url} {server_url}
// {cwd} {session} {self} and {resume_file} in every arg. Substituted text is
// not re-scanned, so a task containing "{model}" stays literal.
func (p placeholders) expand(args []string) []string {
	r := strings.NewReplacer(
		"{task}", p.Task, "{task_file}", p.TaskFile, "{model}", p.Model,
		"{base_url}", p.BaseURL, "{server_url}", p.ServerURL, "{cwd}", p.Cwd, "{session}", p.Session, "{self}", p.Self,
		"{resume_file}", p.ResumeFile)
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = r.Replace(a)
	}
	return out
}

// loopbackHost maps a listen address to one a local client can dial.
func loopbackHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "127.0.0.1", "7340"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// serverURL is the Cerberus root URL for local clients (CERBERUS_URL).
func serverURL(listen string) string { return "http://" + loopbackHost(listen) }

// baseURL is the gateway URL for local clients ({base_url}, OPENAI_BASE_URL).
func baseURL(listen string) string { return serverURL(listen) + "/v1" }

// selfPath is the running cerberus binary ({self}).
func selfPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

// profileBinary is argv[0] of p after placeholder expansion.
func profileBinary(p config.AgentProfile) string {
	if len(p.Command) == 0 {
		return ""
	}
	return placeholders{Self: selfPath()}.expand(p.Command[:1])[0]
}

// profileAvailable reports whether p's binary can be found.
func profileAvailable(p config.AgentProfile) bool {
	bin := profileBinary(p)
	if bin == "" {
		return false
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// profile returns the named, available profile or an error naming it.
func (m *Manager) profile(name string) (config.AgentProfile, error) {
	p, ok := m.conf().AgentProfile(name)
	if !ok {
		return p, fmt.Errorf("unknown agent profile %q", name)
	}
	if !profileAvailable(p) {
		return p, fmt.Errorf("agent profile %q is not available: %q not found on PATH", name, profileBinary(p))
	}
	return p, nil
}

// usesModel reports whether p's command needs a model value.
func usesModel(p config.AgentProfile) bool {
	for _, a := range p.Command {
		if strings.Contains(a, "{model}") {
			return true
		}
	}
	return false
}

// Agents lists every profile with whether its binary is installed.
func (m *Manager) Agents() []AgentStatus {
	profiles := m.conf().AgentProfiles()
	out := make([]AgentStatus, 0, len(profiles))
	for _, p := range profiles {
		if internalAgent(p.Name) {
			continue // never offered in a picker, even if the config names one so
		}
		out = append(out, AgentStatus{Name: p.Name, Description: p.Description,
			Available: profileAvailable(p), NeedsModel: p.NeedsModel, Interactive: p.Interactive})
	}
	return out
}
