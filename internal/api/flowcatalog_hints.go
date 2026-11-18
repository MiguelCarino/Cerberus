package api

// flowCatalog's flows.ModelHints (SCOPE "Flow quality (2026-11-16)"): what
// the small-model warning on plan, judge and agent session steps needs.

// ModelSizeMiB is the listed model's file size (0 = unknown).
func (c flowCatalog) ModelSizeMiB(id string) int {
	for _, m := range c.h.modelList() {
		if m.ID == id {
			return m.SizeMiB
		}
	}
	return 0
}

// DefaultModel is engines.default_model ("" when unset: the engine then
// picks, and the hint stays quiet).
func (c flowCatalog) DefaultModel() string { return c.h.conf().Engines.DefaultModel }

// AgentModel is the default model for a profile that takes a model (the
// session manager gives it that one when the step names none), else "".
func (c flowCatalog) AgentModel(agent string) string {
	if p, ok := c.h.conf().AgentProfile(agent); ok && p.NeedsModel {
		return c.DefaultModel()
	}
	return ""
}

// LocalModels lists the installed local models.
func (c flowCatalog) LocalModels() []string {
	var out []string
	for _, m := range c.h.modelList() {
		if m.Local {
			out = append(out, m.ID)
		}
	}
	return out
}
