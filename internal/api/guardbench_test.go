package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"carino.systems/cerberus/internal/guardbench"
)

type fakeScreen struct{ attack, benign float64 }

func (f fakeScreen) ScreenText(_ context.Context, _ /*model*/, text string) (float64, string, error) {
	set, _ := guardbench.Load()
	for _, c := range set.Cases {
		if c.Text == text {
			if guardbench.IsBenign(c.Category) {
				return f.benign, "Controversial", nil
			}
			return f.attack, "Unsafe", nil
		}
	}
	return 0, "Safe", nil
}

func TestGuardbenchRun(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.dataDir = t.TempDir()
	env.srv.guardScreen = fakeScreen{attack: 0.9, benign: 0.1}

	w := env.do("POST", "/api/guardbench", `{"model":"guard-x"}`, withBearer(testToken))
	if w.Code != 202 {
		t.Fatalf("POST status %d: %s", w.Code, w.Body.String())
	}
	var started struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil || started.ID == "" {
		t.Fatalf("start reply %q: %v", w.Body.String(), err)
	}

	var last *guardbench.Run
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g := env.do("GET", "/api/guardbench", "", withBearer(testToken))
		if g.Code != 200 {
			t.Fatalf("GET status %d: %s", g.Code, g.Body.String())
		}
		var list struct {
			Running *guardbench.Run  `json:"running"`
			Runs    []guardbench.Run `json:"runs"`
			Last    *guardbench.Run  `json:"last"`
		}
		if err := json.Unmarshal(g.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		if list.Running == nil && list.Last != nil {
			last = list.Last
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last == nil {
		t.Fatal("run never finished")
	}
	if last.State != "done" {
		t.Fatalf("run state %q: %s", last.State, last.Error)
	}
	if last.Model != "guard-x" {
		t.Errorf("model %q", last.Model)
	}
	if last.Detection != 1 || last.FalseAlarm != 0 {
		t.Errorf("detection %.2f false alarm %.2f", last.Detection, last.FalseAlarm)
	}
	if last.Attacks < 1 || last.Benign < 1 {
		t.Errorf("attacks %d benign %d", last.Attacks, last.Benign)
	}
}

func TestGuardbenchStartNoModel(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.dataDir = t.TempDir()
	env.srv.guardScreen = fakeScreen{}
	// No model passed and none configured → 400.
	w := env.do("POST", "/api/guardbench", `{}`, withBearer(testToken))
	if w.Code != 400 {
		t.Fatalf("status %d, want 400: %s", w.Code, w.Body.String())
	}
}

func TestGuardbenchEmptyList(t *testing.T) {
	env := newEnv(t, nil)
	env.srv.dataDir = t.TempDir()
	w := env.do("GET", "/api/guardbench", "", withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var list struct {
		Runs             []guardbench.Run `json:"runs"`
		AttackCategories []string         `json:"attack_categories"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 0 {
		t.Errorf("want no runs, got %d", len(list.Runs))
	}
	if len(list.AttackCategories) == 0 {
		t.Error("attack categories should be listed")
	}
}
