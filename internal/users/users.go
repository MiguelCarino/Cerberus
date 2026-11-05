// Package users holds Cerberus' user accounts (Phase 4a): several people on
// one daemon, each with a role. Accounts live in DATA/users.json (mode
// 0600, written atomically), browser sign-ins in DATA/logins.json, and
// failed sign-ins are counted in memory (Lockout).
//
// Passwords are PBKDF2-HMAC-SHA256 (stdlib crypto/pbkdf2) with a random
// 16-byte salt per hash and 600 000 iterations; only the hash is stored.
// A login cookie ("cls-…") and a personal /v1 key ("cuk-…") are 32 random
// bytes stored only as their SHA-256.
//
// No users means nothing changes: the daemon behaves as a single-owner
// server (admin token, paired devices). Creating the first user (always an
// admin) sets Moved[""] to that user, so every object from before users
// (Owner "") belongs to the first admin from then on. Removing a user sets
// Moved[<id>] to the admin who takes their data; Owner follows the chain.
//
// See docs/SCOPE.md "Users and roles (Phase 4a)".
package users

import (
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// Roles.
const (
	RoleAdmin   = "admin"
	RoleUser    = "user"
	RoleViewer  = "viewer"
	RoleMonitor = "monitor"
	RoleChat    = "chat"
)

// Roles is every role, in the order the UI lists them.
var Roles = []string{RoleAdmin, RoleUser, RoleViewer, RoleMonitor, RoleChat}

// ValidRole reports whether r is a role name.
func ValidRole(r string) bool { return slices.Contains(Roles, r) }

// Capability ids (the "caps" of GET /api/me).
const (
	CapChat          = "chat"
	CapChatTools     = "chat.tools"
	CapChatImport    = "chat.import"
	CapSessions      = "sessions"
	CapWatchAll      = "sessions.watch_all"
	CapSessionsWrite = "sessions.write"
	CapFlows         = "flows"
	CapMonitor       = "monitor"
	CapMonitorSignal = "monitor.signal"
	CapDoctor        = "doctor"
	CapDoctorCounts  = "doctor.counts"
	CapModelsRead    = "models.read"
	CapModelsManage  = "models.manage"
	CapUsage         = "usage"
	CapUsageAll      = "usage.all"
	CapGateway       = "gateway"
	CapVoice         = "voice"
	CapDevices       = "devices"
	CapDevicesAll    = "devices.all"
	CapSettings      = "settings"
	CapSecrets       = "secrets"
	CapUsers         = "users"
	CapAudit         = "audit"
	CapFinetune      = "finetune"
	CapHost          = "host"
)

// AllCaps is every capability id, in a stable order.
var AllCaps = []string{CapChat, CapChatTools, CapChatImport, CapSessions, CapWatchAll, CapSessionsWrite,
	CapFlows, CapMonitor, CapMonitorSignal, CapDoctor, CapDoctorCounts, CapModelsRead, CapModelsManage,
	CapUsage, CapUsageAll, CapGateway, CapVoice, CapDevices, CapDevicesAll, CapSettings, CapSecrets,
	CapUsers, CapAudit, CapFinetune, CapHost}

// roleCaps is the role table of docs/SCOPE.md "Roles", in code. A user's
// "own only" rows (sessions, usage, devices) are the cap without its "all"
// sibling.
var roleCaps = map[string][]string{
	RoleAdmin: AllCaps,
	RoleUser: {CapChat, CapChatTools, CapSessions, CapSessionsWrite, CapFlows, CapModelsRead, CapUsage,
		CapGateway, CapVoice, CapDevices},
	RoleViewer:  {CapWatchAll, CapMonitor, CapDoctorCounts, CapDevices},
	RoleMonitor: {CapMonitor, CapDoctorCounts, CapDevices},
	RoleChat:    {CapChat, CapUsage, CapVoice, CapDevices},
}

// Can reports whether role has the capability.
func Can(role, capID string) bool { return slices.Contains(roleCaps[role], capID) }

// Caps is the role's capabilities (a copy; empty for an unknown role).
func Caps(role string) []string { return slices.Clone(append([]string{}, roleCaps[role]...)) }

// Home is the role's home view in the web UI.
func Home(role string) string {
	switch role {
	case RoleViewer:
		return "#/sessions"
	case RoleMonitor:
		return "#/monitor"
	}
	return "#/chat"
}

// File is users.json.
type File struct {
	Version int               `json:"version"` // 1
	Users   []User            `json:"users"`
	Moved   map[string]string `json:"moved,omitempty"` // owner id -> new owner id ("" = data from before users)
}

// User is one account.
type User struct {
	ID          string      `json:"id"`                // "u-" + 12 lowercase hex, random, never reused
	Name        string      `json:"name"`              // login name: lowercase [a-z0-9._-], 1..32, starts with a letter or digit
	Display     string      `json:"display,omitempty"` // shown name, 0..60 runes
	Role        string      `json:"role"`
	Disabled    bool        `json:"disabled,omitempty"`
	Password    string      `json:"password"` // "pbkdf2-sha256$<iter>$<salt b64raw>$<key b64raw>"
	MustChange  bool        `json:"must_change,omitempty"`
	Created     time.Time   `json:"created"`
	Updated     time.Time   `json:"updated"`
	PasswordSet time.Time   `json:"password_set"`
	LastLogin   time.Time   `json:"last_login,omitzero"`
	LastAddr    string      `json:"last_addr,omitempty"`
	Budget      Budget      `json:"budget"`
	Repos       []RepoShare `json:"repos,omitempty"`
	Agents      []string    `json:"agents,omitempty"` // agent profiles allowed; empty = the built-in agent only
	APIKeyHash  string      `json:"api_key_hash,omitempty"`
	APIKeyHint  string      `json:"api_key_hint,omitempty"`
	APIKeyMade  time.Time   `json:"api_key_made,omitzero"`
}

// Budget is a per-user monthly cap. Zero means no cap.
type Budget struct {
	MonthlyUSD    float64 `json:"monthly_usd"`
	MonthlyTokens int64   `json:"monthly_tokens"`
}

// RepoShare is one repo shared with a non-admin user.
type RepoShare struct {
	Path   string `json:"path"`   // absolute repo top level
	Accept bool   `json:"accept"` // may fast-forward main with Accept
}

// Label is the display name, or the login name.
func (u User) Label() string {
	if u.Display != "" {
		return u.Display
	}
	return u.Name
}

// Share returns the share of repo (a cleaned absolute path), if any.
func (u User) Share(repo string) (RepoShare, bool) {
	repo = filepath.Clean(repo)
	for _, r := range u.Repos {
		if filepath.Clean(r.Path) == repo {
			return r, true
		}
	}
	return RepoShare{}, false
}

// DefaultAgent is the agent profile a user may always use.
const DefaultAgent = "cerberus"

// AllowsAgent reports whether a non-admin user may start sessions with the
// agent profile.
func (u User) AllowsAgent(name string) bool {
	if len(u.Agents) == 0 {
		return name == DefaultAgent
	}
	return slices.Contains(u.Agents, name)
}

// Errors.
var (
	ErrBadLogin   = errors.New("Wrong user name or password.")
	ErrNotFound   = errors.New("no such user")
	ErrName       = errors.New("a user name is 1 to 32 characters: lowercase letters, digits, '.', '_' or '-', starting with a letter or digit")
	ErrNameTaken  = errors.New("that user name is taken")
	ErrRole       = errors.New("role must be admin, user, viewer, monitor or chat")
	ErrFirstAdmin = errors.New("the first user must be an admin")
	ErrLastAdmin  = errors.New("this is the last enabled admin: add or enable another admin first")
	ErrDisplay    = errors.New("the display name is at most 60 characters")
)

// PasswordError is a password rule failure.
type PasswordError struct{ Msg string }

func (e *PasswordError) Error() string { return e.Msg }

// IsPasswordError reports whether err is a password rule failure.
func IsPasswordError(err error) bool {
	var pe *PasswordError
	return errors.As(err, &pe)
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// NormName lowercases and trims a name (the lookup form).
func NormName(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// ValidName reports whether name (already normalised) is a valid login name.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Password rules: 10 to 256 bytes, not the name (case-insensitive).
const (
	MinPassword = 10
	MaxPassword = 256
)

// CheckPassword applies the password rules.
func CheckPassword(name, pw string) error {
	switch {
	case len(pw) < MinPassword:
		return &PasswordError{Msg: "the password must be at least 10 characters"}
	case len(pw) > MaxPassword:
		return &PasswordError{Msg: "the password must be at most 256 bytes"}
	case strings.EqualFold(strings.TrimSpace(pw), strings.TrimSpace(name)):
		return &PasswordError{Msg: "the password must not be the user name"}
	}
	return nil
}

// cleanDisplay keeps a display name printable and short.
func cleanDisplay(s string) (string, error) {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if len([]rune(s)) > 60 {
		return "", ErrDisplay
	}
	return s, nil
}

// FilesDir is a user's own folder: DATA/users/<id>/files.
func FilesDir(dataDir, id string) string { return filepath.Join(dataDir, "users", id, "files") }
