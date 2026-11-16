// Package railguard reads the settings of Atrium's railguard, the desktop
// memory watchdog that ends processes when RAM or VRAM stays too full (SCOPE
// "Railguard contract and engine hardening"). Cerberus never needs it: with
// no railguard files every reader here returns the defaults and present =
// false. It only reads; it never writes railguard's files, and railguard
// never reads Cerberus' config or token.
package railguard

import (
	"os"
	"path/filepath"
	"slices"
)

// File names inside Dir.
const (
	SettingsFile = "railguard.json"
	HistoryFile  = "railguard-history.jsonl"
)

// Settings are the railguard keys Cerberus cares about. Every key is
// optional in railguard.json; missing keys take railguard's defaults
// (Defaults), and present ones are read the way railguard reads them
// (parse).
type Settings struct {
	VRAM          bool     // VRAM watch on (key "vram")
	VRAMPct       float64  // VRAM line in percent of the card ("vram_pct")
	VRAMS         int      // seconds above the line before it acts ("vram_s")
	ManagedUnits  []string // units it asks for VRAM before killing ("managed_units")
	ManagedGraceS int      // seconds it waits after asking ("managed_grace_s", 5-60)
	VRAMHardPct   float64  // above this it acts at once ("vram_hard_pct")
}

// Defaults are railguard's own defaults. cerberus-lmstudio.scope is where
// Cerberus starts LM Studio's daemon when lmstudio.service is not there
// (engine/lmsdaemon.go). The list is sorted, as railguard keeps it.
func Defaults() Settings {
	return Settings{VRAM: true, VRAMPct: 97, VRAMS: 12,
		ManagedUnits:  []string{"cerberus-lmstudio.scope", "cerberus.service", "lmstudio.service"},
		ManagedGraceS: 20, VRAMHardPct: 99.5}
}

// ranges are railguard's RANGES for the numbers Cerberus reads: a value is
// clamped into them.
var ranges = map[string][2]float64{
	"vram_pct": {80, 100}, "vram_s": {3, 120},
	"managed_grace_s": {5, 60}, "vram_hard_pct": {80, 100},
}

// Dir is railguard's state folder: $XDG_STATE_HOME/hypr, by default
// ~/.local/state/hypr. "" when no home is known.
func Dir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "hypr")
}

// Read returns railguard's settings from dir and whether railguard is
// present there (railguard.json or railguard-history.jsonl exists). A
// missing or unreadable railguard.json gives the defaults.
func Read(dir string) (s Settings, present bool) {
	s = Defaults()
	if dir == "" {
		return s, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, SettingsFile))
	if err == nil {
		present = true
		s = parse(raw)
	} else if _, err := os.Stat(filepath.Join(dir, HistoryFile)); err == nil {
		present = true
	}
	return s, present
}

// parse applies the keys of raw over the defaults exactly as railguard's
// read_settings does (the contract: SCOPE "Railguard contract and engine
// hardening"). The file is read as Python's json.load reads it (pyjson.go);
// where that raises, or the file is not an object, it is all defaults.
// Then each key on its own, so one bad value never drops the others: "vram"
// is Python's bool(); the numbers go through Python's float() (ints,
// floats, bools, numeric strings) and are clamped into railguard's RANGES
// as min(hi, max(lo, x)) does (NaN lands on lo), vram_s and
// managed_grace_s truncated to whole seconds; a value float() refuses
// keeps that key's default; managed_units, when a list, is
// sorted({str(x) for x in v if str(x).strip()}).
func parse(raw []byte) Settings {
	s := Defaults()
	v, ok := decodePy(raw)
	if !ok {
		return s
	}
	f, ok := v.(*pyDict)
	if !ok {
		return s
	}
	for _, k := range f.keys {
		v := f.vals[k]
		switch k {
		case "vram":
			s.VRAM = truthy(v)
		case "managed_units":
			if list, ok := v.([]any); ok {
				s.ManagedUnits = unitList(list)
			}
		case "vram_pct", "vram_s", "managed_grace_s", "vram_hard_pct":
			n, ok := pyFloat(v)
			if !ok {
				continue
			}
			n = clamp(n, ranges[k])
			switch k {
			case "vram_pct":
				s.VRAMPct = n
			case "vram_s":
				s.VRAMS = int(n)
			case "managed_grace_s":
				s.ManagedGraceS = int(n)
			case "vram_hard_pct":
				s.VRAMHardPct = n
			}
		}
	}
	return s
}

// clamp is Python's min(hi, max(lo, x)): max keeps lo unless x is greater,
// min keeps that unless hi is smaller, so NaN gives lo, +inf hi, -inf lo.
func clamp(x float64, r [2]float64) float64 {
	y := r[0]
	if x > y {
		y = x
	}
	if y < r[1] {
		return y
	}
	return r[1]
}

// unitList is railguard's sorted({str(x) for x in v if str(x).strip()}):
// every entry by Python's str() (3 is "3", 3.0 "3.0", null "None", a dict
// its repr), blank ones left out. Go sorts strings by their UTF-8 bytes,
// which is Python's code point order.
func unitList(list []any) []string {
	out := []string{}
	for _, x := range list {
		str := pyStr(x)
		if !pyBlank(str) && !slices.Contains(out, str) {
			out = append(out, str)
		}
	}
	slices.Sort(out)
	return out
}
