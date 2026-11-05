package session

import (
	"errors"
	"fmt"
	"slices"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/vault"
)

// secretEnv resolves the session's secret variables through the vault: the
// profile's SecretEnv, then the repo's (which wins for the same variable).
// A secret that cannot be read is an error naming it, so the session goes to
// the error state instead of running without its credentials.
func (m *Manager) secretEnv(p config.AgentProfile, repo string) (map[string]string, error) {
	want := map[string]string{}
	for k, name := range p.SecretEnv {
		want[k] = name
	}
	if repo != "" {
		if rc, ok := m.repoConfig(repo); ok {
			for k, name := range rc.SecretEnv {
				want[k] = name
			}
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	vars := make([]string, 0, len(want))
	for k := range want {
		vars = append(vars, k)
	}
	slices.Sort(vars) // the first missing one is reported deterministically
	out := make(map[string]string, len(want))
	for _, k := range vars {
		name := want[k]
		if m.vault == nil {
			return nil, fmt.Errorf("secret %q (for %s) is needed but no vault is configured", name, k)
		}
		v, err := m.vault.Get(name)
		switch {
		case errors.Is(err, vault.ErrNotFound):
			return nil, fmt.Errorf("secret %q (for %s) is not in the vault; add it under Secrets", name, k)
		case err != nil:
			return nil, fmt.Errorf("secret %q (for %s): %w", name, k, err)
		}
		out[k] = v
	}
	return out, nil
}

// secretValues are the values to mask in session output (none without a
// vault). The vault caches them, but a reload after a failed read may run
// secret-tool, so it is never called with m.mu held.
func (m *Manager) secretValues() []string {
	if m.vault == nil {
		return nil
	}
	return m.vault.Values()
}

// MaskSecrets replaces, in place, every vault value and every value pinned
// into a live run found in strs (process command lines in the Monitor, which
// would otherwise show a secret an agent expanded into argv). It returns strs.
func (m *Manager) MaskSecrets(strs []string) []string {
	vals := m.secretValues() // before m.mu: may run secret-tool
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.run != nil {
			vals = append(vals, s.run.pinned...)
		}
	}
	m.mu.Unlock()
	var r redactor
	r.set(vals)
	if len(r.values) == 0 {
		return strs
	}
	for i, v := range strs {
		strs[i] = string(r.mask(nil, []byte(v)))
	}
	return strs
}
