package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Errors from Get and Set.
var (
	ErrNoPath    = errors.New("no such setting")
	ErrSensitive = errors.New("setting is sensitive and never read or written by path")
	ErrReadOnly  = errors.New("setting is read-only")
	ErrType      = errors.New("wrong type")
	ErrInvalid   = errors.New("invalid setting")
)

// token kinds of a parsed path.
const (
	tokName  = iota // a struct field or an unquoted map key
	tokIndex        // [n]
	tokKey          // ["key"]
)

type pathTok struct {
	kind  int
	name  string // tokName / tokKey
	index int    // tokIndex
	start int    // byte offset where the token text starts in the path
	end   int    // byte offset just past the token text
}

// parsePath splits "a.b[2].c" and `prices["x.y"].in` into tokens.
func parsePath(p string) ([]pathTok, error) {
	if p == "" {
		return nil, fmt.Errorf("%w: empty path", ErrNoPath)
	}
	var toks []pathTok
	i := 0
	for i < len(p) {
		switch {
		case p[i] == '.':
			if i == 0 || i == len(p)-1 || p[i+1] == '.' || p[i+1] == '[' {
				return nil, fmt.Errorf("%w: bad path %q", ErrNoPath, p)
			}
			i++
		case p[i] == '[':
			end := strings.IndexByte(p[i:], ']')
			if i+1 < len(p) && p[i+1] == '"' {
				// find the closing `"]`, allowing escaped quotes
				j := i + 2
				for j < len(p) && !(p[j] == '"' && p[j-1] != '\\' && j+1 < len(p) && p[j+1] == ']') {
					j++
				}
				if j >= len(p) {
					return nil, fmt.Errorf("%w: unclosed key in %q", ErrNoPath, p)
				}
				key, err := strconv.Unquote(p[i+1 : j+1])
				if err != nil {
					return nil, fmt.Errorf("%w: bad key in %q", ErrNoPath, p)
				}
				toks = append(toks, pathTok{kind: tokKey, name: key, start: i, end: j + 2})
				i = j + 2
				continue
			}
			if end < 0 {
				return nil, fmt.Errorf("%w: unclosed index in %q", ErrNoPath, p)
			}
			s := p[i+1 : i+end]
			if s == "" {
				// "[]" appears in schema paths only
				toks = append(toks, pathTok{kind: tokIndex, index: -1, start: i, end: i + end + 1})
				i += end + 1
				continue
			}
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%w: bad index %q in %q", ErrNoPath, s, p)
			}
			toks = append(toks, pathTok{kind: tokIndex, index: n, start: i, end: i + end + 1})
			i += end + 1
		default:
			j := i
			for j < len(p) && p[j] != '.' && p[j] != '[' {
				j++
			}
			toks = append(toks, pathTok{kind: tokName, name: p[i:j], start: i, end: j})
			i = j
		}
	}
	return toks, nil
}

// genericPath turns a concrete path into its schema path
// ("agents[2].command" → "agents[].command", "prices.x.in" → "prices.*.in").
func genericPath(p string) (string, error) {
	toks, err := parsePath(p)
	if err != nil {
		return "", err
	}
	t := reflect.TypeOf(Config{})
	gen := ""
	for i := 0; i < len(toks); {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		tk := toks[i]
		switch t.Kind() {
		case reflect.Struct:
			if t == timeType || tk.kind != tokName {
				return "", fmt.Errorf("%w: %q", ErrNoPath, p)
			}
			idx, ok := fieldByJSON(t, tk.name)
			if !ok {
				return "", fmt.Errorf("%w: %q (no %q here)", ErrNoPath, p, tk.name)
			}
			gen = joinPath(gen, tk.name)
			t = t.Field(idx).Type
			i++
		case reflect.Slice:
			if tk.kind != tokIndex {
				return "", fmt.Errorf("%w: %q (%s is a list; use [n])", ErrNoPath, p, gen)
			}
			gen += "[]"
			t = t.Elem()
			i++
		case reflect.Map:
			_, next, err := mapKey(t.Elem(), p, toks, i)
			if err != nil {
				return "", err
			}
			gen += ".*"
			t = t.Elem()
			i = next
		default:
			return "", fmt.Errorf("%w: %q (%s has no parts)", ErrNoPath, p, gen)
		}
	}
	return gen, nil
}

// fieldByJSON finds the struct field index for a JSON key.
func fieldByJSON(t reflect.Type, name string) (int, bool) {
	for i := 0; i < t.NumField(); i++ {
		if jsonName(t.Field(i)) == name {
			return i, true
		}
	}
	return 0, false
}

// walk follows toks from v and calls fn with the target value and its
// schema path. With create, missing list slots (index == len), map keys and
// nil pointers are made, and map values are written back after fn, so fn
// may modify the target; v must then be addressable. Without create the
// target may be unaddressable and nothing is changed.
func walk(v reflect.Value, path string, toks []pathTok, create bool, fn func(reflect.Value, string) error) error {
	return walkFrom(v, path, toks, 0, "", create, fn)
}

func walkFrom(v reflect.Value, path string, toks []pathTok, i int, gen string, create bool, fn func(reflect.Value, string) error) error {
	if i == len(toks) {
		return fn(v, gen)
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			if !create {
				return walkFrom(reflect.Zero(v.Type().Elem()), path, toks, i, gen, false, fn)
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	t := toks[i]
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType || t.kind != tokName {
			return fmt.Errorf("%w: %q", ErrNoPath, path)
		}
		idx, ok := fieldByJSON(v.Type(), t.name)
		if !ok {
			return fmt.Errorf("%w: %q (no %q here)", ErrNoPath, path, t.name)
		}
		return walkFrom(v.Field(idx), path, toks, i+1, joinPath(gen, t.name), create, fn)
	case reflect.Slice:
		if t.kind != tokIndex {
			return fmt.Errorf("%w: %q (%s is a list; use [n])", ErrNoPath, path, gen)
		}
		n := t.index
		switch {
		case n < 0:
			return fmt.Errorf("%w: %q (give an index, not [])", ErrNoPath, path)
		case n < v.Len():
		case n == v.Len() && create:
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		default:
			return fmt.Errorf("%w: %q (index %d out of range, %s has %d)", ErrNoPath, path, n, gen, v.Len())
		}
		return walkFrom(v.Index(n), path, toks, i+1, gen+"[]", create, fn)
	case reflect.Map:
		key, next, err := mapKey(v.Type().Elem(), path, toks, i)
		if err != nil {
			return err
		}
		gen += ".*"
		kv := reflect.ValueOf(key).Convert(v.Type().Key())
		cur := v.MapIndex(kv)
		if !cur.IsValid() && !create {
			return fmt.Errorf("%w: %q (no key %q)", ErrNoPath, path, key)
		}
		cp := reflect.New(v.Type().Elem()).Elem()
		if cur.IsValid() {
			cp.Set(cur)
		}
		if err := walkFrom(cp, path, toks, next, gen, create, fn); err != nil {
			return err
		}
		if create {
			if v.IsNil() {
				v.Set(reflect.MakeMap(v.Type()))
			}
			v.SetMapIndex(kv, cp)
		}
		return nil
	}
	return fmt.Errorf("%w: %q (%s has no parts)", ErrNoPath, path, gen)
}

// mapKey reads the map key starting at toks[i] and returns the index of the
// first token after it. A quoted key is one token. An unquoted key runs to
// the end of the path for scalar values; for struct values it runs up to the
// last token when that names a field of the struct (so "prices.a.b.in" is key
// "a.b", field "in").
func mapKey(elem reflect.Type, path string, toks []pathTok, i int) (string, int, error) {
	t := toks[i]
	switch t.kind {
	case tokKey:
		return t.name, i + 1, nil
	case tokIndex:
		return "", 0, fmt.Errorf("%w: %q (a map takes a key, not [n])", ErrNoPath, path)
	}
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	last := len(toks) - 1
	if elem.Kind() == reflect.Struct && elem != timeType {
		for k := i + 1; k <= last; k++ {
			if toks[k].kind == tokName {
				if _, ok := fieldByJSON(elem, toks[k].name); ok {
					return path[t.start : toks[k].start-1], k, nil
				}
			}
		}
	}
	if elem.Kind() == reflect.Struct || elem.Kind() == reflect.Slice || elem.Kind() == reflect.Map {
		// key is one name; the rest walks into the value
		return t.name, i + 1, nil
	}
	return path[t.start:], len(toks), nil
}

// lookupMeta returns the metadata for a schema path.
func lookupMeta(gen string) (meta, bool) {
	m, ok := metaTable[gen]
	return m, ok
}

// Get returns the value at path (a copy for lists, maps and structs).
// Sensitive settings return ErrSensitive.
func (c *Config) Get(path string) (any, error) {
	toks, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	var out any
	tokenMu.RLock()
	defer tokenMu.RUnlock()
	err = walk(reflect.ValueOf(c).Elem(), path, toks, false, func(v reflect.Value, gen string) error {
		if m, ok := lookupMeta(gen); ok && m.sensitive {
			return fmt.Errorf("%w: %s", ErrSensitive, path)
		}
		if strings.HasPrefix(path, "server") && containsSensitive(v) {
			return fmt.Errorf("%w: %s holds the token", ErrSensitive, path)
		}
		out = deepCopyValue(v)
		return nil
	})
	return out, err
}

// containsSensitive reports whether v is the whole ServerConfig (which holds
// the token); Get on "server" would otherwise leak it.
func containsSensitive(v reflect.Value) bool {
	return v.IsValid() && v.Type() == reflect.TypeOf(ServerConfig{})
}

func deepCopyValue(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Map, reflect.Struct, reflect.Pointer:
		if v.Type() == timeType {
			return v.Interface()
		}
		cp := reflect.New(v.Type())
		raw, err := json.Marshal(v.Interface())
		if err == nil && json.Unmarshal(raw, cp.Interface()) == nil {
			return cp.Elem().Interface()
		}
	}
	return v.Interface()
}

// Set changes the setting at path to value, type-checked (value may be the
// Go type, a decoded JSON value, or a string as typed on the command line)
// and validated: it fails, changing nothing, when the new value is invalid
// or the change brings a new validation error. A list index equal to the
// list length appends. Set does not save.
func (c *Config) Set(path string, value any) error {
	probs, err := c.SetAll(map[string]any{path: value})
	if err != nil && errors.Is(err, ErrInvalid) {
		var errs []error
		for _, p := range probs {
			if p.Level == LevelError {
				errs = append(errs, fmt.Errorf("%s: %s", p.Path, p.Message))
			}
		}
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return err
}

// SetAll applies several changes atomically: all of them or none. A type or
// path error returns that error and no problems. When the result has new
// validation errors (errors not already present before), it returns those
// problems with ErrInvalid and changes nothing. On success it returns the
// problems that remain (warnings, or errors that were already there).
func (c *Config) SetAll(changes map[string]any) ([]Problem, error) {
	return c.SetAllWith(changes, ValidateOptions{})
}

// SetAllWith is SetAll with validation options (a vault lookup).
func (c *Config) SetAllWith(changes map[string]any, opts ValidateOptions) ([]Problem, error) {
	n := c.Clone()
	paths := make([]string, 0, len(changes))
	for p := range changes {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	var direct []Problem
	for _, p := range paths {
		probs, err := n.setOne(p, changes[p], opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		direct = append(direct, probs...)
	}
	after := n.ValidateWith(opts)
	bad := newErrors(c.ValidateWith(opts), direct, after)
	if len(bad) > 0 {
		return bad, ErrInvalid
	}
	c.adopt(n)
	return after, nil
}

// listIndex matches a list index in a path or message ("agents[2]").
var listIndex = regexp.MustCompile(`\[\d+\]`)

// newErrors returns the errors an edit brings: the errors of the result
// (field checks of the values set, then the whole config) that were not
// already there before. Errors are matched by position-free key (list
// indices removed from path and message) and counted, so deleting or
// moving list items in front of an item that was already invalid does not
// make its error look new: an error is new only when its key appears more
// often than before.
func newErrors(before, direct, after []Problem) []Problem {
	key := func(p Problem) string {
		return listIndex.ReplaceAllString(p.Path, "[]") + "\x00" + listIndex.ReplaceAllString(p.Message, "[]")
	}
	had := map[string]int{}
	exact := map[string]bool{}
	for _, p := range before {
		if p.Level == LevelError {
			had[key(p)]++
			exact[p.Path+"\x00"+p.Message] = true
		}
	}
	// The result's errors, each once (a field check and the whole-config
	// check may report the same problem).
	var errs []Problem
	seen := map[string]bool{}
	for _, list := range [][]Problem{after, direct} {
		for _, p := range list {
			k := p.Path + "\x00" + p.Message
			if p.Level == LevelError && !seen[k] {
				seen[k] = true
				errs = append(errs, p)
			}
		}
	}
	now := map[string]int{}
	for _, p := range errs {
		now[key(p)]++
	}
	var bad []Problem
	for _, p := range errs {
		k := key(p)
		if now[k] <= had[k] {
			continue
		}
		// More of this kind than before: the new ones are those not
		// already present at the same place (a rise means there is one).
		if !exact[p.Path+"\x00"+p.Message] {
			bad = append(bad, p)
		}
	}
	slices.SortStableFunc(bad, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	return bad
}

// setOne assigns one path in c (a clone) and returns the field-level
// problems of the new value.
func (c *Config) setOne(path string, value any, opts ValidateOptions) ([]Problem, error) {
	toks, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	var probs []Problem
	err = walk(reflect.ValueOf(c).Elem(), path, toks, true, func(v reflect.Value, gen string) error {
		m, _ := lookupMeta(gen)
		if m.sensitive {
			return ErrSensitive
		}
		if m.readOnly {
			return ErrReadOnly
		}
		if v.Type() == reflect.TypeOf(ServerConfig{}) {
			return fmt.Errorf("%w: set server settings one by one", ErrSensitive)
		}
		if err := assign(v, m, value); err != nil {
			return err
		}
		probs = checkTree(v, gen, path, opts)
		return nil
	})
	return probs, err
}

// assign converts value to v's type and stores it.
func assign(v reflect.Value, m meta, value any) error {
	t := v.Type()
	if value == nil {
		v.Set(reflect.Zero(t))
		return nil
	}
	if s, ok := value.(string); ok && t.Kind() != reflect.String {
		if m.kind == KindDuration && isIntKind(t.Kind()) {
			if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
				v.SetInt(int64(d / time.Second))
				return nil
			}
		}
		if t.Kind() == reflect.Bool {
			if b, ok := parseBoolWord(s); ok {
				v.SetBool(b)
				return nil
			}
		}
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String && !strings.HasPrefix(strings.TrimSpace(s), "[") {
			return fmt.Errorf("%w: %s is a list; give a JSON array such as [\"a\",\"b\"]", ErrType, describe(t))
		}
		return decodeInto(v, []byte(s))
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrType, err)
	}
	return decodeInto(v, raw)
}

func decodeInto(v reflect.Value, raw []byte) error {
	cp := reflect.New(v.Type())
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cp.Interface()); err != nil {
		return fmt.Errorf("%w: want %s: %v", ErrType, describe(v.Type()), err)
	}
	if dec.More() {
		return fmt.Errorf("%w: want %s: trailing data", ErrType, describe(v.Type()))
	}
	v.Set(cp.Elem())
	return nil
}

func describe(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Slice:
		return "a list"
	case reflect.Map:
		return "an object of keys"
	case reflect.Struct:
		return "an object"
	}
	if isIntKind(t.Kind()) {
		return "a whole number"
	}
	return t.String()
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func parseBoolWord(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	}
	return false, false
}

// Reset sets path back to its default (from Defaults). Validation applies as
// for Set.
func (c *Config) Reset(path string) error {
	toks, err := parsePath(path)
	if err != nil {
		return err
	}
	var def any
	err = walk(reflect.ValueOf(Defaults()).Elem(), path, toks, false, func(v reflect.Value, _ string) error {
		def = deepCopyValue(v)
		return nil
	})
	if errors.Is(err, ErrNoPath) && strings.ContainsAny(path, "[") {
		// an item that has no default counterpart resets to empty
		return c.Set(path, nil)
	}
	if err != nil {
		return err
	}
	return c.Set(path, def)
}

// adopt copies n into c, keeping c's file path and its current token (a
// rotation may have happened since n was cloned).
func (c *Config) adopt(n *Config) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	tok, created, p := c.Server.Token, c.Server.TokenCreated, c.path
	*c = *n
	c.Server.Token, c.Server.TokenCreated, c.path = tok, created, p
}
