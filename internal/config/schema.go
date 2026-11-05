package config

// Config schema (day phase 4): every setting described for the CLI
// (`cerberus config schema`), the API (GET /api/config/schema) and the web
// Settings view.
//
// The shape comes from reflection over Config (JSON tags are the paths);
// the words come from the hand-written table in schema_meta.go. Paths use
// "[]" for list items and ".*" for map values in the schema
// ("agents[].command", "prices.*.in"); concrete paths for Get and Set use
// an index or a key instead ("agents[2].command", "prices.gpt-4o.in" or
// `prices["qwen2.5"].in` when the key holds dots).

import (
	"reflect"
	"strings"
	"time"
)

// Field kinds.
const (
	KindString    = "string"
	KindInt       = "int"
	KindFloat     = "float"
	KindBool      = "bool"
	KindEnum      = "enum"
	KindDuration  = "duration-seconds" // an int number of seconds; Set also takes "10m"
	KindPath      = "path"
	KindList      = "list"
	KindMap       = "map"
	KindSecretRef = "secret-ref" // the NAME of a vault secret, never its value
	KindTime      = "time"       // RFC 3339 timestamp (read-only)
)

// Field formats: hints for strings that are validated beyond their kind.
const (
	FormatAddress = "address" // host:port
	FormatURL     = "url"     // http(s)://host[...]
	FormatSize    = "size"    // systemd size: 6G, 512M, 50%, infinity
	FormatCommand = "command" // argv list with {placeholders}
	FormatGlob    = "glob"
	FormatEmail   = "email"
)

// Levels.
const (
	LevelBasic    = "basic"
	LevelAdvanced = "advanced"
)

// Groups, in display order (Groups returns them).
const (
	GroupServer        = "Server"
	GroupModels        = "Models"
	GroupGateway       = "Gateway & spend"
	GroupSessions      = "Sessions"
	GroupAgents        = "Agents"
	GroupSafety        = "Safety"
	GroupMonitor       = "Monitor & energy"
	GroupVoice         = "Voice"
	GroupNotifications = "Notifications"
	GroupFiles         = "Files"
	GroupAdvanced      = "Advanced"
)

// Groups lists the setting groups in display order.
func Groups() []string {
	return []string{GroupServer, GroupModels, GroupGateway, GroupSessions, GroupAgents, GroupSafety,
		GroupMonitor, GroupVoice, GroupNotifications, GroupFiles, GroupAdvanced}
}

// Field describes one setting. Lists and maps carry the schema of their
// items in Items: the fields of a struct item (paths below "x[]" or "x.*"),
// or a single field for a scalar item (path "x[]" or "x.*").
type Field struct {
	Path      string   `json:"path"`
	Label     string   `json:"label"`
	Help      string   `json:"help"`
	Group     string   `json:"group"`
	Level     string   `json:"level"`
	Kind      string   `json:"kind"`
	Format    string   `json:"format,omitempty"`
	Enum      []string `json:"enum,omitempty"`
	Min       *float64 `json:"min,omitempty"`
	Max       *float64 `json:"max,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Restart   bool     `json:"restart,omitempty"`   // a change needs a daemon restart
	Sensitive bool     `json:"sensitive,omitempty"` // never returned by Get or the API (the token)
	ReadOnly  bool     `json:"read_only,omitempty"` // shown, never set by Set
	Required  bool     `json:"required,omitempty"`  // must not be empty
	Default   any      `json:"default,omitempty"`   // only for fields outside lists and maps
	Items     []Field  `json:"items,omitempty"`
}

var timeType = reflect.TypeOf(time.Time{})

// Schema returns every setting: the leaves of nested sections, and lists and
// maps as one field each with their item schema. The order follows Config.
func Schema() []Field {
	def := Defaults()
	fields := buildFields(reflect.TypeOf(Config{}), "")
	fillDefaults(fields, def)
	return fields
}

// SchemaField finds the field for a schema path ("agents[].command") or a
// concrete one ("agents[2].command").
func SchemaField(path string) (Field, bool) {
	gen := path
	if g, err := genericPath(path); err == nil {
		gen = g
	}
	var find func(fs []Field) (Field, bool)
	find = func(fs []Field) (Field, bool) {
		for _, f := range fs {
			if f.Path == gen {
				return f, true
			}
			if strings.HasPrefix(gen, f.Path) {
				if r, ok := find(f.Items); ok {
					return r, true
				}
			}
		}
		return Field{}, false
	}
	return find(Schema())
}

func fillDefaults(fields []Field, def *Config) {
	root := reflect.ValueOf(def).Elem()
	for i := range fields {
		f := &fields[i]
		if f.Sensitive || strings.ContainsAny(f.Path, "[*") {
			continue
		}
		toks, err := parsePath(f.Path)
		if err != nil {
			continue
		}
		_ = walk(root, f.Path, toks, false, func(v reflect.Value, _ string) error {
			if v.IsValid() && v.CanInterface() {
				f.Default = v.Interface()
			}
			return nil
		})
	}
}

func joinPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// jsonName returns the JSON key of a struct field ("" to skip it).
func jsonName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		name = f.Name
	}
	return name
}

func buildFields(t reflect.Type, prefix string) []Field {
	var out []Field
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		name := jsonName(sf)
		if name == "" {
			continue
		}
		p := joinPath(prefix, name)
		ft := sf.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft != timeType {
			out = append(out, buildFields(ft, p)...)
			continue
		}
		out = append(out, leafField(p, ft))
	}
	return out
}

// leafField builds the field at path p for a non-struct type (or time).
func leafField(p string, t reflect.Type) Field {
	f := Field{Path: p, Kind: reflectKind(t), Level: LevelAdvanced}
	switch t.Kind() {
	case reflect.Slice:
		f.Items = itemFields(t.Elem(), p+"[]", p)
	case reflect.Map:
		f.Items = itemFields(t.Elem(), p+".*", p)
	}
	applyMeta(&f, p)
	return f
}

func itemFields(t reflect.Type, p, parent string) []Field {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct && t != timeType {
		return buildFields(t, p)
	}
	f := leafField(p, t)
	if _, ok := metaTable[p]; !ok {
		// A scalar item without its own entry takes its parent's words.
		if pm, ok := metaTable[parent]; ok {
			f.Label, f.Help, f.Group, f.Level = pm.label, pm.help, pm.group, pm.level
		}
	}
	return []Field{f}
}

func reflectKind(t reflect.Type) string {
	if t == timeType {
		return KindTime
	}
	switch t.Kind() {
	case reflect.String:
		return KindString
	case reflect.Bool:
		return KindBool
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return KindInt
	case reflect.Float32, reflect.Float64:
		return KindFloat
	case reflect.Slice, reflect.Array:
		return KindList
	case reflect.Map:
		return KindMap
	}
	return KindString
}

func applyMeta(f *Field, p string) {
	m, ok := metaTable[p]
	if !ok {
		return
	}
	f.Label, f.Help, f.Group = m.label, m.help, m.group
	if m.level != "" {
		f.Level = m.level
	}
	if m.kind != "" && f.Kind != KindList && f.Kind != KindMap {
		f.Kind = m.kind
	}
	if len(m.enum) > 0 {
		f.Enum = m.enum
		if f.Kind == KindString {
			f.Kind = KindEnum
		}
	}
	f.Format, f.Unit, f.Min, f.Max = m.format, m.unit, m.min, m.max
	f.Restart, f.Sensitive, f.ReadOnly, f.Required = m.restart, m.sensitive, m.readOnly, m.required
}
