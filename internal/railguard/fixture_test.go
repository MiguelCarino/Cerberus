package railguard

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fixtureRel is the shared fixture inside the Atrium repo; testdata holds a
// copy with a "source" key naming it.
const fixtureRel = "home/.config/hypr/tests/fixtures/railguard-settings-cases.json"

func loadFixture(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 97.0 and 97 stay apart, as they do for Python
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// TestFixtureCases reads every case of the shared fixture (what
// railguard.py's read_settings gives) and checks the keys Cerberus reads,
// value and type: vram_pct and vram_hard_pct are floats (97.0), vram_s and
// managed_grace_s ints. Keys Cerberus does not read (ram, notify) are
// railguard's alone.
func TestFixtureCases(t *testing.T) {
	var doc struct {
		Cases []struct {
			Name string                     `json:"name"`
			File json.RawMessage            `json:"file"`
			Want map[string]json.RawMessage `json:"want"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "railguard-settings-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 8 {
		t.Fatalf("only %d cases", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			file := []byte(c.File)
			var text string
			if json.Unmarshal(c.File, &text) == nil {
				file = []byte(text) // a string is the file's raw text
			}
			got := parse(file)
			checked := 0
			for key, w := range c.Want {
				var ok bool
				switch key {
				case "vram":
					var want bool
					ok = json.Unmarshal(w, &want) == nil && got.VRAM == want
				case "vram_pct", "vram_hard_pct":
					g := map[string]float64{"vram_pct": got.VRAMPct, "vram_hard_pct": got.VRAMHardPct}[key]
					var want float64
					ok = bytes.ContainsAny(w, ".eE") && json.Unmarshal(w, &want) == nil && g == want
				case "vram_s", "managed_grace_s":
					g := map[string]int{"vram_s": got.VRAMS, "managed_grace_s": got.ManagedGraceS}[key]
					var want int
					ok = json.Unmarshal(w, &want) == nil && g == want
				case "managed_units":
					var want []string
					ok = json.Unmarshal(w, &want) == nil && slices.Equal(got.ManagedUnits, want)
				default:
					continue // a key Cerberus does not read
				}
				checked++
				if !ok {
					t.Errorf("%s: want %s, got %+v", key, w, got)
				}
			}
			if checked == 0 {
				t.Fatal("no key Cerberus reads in this case")
			}
		})
	}
}

// TestFixtureMatchesAtrium fails when testdata's copy drifts from Atrium's
// fixture. It needs the Atrium repo beside this one (or at
// ~/Github/Atrium) and skips without it.
func TestFixtureMatchesAtrium(t *testing.T) {
	cands := []string{filepath.Join("..", "..", "..", "Atrium", fixtureRel)}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Github", "Atrium", fixtureRel))
	}
	src := ""
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			src = c
			break
		}
	}
	if src == "" {
		t.Skip("Atrium's fixture is not on this machine")
	}
	copyDoc := loadFixture(t, filepath.Join("testdata", "railguard-settings-cases.json"))
	if _, ok := copyDoc["source"]; !ok {
		t.Fatal("the copy lost its source key")
	}
	delete(copyDoc, "source")
	if orig := loadFixture(t, src); !reflect.DeepEqual(copyDoc, orig) {
		t.Fatalf("testdata/railguard-settings-cases.json differs from %s: copy it again (keeping the source key)", src)
	}
}

// TestPyEdgeCases are files encoding/json would read differently from
// Python's json.load. Each want is what railguard.py's read_settings gave
// for the same text (2026-11-15, python3 with railguard imported).
func TestPyEdgeCases(t *testing.T) {
	def := Defaults()
	cases := []struct {
		name, file string
		want       func(s *Settings)
	}{
		{"NaN and Infinity literals", `{"vram_pct": NaN, "vram_s": Infinity, "managed_grace_s": -Infinity, "vram": NaN}`,
			func(s *Settings) { s.VRAMPct, s.VRAMS, s.ManagedGraceS = 80, 120, 5 }},
		{"overflow is inf", `{"vram_pct": 1e400, "vram_s": "1e999", "vram_hard_pct": "-inf"}`,
			func(s *Settings) { s.VRAMPct, s.VRAMS, s.VRAMHardPct = 100, 120, 80 }},
		{"underscores, hex, Unicode digits", `{"vram_pct": " 9_5 ", "vram_s": "1__0", "managed_grace_s": "0x10", "vram_hard_pct": "\u0661\u0669"}`,
			func(s *Settings) { s.VRAMPct, s.VRAMHardPct = 95, 80 }},
		{"nan string, signs, a trailing dot", `{"vram_pct": "nan", "vram_s": "+Infinity", "managed_grace_s": "_7", "vram_hard_pct": "9_9."}`,
			func(s *Settings) { s.VRAMPct, s.VRAMS, s.VRAMHardPct = 80, 120, 99 }},
		{"unit names by str()", `{"managed_units": ["b.scope", 3, 3.0, 1e2, 1e16, 1e-5, true, null, {"x": 1, "y": ["it's"]}, [], "\u001c", "\u00a0", -0, -0.0]}`,
			func(s *Settings) {
				s.ManagedUnits = []string{"-0.0", "0", "100.0", "1e+16", "1e-05", "3", "3.0", "None", "True", "[]", "b.scope", `{'x': 1, 'y': ["it's"]}`}
			}},
		{"reprs", `{"managed_units": ["x", {"a": "q\"'"}, ["\n\t\u0001\u00e9\u200b"], 1.5e20, 123456789012345680000]}`,
			func(s *Settings) {
				s.ManagedUnits = []string{"1.5e+20", "123456789012345680000", `['\n\t\x01` + "\u00e9" + `\u200b']`, "x", `{'a': 'q"\''}`}
			}},
		{"a repeated key: the last", `{"vram": "", "vram_pct": 90, "vram_pct": 85}`,
			func(s *Settings) { s.VRAM, s.VRAMPct = false, 85 }},
		{"empty list is false, 0.0 clamps", `{"vram": [], "vram_s": 0.0}`, func(s *Settings) { s.VRAM, s.VRAMS = false, 3 }},
		{"empty dict is false, -0", `{"vram": {}, "vram_s": -0}`, func(s *Settings) { s.VRAM, s.VRAMS = false, 3 }},
		{"-0.0 is false", `{"vram": -0.0}`, func(s *Settings) { s.VRAM = false }},
		{"underflow is 0", `{"vram": 1e-400}`, func(s *Settings) { s.VRAM = false }},
		{"trailing comma", `{"vram_pct": 90,}`, func(*Settings) {}},
		{"extra data", `{"vram_pct": 90} x`, func(*Settings) {}},
		{"a BOM", "\ufeff{\"vram_pct\": 90}", func(*Settings) {}},
		{"whitespace around", "  {\"vram_pct\": 90}\n\n", func(s *Settings) { s.VRAMPct = 90 }},
		{"leading zero", `{"vram_pct": 01}`, func(*Settings) {}},
		{"bare dot", `{"vram_pct": 9.}`, func(*Settings) {}},
		{"spaces in strings", `{"vram_pct": "9.5e1", "vram_s": "  12  ", "managed_grace_s": "\u00a030\u2003"}`,
			func(s *Settings) { s.VRAMPct, s.ManagedGraceS = 95, 30 }},
		{"more float() forms", `{"vram_pct": "inFINity", "vram_s": "1_0.5_0", "managed_grace_s": "1e1_0", "vram_hard_pct": ".5e2"}`,
			func(s *Settings) { s.VRAMPct, s.VRAMS, s.ManagedGraceS, s.VRAMHardPct = 100, 10, 60, 80 }},
		{"a NUL in a number", `{"vram_pct": "95\u0000"}`, func(*Settings) {}},
		{"null file", `null`, func(*Settings) {}},
		{"a string file", `"x"`, func(*Settings) {}},
		{"bad UTF-8", "{\"vram_pct\": 90, \"x\": \"\xff\"}", func(*Settings) {}},
		// float() strips C's ASCII spaces only: U+001C to U+001F stay and
		// it refuses them, though str.isspace (and so strip) counts them.
		{"U+001C in a number", `{"vram_s": "\u001c5", "vram_pct": "5\u001f", "managed_grace_s": " \u000b30\u000c ", "vram_hard_pct": " \u001c99"}`,
			func(s *Settings) { s.ManagedGraceS = 30 }},
		{"a sign before nan", `{"vram_s": "+nan", "vram_pct": "-NaN", "managed_grace_s": "-nan", "vram_hard_pct": "+inf"}`,
			func(s *Settings) { s.VRAMS, s.VRAMPct, s.ManagedGraceS, s.VRAMHardPct = 3, 80, 5, 100 }},
		// Python keeps a lone surrogate in the str; Cerberus keeps its WTF-8
		// bytes, which sort and repr the same.
		{"lone surrogates", `{"managed_units": ["\ud800", ["\udfff\ud83d"], "\ud83d😀", "퟿", "", "\udc00x", " \ud800 "]}`,
			func(s *Settings) {
				s.ManagedUnits = []string{" \xed\xa0\x80 ", `['\udfff\ud83d']`, "퟿", "\xed\xa0\x80",
					"\xed\xa0\xbd\U0001F600", "\xed\xb0\x80x", ""}
			}},
		// Python's C scanner reads far deeper than sys.getrecursionlimit().
		{"5000 levels deep", `{"managed_units": [` + strings.Repeat("[", 5000) + strings.Repeat("]", 5000) + `], "vram_pct": 90}`,
			func(s *Settings) {
				s.VRAMPct = 90
				s.ManagedUnits = []string{strings.Repeat("[", 5000) + strings.Repeat("]", 5000)}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := def
			want.ManagedUnits = slices.Clone(def.ManagedUnits)
			c.want(&want)
			got := parse([]byte(c.file))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got  %+v\nwant %+v", got, want)
			}
		})
	}
	if s := parse([]byte(`{"vram_pct": NaN}`)); math.IsNaN(s.VRAMPct) {
		t.Fatal("NaN kept")
	}
}
