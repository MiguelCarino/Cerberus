package evals

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// skipDirs are folders left out of the file evidence: version control,
// dependencies and caches, never the agent's own work.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".cache": true, ".next": true, ".nuxt": true, ".tox": true, ".mypy_cache": true,
	".pytest_cache": true, ".gradle": true, ".idea": true, ".vscode": true, "target": true, ".terraform": true,
	".cerberus": true,
}

// Bounds of the file walk.
const (
	maxWalkFiles   = 5000
	maxFileRead    = 256 << 10
	treeShareNum   = 1 // the tree takes at most a third of the files share
	treeShareDen   = 3
	minSectionSize = 256
)

// evidenceInput is what one evidence build reads. Transcript and Diff are
// read once per session and shared by its criteria.
type evidenceInput struct {
	Info       SessionInfo
	Transcript string
	Diff       string
	Mask       func(string) string
}

type walked struct {
	rel  string
	size int64
	mod  int64
}

// walkWorkspace lists the regular files of a workspace (no symlinks, the
// skipped folders left out), newest first within the cap.
func walkWorkspace(root string) []walked {
	if root == "" {
		return nil
	}
	var out []walked
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices: an agent could point one anywhere
		}
		if len(out) >= maxWalkFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		out = append(out, walked{rel: filepath.ToSlash(rel), size: info.Size(), mod: info.ModTime().UnixNano()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// readText reads a regular file (never through a symlink) when it looks
// like text, up to max bytes.
func readText(root, rel string, max int) (string, bool) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if max > maxFileRead {
		max = maxFileRead
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(max)))
	if err != nil {
		return "", false
	}
	if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return "", false
	}
	if !utf8.Valid(b) {
		// Cut mid-rune at the cap is fine; invalid elsewhere is binary.
		trimmed := b
		for i := 0; i < 3 && len(trimmed) > 0 && !utf8.Valid(trimmed); i++ {
			trimmed = trimmed[:len(trimmed)-1]
		}
		if !utf8.Valid(trimmed) {
			return "", false
		}
		b = trimmed
	}
	return string(b), true
}

// mentioned reports whether the criterion's text names a path or its file
// name (case-insensitive; names shorter than 3 characters do not count).
func mentioned(text, rel string) bool {
	t := strings.ToLower(text)
	r := strings.ToLower(rel)
	if strings.Contains(t, r) {
		return true
	}
	base := strings.ToLower(filepath.Base(rel))
	if len(base) < 3 {
		return false
	}
	i := strings.Index(t, base)
	for i >= 0 {
		before := i == 0 || !isNameByte(t[i-1])
		end := i + len(base)
		after := end >= len(t) || !isNameByte(t[end])
		if before && after {
			return true
		}
		next := strings.Index(t[i+1:], base)
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}

func isNameByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' || c == '/' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// filesEvidence: the tree (paths and sizes), then the text files the
// criterion mentions, then the newest-changed files, until max bytes.
func filesEvidence(root, criterion string, max int, mask func(string) string) string {
	files := walkWorkspace(root)
	if len(files) == 0 {
		return "(the workspace has no files)"
	}
	var b strings.Builder
	treeMax := max * treeShareNum / treeShareDen
	fmt.Fprintf(&b, "File tree (%d files):\n", len(files))
	for i, f := range files {
		line := fmt.Sprintf("%s (%d bytes)\n", f.rel, f.size)
		if b.Len()+len(line) > treeMax {
			fmt.Fprintf(&b, "... and %d more files\n", len(files)-i)
			break
		}
		b.WriteString(line)
	}
	var pick []walked
	seen := map[string]bool{}
	for _, f := range files {
		if mentioned(criterion, f.rel) {
			pick = append(pick, f)
			seen[f.rel] = true
		}
	}
	byNew := append([]walked(nil), files...)
	sort.SliceStable(byNew, func(i, j int) bool { return byNew[i].mod > byNew[j].mod })
	for _, f := range byNew {
		if !seen[f.rel] {
			pick = append(pick, f)
		}
	}
	for _, f := range pick {
		room := max - b.Len()
		header := fmt.Sprintf("\n--- %s ---\n", f.rel)
		if room < len(header)+minSectionSize/2 {
			break
		}
		// Read the whole file (up to maxFileRead), mask it, and only then
		// cut it to the room left: a secret that straddles the cut is
		// masked as a whole first, never sent half visible.
		text, ok := readText(root, f.rel, maxFileRead)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		cut := int64(len(text)) < f.size
		if cut {
			// Read short of the file's end: drop the last partial line, so
			// a value the read cap cut through is not sent in part.
			if i := strings.LastIndexByte(text, '\n'); i >= 0 {
				text = text[:i+1]
			} else {
				continue
			}
		}
		masked := mask(text)
		text = clipBytes(masked, room-len(header))
		cut = cut || len(text) < len(masked)
		b.WriteString(header)
		b.WriteString(text)
		if cut {
			b.WriteString("\n[... cut]")
		}
	}
	return clipBytes(b.String(), max)
}

// clipBytes cuts s to at most n bytes on a rune boundary.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// tailBytes keeps the last n bytes of s, from a line start when one is
// close.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "[... earlier output cut]\n"
	n -= len(marker)
	if n <= 0 {
		return ""
	}
	t := s[len(s)-n:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	if i := strings.IndexByte(t, '\n'); i >= 0 && i < 200 {
		t = t[i+1:]
	}
	return marker + t
}

// buildEvidence is the evidence text of one criterion: the kinds it asks
// for in a fixed order, each with its own heading, all within max bytes
// (a kind that needs less leaves its share to the next). Secrets are
// masked before anything is cut, so a cut never leaves half a secret.
func buildEvidence(in evidenceInput, c Criterion, max int) string {
	kinds := EvidenceOf(c)
	if max < minSectionSize {
		max = minSectionSize
	}
	mask := in.Mask
	if mask == nil {
		mask = func(s string) string { return s }
	}
	var b strings.Builder
	left := max
	for i, k := range kinds {
		share := left / (len(kinds) - i)
		var head, body string
		switch k {
		case EvidenceFiles:
			head = "== files =="
			body = filesEvidence(in.Info.Worktree, c.Prompt, max0(share-len(head)-2), mask)
		case EvidenceChecks:
			head = "== checks =="
			body = strings.TrimSpace(mask(in.Info.Checks))
			if body == "" {
				body = "(no checks output: the project has no checks, or they did not run)"
			}
			body = tailBytes(body, max0(share-len(head)-2))
		case EvidenceTranscript:
			head = "== transcript (end) =="
			body = strings.TrimSpace(mask(in.Transcript))
			if body == "" {
				body = "(no transcript)"
			}
			body = tailBytes(body, max0(share-len(head)-2))
		case EvidenceDiff:
			head = "== diff =="
			body = strings.TrimSpace(mask(in.Diff))
			if in.Info.Workspace != WorkspaceRepo {
				body = "(not a repo session: no diff)"
			} else if body == "" {
				body = "(no changes against the base commit)"
			}
			body = clipBytes(body, max0(share-len(head)-2))
		}
		part := head + "\n" + body + "\n"
		if i > 0 {
			part = "\n" + part
		}
		part = clipBytes(part, share)
		b.WriteString(part)
		left -= len(part)
	}
	return b.String()
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
