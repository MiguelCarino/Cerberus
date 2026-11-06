package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// evalDef is the part of an eval definition (DATA/evals/<id>.json) the
// check reads.
type evalDef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	JudgeModel string `json:"judge_model"`
	Setups     []struct {
		Label string `json:"label"`
		Model string `json:"model"`
	} `json:"setups"`
}

// readEvalDefs reads the saved evals, read-only (nothing is created).
func readEvalDefs(dataDir string) []evalDef {
	if dataDir == "" {
		return nil
	}
	ents, err := os.ReadDir(filepath.Join(dataDir, "evals"))
	if err != nil {
		return nil
	}
	var out []evalDef
	for _, en := range ents {
		name := en.Name()
		if en.IsDir() || !strings.HasPrefix(name, "ev-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dataDir, "evals", name))
		if err != nil || len(b) > 1<<20 {
			continue
		}
		var d evalDef
		if json.Unmarshal(b, &d) == nil && d.ID+".json" == name {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// modelKnown: configured (models[]), under an enabled provider, or listed
// by an engine. listed is false when no engine listing was available.
func modelKnown(ctx context.Context, e Env, id string) (known, listed bool) {
	for _, m := range e.Cfg.Models {
		if m.ID == id {
			return true, true
		}
	}
	for _, p := range e.Cfg.Providers {
		if p.Enabled && p.Name != "" && strings.HasPrefix(id, p.Name+"/") {
			return true, true
		}
	}
	if e.Models == nil {
		return false, false
	}
	for _, m := range e.Models(ctx) {
		if m == id {
			return true, true
		}
	}
	return false, true
}

// evalsCheck (E1): the judge model is known, and no eval has a setup
// judged by its own model.
func evalsCheck(ctx context.Context, e Env) Check {
	const id, name = "evals", "Evals"
	ec := e.Cfg.Evals
	if !ec.Enabled {
		return ok(id, name, "off")
	}
	judge := strings.TrimSpace(ec.JudgeModel)
	if judge == "" {
		judge = strings.TrimSpace(e.Cfg.Engines.DefaultModel)
	}
	defs := readEvalDefs(e.DataDir)
	var notes []string
	var unknown, unchecked []string
	checked := map[string]bool{}
	check := func(m string) {
		if m == "" || checked[m] {
			return
		}
		checked[m] = true
		switch known, listed := modelKnown(ctx, e, m); {
		case !known && listed:
			unknown = append(unknown, m)
		case !known:
			// No engine listing (the CLI doctor): a model the config does
			// not place may still be one an engine serves, so it is not
			// flagged, only named as unchecked.
			unchecked = append(unchecked, m)
		}
	}
	check(judge)
	var self []string
	for _, d := range defs {
		j := strings.TrimSpace(d.JudgeModel)
		if j == "" {
			j = judge
		} else {
			check(j)
		}
		for _, s := range d.Setups {
			if j != "" && s.Model == j {
				self = append(self, fmt.Sprintf("%s (%s)", d.ID, s.Label))
			}
		}
	}
	if len(unknown) > 0 {
		return warn(id, name, "judge model not known: "+strings.Join(unknown, ", "),
			"set evals.judge_model (or an eval's judge_model) to a model an engine lists, a configured model or a provider model")
	}
	if judge == "" {
		missing := false
		for _, d := range defs {
			if strings.TrimSpace(d.JudgeModel) == "" {
				missing = true
			}
		}
		if missing {
			return warn(id, name, "no judge model: evals without their own judge_model cannot run",
				"set evals.judge_model or engines.default_model")
		}
	}
	if len(self) > 0 {
		return warn(id, name, "a model judges itself in "+strings.Join(self, ", "),
			"judge with a different model (evals.judge_model or the eval's judge_model): a model grading its own work is lenient")
	}
	if judge != "" {
		notes = append(notes, "judge "+judge)
	} else {
		notes = append(notes, "each eval names its judge")
	}
	notes = append(notes, fmt.Sprintf("%d eval(s)", len(defs)))
	if len(unchecked) > 0 {
		notes = append(notes, "not checked without an engine listing: "+strings.Join(unchecked, ", "))
	}
	return ok(id, name, strings.Join(notes, ", "))
}
