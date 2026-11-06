package guardbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
)

// The case set is append-only: a changed case gets a new id. Add a new
// case's hash here; never change an existing one.
var pinned = map[string]string{
	"override_readme_footer":             "c9319fb59774384b",
	"override_agents_md":                 "a27a80e96df67933",
	"override_issue_body":                "02f2bad3c9f3520d",
	"override_web_hidden":                "57cbfbd899bc3746",
	"override_command_output":            "e6640c9173a032e9",
	"override_file_comment":              "4b4d67478a806ef4",
	"override_priority_claim":            "b61cfdd2660c1cf2",
	"override_fake_tag_reset":            "eaaa433a56d39307",
	"roleplay_devmode_readme":            "faf39afe8bc31fda",
	"roleplay_grandma_web":               "8e93b9c6075c5272",
	"roleplay_two_ais":                   "d9d5c7d0586d2dc3",
	"roleplay_hypothetical":              "6321cd14c85b9080",
	"roleplay_debug_persona":             "8047e7b26bb8456e",
	"roleplay_simulator":                 "2e1547c1215c04ad",
	"roleplay_actor_instruction":         "c9d195610e502bb3",
	"encoded_base64_tool":                "295f72c8fa9ee04d",
	"encoded_rot13_readme":               "b8306d18e9e2f393",
	"encoded_hex_file":                   "8fad77b6572744df",
	"encoded_spaced":                     "2a6e4f7a53158609",
	"encoded_zero_widthish":              "4c93521dbbaef5ae",
	"encoded_reversed":                   "1e493eea4a3af02f",
	"encoded_base64_nested_web":          "692907da4f39a5fe",
	"bait_fake_tool_result":              "d4d1bccb185fa415",
	"bait_run_this":                      "7b42a3ce758ef8b2",
	"bait_injected_function":             "12fb2a7548d0b8b1",
	"bait_issue_asks_commit":             "f2329ce8c5ef31d9",
	"bait_markdown_link_action":          "44e8617e540152a3",
	"bait_chained_steps":                 "96723b1d3e28b471",
	"bait_pseudo_system_tool":            "56718a1dc828826e",
	"exfil_env_to_url":                   "3057f81c4a931d7a",
	"exfil_paste_secrets":                "804a7028e27370cf",
	"exfil_summarize_and_send":           "af778fe26fb85d16",
	"exfil_hidden_prompt_web":            "0fe41f454401da2a",
	"exfil_cred_echo":                    "51d8a7a1695065e6",
	"exfil_copy_ssh":                     "47669efdd9f45f39",
	"exfil_dm_history":                   "f83e70ddaf4ccda8",
	"benign_owasp_llm01":                 "9cfbc1e7ce16cb25",
	"benign_blog_defense":                "5da1d140df4d616a",
	"benign_threat_model":                "d3047bda9b578ba5",
	"benign_security_readme":             "f6cd1250ac466fcd",
	"benign_policy_doc":                  "d4734f47589f0ba9",
	"benign_ctf_writeup":                 "47bae171b9cf399b",
	"benign_detection_rules":             "f4a9f3c6d0f59584",
	"benign_incident_report":             "2276ecafa2ebcb6d",
	"benign_course_notes":                "ec9a522fb2f53443",
	"benign_code_ignore_flag":            "74731ed1ac8a9361",
	"benign_code_gitignore":              "56fbb9e1b5e2d49b",
	"benign_code_system_prompt_var":      "4aaeea2b049a51a8",
	"benign_code_exec_wrapper":           "fbfa6eda641bc2b3",
	"benign_code_tutorial_roleplay":      "2ae5b88bc17bfd65",
	"benign_code_test_injection_fixture": "21c929af1e31f6d9",
	"benign_code_command_output_log":     "efb0e67cd1f8d827",
	"benign_code_curl_docs":              "72d25cbd31b9b058",
	"benign_chat_question":               "b2cd6407c145cde6",
	"benign_chat_support":                "9c357a7638e0096a",
	"benign_chat_feature_request":        "6c7face190029256",
	"benign_chat_thanks":                 "c1981f9df1891589",
	"benign_chat_long_story":             "6c3d657732853fe5",
	"benign_chat_recipe":                 "016f7ba4701a5c6a",
	"benign_chat_meeting":                "075cea9e5cebaccb",
	"benign_chat_weather":                "5684f21d1f180b8c",
}

func caseHash(c Case) string {
	h := sha256.Sum256([]byte(c.ID + "\x00" + c.Category + "\x00" + c.Text))
	return hex.EncodeToString(h[:])[:16]
}

func TestCaseSetIsAppendOnly(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, c := range set.Cases {
		if seen[c.ID] {
			t.Errorf("%s appears twice", c.ID)
		}
		seen[c.ID] = true
		got := caseHash(c)
		want, ok := pinned[c.ID]
		if !ok {
			t.Errorf("new case %s: pin its hash %q in guardbench_test.go", c.ID, got)
			continue
		}
		if got != want {
			t.Errorf("case %s changed (hash %s, pinned %s): add a new id instead of editing", c.ID, got, want)
		}
	}
	for id := range pinned {
		if !seen[id] {
			t.Errorf("case %s was removed", id)
		}
	}
}

func TestCaseSetShape(t *testing.T) {
	set, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Cases) < 60 {
		t.Fatalf("only %d cases, want at least 60", len(set.Cases))
	}
	benign := 0
	known := map[string]bool{}
	for _, c := range AttackCategories {
		known[c] = true
	}
	for _, c := range BenignCategories {
		known[c] = true
	}
	perCat := map[string]int{}
	for _, c := range set.Cases {
		if !known[c.Category] {
			t.Errorf("case %s has unknown category %q", c.ID, c.Category)
		}
		if c.Place == "" || c.Text == "" {
			t.Errorf("case %s is missing place or text", c.ID)
		}
		if IsBenign(c.Category) {
			benign++
		}
		perCat[c.Category]++
	}
	if benign*3 < len(set.Cases) {
		t.Errorf("only %d benign of %d cases, want at least a third", benign, len(set.Cases))
	}
	for _, c := range append(append([]string{}, AttackCategories...), BenignCategories...) {
		if perCat[c] == 0 {
			t.Errorf("category %q has no cases", c)
		}
	}
}

// fakeClassifier scores by a lookup, or a per-category default. It never
// loads a model.
type fakeClassifier struct {
	byID    map[string]float64
	attack  float64 // default score for attack cases
	benign  float64 // default score for benign cases
	failIDs map[string]bool
	gotText map[string]bool
}

func (f *fakeClassifier) ScreenText(_ context.Context, _ /*model*/, text string) (float64, string, error) {
	if f.gotText != nil {
		f.gotText[text] = true
	}
	// Find the case by its text so the fake can key on id/category.
	set, _ := Load()
	for _, c := range set.Cases {
		if c.Text != text {
			continue
		}
		if f.failIDs[c.ID] {
			return 0, "", errors.New("classifier down")
		}
		if v, ok := f.byID[c.ID]; ok {
			return v, label(v), nil
		}
		if IsBenign(c.Category) {
			return f.benign, label(f.benign), nil
		}
		return f.attack, label(f.attack), nil
	}
	return 0, "Safe", nil
}

func label(v float64) string {
	switch {
	case v >= 1:
		return "Unsafe"
	case v > 0:
		return "Controversial"
	}
	return "Safe"
}

func TestExecutePerfectGraded(t *testing.T) {
	set, _ := Load()
	fc := &fakeClassifier{attack: 0.9, benign: 0.1, gotText: map[string]bool{}}
	r := NewRun(Options{Model: "guard-x"}, "v")
	if err := Execute(context.Background(), fc, set, r.Options, r, nil); err != nil {
		t.Fatal(err)
	}
	if len(fc.gotText) < 60 {
		t.Fatalf("classifier saw %d distinct texts", len(fc.gotText))
	}
	if r.Detection != 1 {
		t.Errorf("detection %.2f, want 1 (all attacks score 0.9 ≥ 0.5)", r.Detection)
	}
	if r.FalseAlarm != 0 {
		t.Errorf("false alarm %.2f, want 0", r.FalseAlarm)
	}
	if !r.HasScores {
		t.Error("graded scores should set HasScores")
	}
	if len(r.Sweep) == 0 {
		t.Error("graded scores should produce a sweep")
	}
	if len(r.Misses) != 0 {
		t.Errorf("no misses expected, got %v", r.Misses)
	}
	// The sweep should catch every attack up to threshold 0.9 (attacks
	// score 0.9) and flag benign only at threshold 0.1 (benign score 0.1).
	for _, p := range r.Sweep {
		if p.Detection != 1 {
			t.Errorf("threshold %.1f: detection %.2f, want 1 (attacks score 0.9)", p.Threshold, p.Detection)
		}
		wantFA := 0.0
		if p.Threshold <= 0.1 {
			wantFA = 1
		}
		if p.FalseAlarm != wantFA {
			t.Errorf("threshold %.1f: false alarm %.2f, want %.2f", p.Threshold, p.FalseAlarm, wantFA)
		}
	}
	// Every attack category reports 100% detection, every benign 0%.
	for _, c := range r.Categories {
		want := 1.0
		if c.Benign {
			want = 0
		}
		if c.Rate != want {
			t.Errorf("category %s rate %.2f, want %.2f", c.Category, c.Rate, want)
		}
	}
}

func TestExecuteMissesAndErrors(t *testing.T) {
	set, _ := Load()
	fc := &fakeClassifier{
		attack: 0.9, benign: 0.1,
		byID:    map[string]float64{"encoded_rot13_readme": 0.2, "benign_code_ignore_flag": 0.8},
		failIDs: map[string]bool{"benign_chat_weather": true},
	}
	r := NewRun(Options{Model: "g", Threshold: 0.5}, "v")
	if err := Execute(context.Background(), fc, set, r.Options, r, nil); err != nil {
		t.Fatal(err)
	}
	misses := map[string]bool{}
	for _, m := range r.Misses {
		misses[m] = true
	}
	if !misses["encoded_rot13_readme"] {
		t.Error("the low-scored attack should be a miss")
	}
	if !misses["benign_code_ignore_flag"] {
		t.Error("the high-scored benign case should be a miss (false alarm)")
	}
	if r.Errors != 1 {
		t.Errorf("errors %d, want 1", r.Errors)
	}
	// The errored benign case is excluded from the denominator.
	for _, c := range r.Categories {
		if c.Category == "benign_chat" {
			if c.Errors != 1 || c.Scored != c.Total-1 {
				t.Errorf("benign_chat: errors %d scored %d total %d", c.Errors, c.Scored, c.Total)
			}
		}
	}
}

func TestExecuteAllErrored(t *testing.T) {
	set, _ := Load()
	fail := map[string]bool{}
	for _, c := range set.Cases {
		fail[c.ID] = true
	}
	fc := &fakeClassifier{failIDs: fail}
	r := NewRun(Options{Model: "g"}, "v")
	err := Execute(context.Background(), fc, set, r.Options, r, nil)
	if err == nil {
		t.Fatal("want an error when every case fails to score")
	}
}

func TestExecuteUngraded(t *testing.T) {
	set, _ := Load()
	// Only 0 or 1 scores: no graded sweep.
	fc := &fakeClassifier{attack: 1, benign: 0}
	r := NewRun(Options{Model: "g"}, "v")
	if err := Execute(context.Background(), fc, set, r.Options, r, nil); err != nil {
		t.Fatal(err)
	}
	if r.HasScores {
		t.Error("binary scores should leave HasScores false")
	}
	if r.Sweep != nil {
		t.Error("no sweep without graded scores")
	}
	if r.Detection != 1 || r.FalseAlarm != 0 {
		t.Errorf("detection %.2f false alarm %.2f", r.Detection, r.FalseAlarm)
	}
}

func TestSaveListGetCompare(t *testing.T) {
	dir := t.TempDir()
	set, _ := Load()
	fc := &fakeClassifier{attack: 0.9, benign: 0.1}
	a := NewRun(Options{Model: "a"}, "v")
	_ = Execute(context.Background(), fc, set, a.Options, a, nil)
	a.State = "done"
	if err := Save(dir, a); err != nil {
		t.Fatal(err)
	}
	fc2 := &fakeClassifier{attack: 0.4, benign: 0.1} // misses all attacks at 0.5
	b := NewRun(Options{Model: "b"}, "v")
	_ = Execute(context.Background(), fc2, set, b.Options, b, nil)
	b.State = "done"
	if err := Save(dir, b); err != nil {
		t.Fatal(err)
	}
	runs, err := List(dir)
	if err != nil || len(runs) != 2 {
		t.Fatalf("list: %d runs, %v", len(runs), err)
	}
	got, err := Get(dir, a.ID)
	if err != nil || got.Model != "a" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if lt := Latest(dir); lt == nil {
		t.Fatal("latest should find a done run")
	}
	diffs := Compare(a, b)
	if len(diffs) == 0 {
		t.Fatal("compare produced no rows")
	}
	// B detects nothing (0.4 < 0.5), so attack categories drop by 1.0.
	for _, d := range diffs {
		if !d.Benign && d.Delta != -1 {
			t.Errorf("attack category %s delta %.2f, want -1", d.Category, d.Delta)
		}
	}
	if Summary(a, b) == "" {
		t.Error("empty summary")
	}
}

func TestGetBadPath(t *testing.T) {
	if _, err := Get(t.TempDir(), "not-an-id"); err == nil {
		t.Error("want an error for a non-id, non-json argument")
	}
	if _, err := Get(filepath.Join(t.TempDir(), "x"), "missing.json"); err == nil {
		t.Error("want an error for a missing file")
	}
}
