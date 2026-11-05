package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// denyList holds DenyPaths globs split into slash segments. "**" matches any
// number of segments (zero included); every other segment is a path.Match
// pattern. A pattern that is neither absolute nor starts with "**" may match
// anywhere, as if prefixed with "**/". "~" expands to the home folder.
type denyList struct {
	raw  []string
	segs [][]string
}

func newDenyList(patterns []string) denyList {
	home, _ := os.UserHomeDir()
	var d denyList
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		q := p
		if q == "~" || strings.HasPrefix(q, "~/") {
			q = home + q[1:]
		}
		q = filepath.ToSlash(q)
		if !filepath.IsAbs(filepath.FromSlash(q)) && !strings.HasPrefix(q, "/") && !strings.HasPrefix(q, "**") {
			q = "**/" + q
		}
		d.raw = append(d.raw, p)
		d.segs = append(d.segs, splitSegs(q))
	}
	return d
}

// DeniedBy returns the first of patterns (deny_paths globs, as the built-in
// agent reads them) that abs matches, or "" if none does.
func DeniedBy(patterns []string, abs string) string {
	return newDenyList(patterns).match(abs)
}

// match returns the first pattern abs matches ("" if none).
func (d denyList) match(abs string) string {
	segs := splitSegs(filepath.ToSlash(abs))
	for i, pat := range d.segs {
		if matchSegs(pat, segs) {
			return d.raw[i]
		}
	}
	return ""
}

func splitSegs(p string) []string {
	parts := strings.Split(p, "/")
	out := parts[:0]
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return true
			}
			for i := 0; i < len(segs); i++ {
				if matchSegs(pat, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// privateArea is a set of folders (roots) the agent may not touch, except
// for the folders in own that lie inside them.
type privateArea struct {
	roots, own []string
}

// resolved adds each folder's symlink-resolved form, so a path reached
// through a link (or a data folder behind one) is still recognised.
func (a privateArea) resolved() privateArea {
	add := func(list []string) []string {
		out := slices.Clone(list)
		for _, p := range list {
			if r := realPath(p); r != p {
				out = append(out, r)
			}
		}
		return out
	}
	return privateArea{roots: add(a.roots), own: add(a.own)}
}

// hides reports whether p is inside a root and not inside an own folder.
func (a privateArea) hides(p string) bool {
	in := false
	for _, r := range a.roots {
		if under(p, r) {
			in = true
			break
		}
	}
	if !in {
		return false
	}
	for _, o := range a.own {
		if o != "" && under(p, o) {
			return false
		}
	}
	return true
}

// resolve makes a tool's path argument absolute and clean (relative to cwd).
func resolve(p, cwd string) string {
	p = strings.Trim(strings.TrimSpace(p), `"'`)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// realPath follows symlinks as far as the path exists, so a link inside the
// worktree cannot smuggle a write (or a denied read) somewhere else.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || filepath.Clean(dir) == p {
		return p
	}
	return filepath.Join(realPath(filepath.Clean(dir)), base)
}

// under reports whether p is folder or inside it.
func under(p, folder string) bool {
	if p == folder {
		return true
	}
	rel, err := filepath.Rel(folder, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// clip shortens text to limit characters, keeping the head and the tail.
func clip(text string, limit int) string {
	if len(text) <= limit || utf8.RuneCountInString(text) <= limit {
		return text
	}
	r := []rune(text)
	head := limit * 2 / 3
	tail := limit - head
	return string(r[:head]) + fmt.Sprintf("\n[... %d characters cut ...]\n", len(r)-limit) + string(r[len(r)-tail:])
}

// truncRunes cuts s to at most n characters.
func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
