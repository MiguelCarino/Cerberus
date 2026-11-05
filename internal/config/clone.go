package config

import (
	"encoding/json"
	"reflect"
	"time"
)

// Clone returns a deep copy of c (same file path), safe against a
// concurrent token rotation.
func (c *Config) Clone() *Config {
	tokenMu.RLock()
	raw, err := json.Marshal(c)
	tokenMu.RUnlock()
	n := &Config{}
	if err != nil || json.Unmarshal(raw, n) != nil {
		// Config always marshals; keep a shallow copy as a last resort.
		tokenMu.RLock()
		*n = *c
		tokenMu.RUnlock()
	}
	n.path = c.path
	return n
}

// Redacted returns a deep copy with the sensitive settings emptied, for
// showing the config (GET /api/config, `cerberus config get`).
func (c *Config) Redacted() *Config {
	n := c.Clone()
	n.Server.Token = ""
	return n
}

// PathChange is one difference found by Diff, at a top-level schema field
// (lists and maps compare as a whole: "agents", "prices").
type PathChange struct {
	Path      string `json:"path"`
	Old       any    `json:"old,omitempty"`
	New       any    `json:"new,omitempty"`
	Restart   bool   `json:"restart,omitempty"`   // applying it needs a daemon restart
	Sensitive bool   `json:"sensitive,omitempty"` // values left out
}

// Diff lists the settings that differ from a to b, in schema order. An empty
// list and a missing (null) one are equal. Sensitive values are left out.
func Diff(a, b *Config) []PathChange {
	if a == nil {
		a = &Config{}
	}
	if b == nil {
		b = &Config{}
	}
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	ra, rb := reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem()
	var out []PathChange
	for _, f := range buildFields(reflect.TypeOf(Config{}), "") {
		toks, err := parsePath(f.Path)
		if err != nil {
			continue
		}
		var va, vb reflect.Value
		_ = walk(ra, f.Path, toks, false, func(v reflect.Value, _ string) error { va = v; return nil })
		_ = walk(rb, f.Path, toks, false, func(v reflect.Value, _ string) error { vb = v; return nil })
		if sameValue(va, vb) {
			continue
		}
		ch := PathChange{Path: f.Path, Restart: f.Restart, Sensitive: f.Sensitive}
		if !f.Sensitive {
			ch.Old, ch.New = deepCopyValue(va), deepCopyValue(vb)
		}
		out = append(out, ch)
	}
	return out
}

func sameValue(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() {
		return a.IsValid() == b.IsValid()
	}
	if (a.Kind() == reflect.Slice || a.Kind() == reflect.Map) && a.Len() == 0 && b.Len() == 0 {
		return true
	}
	if a.Type() == timeType {
		return a.Interface().(time.Time).Equal(b.Interface().(time.Time))
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

// NeedsRestart reports whether any change needs a daemon restart.
func NeedsRestart(changes []PathChange) bool {
	for _, ch := range changes {
		if ch.Restart {
			return true
		}
	}
	return false
}
