// Package fsafe holds the pure, dependency-free rules for files dropped into
// Cerberus: name sanitising (with a minimal NFC), collision naming, the
// secret-ish name test that picks 0600, relative-path cleaning, staging ids
// and moving staged files into a session's inbox.
//
// It is a leaf package (stdlib only), so the session manager, the built-in
// agent and internal/files can all share one set of rules without import
// cycles.
package fsafe

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameBytes is the longest sanitised file name, in bytes.
const MaxNameBytes = 180

// ErrBadName is returned for a name that is empty (or only dots, spaces and
// control characters) once sanitised.
var ErrBadName = errors.New("refused name")

// ErrBadPath is returned for a relative path that leaves its root, is
// absolute, or holds NUL or a .git segment.
var ErrBadPath = errors.New("refused path")

// Name sanitises an uploaded file name:
//
//   - invalid UTF-8 becomes "_", then the name is composed to NFC (see NFC
//     for the limit);
//   - a path keeps only its last part ("../../etc/passwd" → "passwd",
//     `C:\Users\a\b.txt` → "b.txt");
//   - NUL, control characters, bidi overrides and BOMs are removed, and the
//     characters Windows refuses (<>:"|?*) become "_";
//   - leading dots and spaces, and trailing dots and spaces, are removed
//     (no hidden files, no "..");
//   - Windows device names (CON, NUL, COM1…) get a leading "_";
//   - the result is at most MaxNameBytes bytes, cut on a character boundary
//     while keeping a short extension.
//
// An empty result is ErrBadName.
func Name(name string) (string, error) {
	s := strings.ToValidUTF8(name, "_")
	s = NFC(s)
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0, unicode.IsControl(r), isInvisible(r):
			continue
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	s = strings.TrimLeft(b.String(), ". \t")
	s = strings.TrimRight(s, ". \t")
	if s == "" {
		return "", ErrBadName
	}
	if reservedWindows(s) {
		s = "_" + s
	}
	return clipName(s, MaxNameBytes), nil
}

// isInvisible reports characters that only reorder or hide text: bidi
// embeddings, overrides and isolates, line/paragraph separators and the BOM.
func isInvisible(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F,
		r == 0x2028, r == 0x2029, r == 0xFEFF:
		return true
	}
	return false
}

// reservedWindows reports device names Windows refuses as file names, with
// or without an extension.
func reservedWindows(s string) bool {
	stem := strings.ToUpper(s)
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.TrimRight(stem, " ")
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return true
	}
	return false
}

// SplitExt splits name into stem and extension. Compressed tarballs keep both
// parts together ("a.tar.gz" → "a", ".tar.gz"); a name that is only an
// extension has none ("env" → "env", "").
func SplitExt(name string) (stem, ext string) {
	lower := strings.ToLower(name)
	for _, double := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, double) && len(name) > len(double) {
			return name[:len(name)-len(double)], name[len(name)-len(double):]
		}
	}
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return name, ""
	}
	return name[:i], name[i:]
}

// clipName cuts name to max bytes on a character boundary, keeping the
// extension when it is short (≤ 20 bytes).
func clipName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	stem, ext := SplitExt(name)
	if len(ext) > 20 {
		stem, ext = name, ""
	}
	return cutBytes(stem, max-len(ext)) + ext
}

// cutBytes cuts s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimRight(s[:n], " .")
}

// Unique returns name, or "stem (2).ext", "stem (3).ext"… for the first one
// exists reports false for. A name already ending in " (n)" counts on from
// n. It gives up (returning an error) after 10000 tries.
func Unique(name string, exists func(string) bool) (string, error) {
	if !exists(name) {
		return name, nil
	}
	stem, ext := SplitExt(name)
	n := 1
	if i := strings.LastIndex(stem, " ("); i > 0 && strings.HasSuffix(stem, ")") {
		if k, err := strconv.Atoi(stem[i+2 : len(stem)-1]); err == nil && k > 0 {
			stem, n = stem[:i], k
		}
	}
	for tries := 0; tries < 10000; tries++ {
		n++
		suffix := fmt.Sprintf(" (%d)", n)
		cand := cutBytes(stem, MaxNameBytes-len(ext)-len(suffix)) + suffix + ext
		if !exists(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("no free name for %q", name)
}

// Secretish reports whether a file name looks like it holds a secret (keys,
// certificates, .env files, SSH identities), so it is written 0600. Check the
// original name as well as the sanitised one: sanitising drops the leading
// dot of ".env".
func Secretish(name string) bool {
	n := strings.ToLower(path.Base(strings.ReplaceAll(name, `\`, "/")))
	n = strings.TrimLeft(n, ".")
	switch {
	case n == "env" || strings.HasPrefix(n, "env.") || strings.HasSuffix(n, ".env"):
		return true
	case strings.HasPrefix(n, "id_"), n == "netrc", n == "pgpass", n == "npmrc", n == "pypirc":
		return true
	case strings.HasPrefix(n, "credentials"), strings.HasPrefix(n, "secrets"):
		return true
	}
	switch strings.ToLower(path.Ext(n)) {
	case ".pem", ".key", ".p12", ".pfx", ".jks", ".keystore", ".kdbx", ".gpg", ".asc", ".ovpn", ".ppk":
		return true
	}
	return false
}

// CleanRel validates a slash-separated path relative to a workspace and
// returns it cleaned ("" for the root itself). ".." anywhere, an absolute
// path, a drive letter, NUL, a backslash or a ".git" segment are ErrBadPath:
// a path is refused, never silently clamped.
func CleanRel(rel string) (string, error) {
	if strings.ContainsAny(rel, "\x00\\") {
		return "", ErrBadPath
	}
	if strings.HasPrefix(rel, "/") || (len(rel) >= 2 && rel[1] == ':') {
		return "", ErrBadPath
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." || strings.EqualFold(seg, ".git") {
			return "", ErrBadPath
		}
	}
	c := path.Clean("/" + rel)[1:]
	return c, nil
}

// ValidStaging reports whether id is a staging id NewStagingID could have
// made (lowercase hex, 16 to 64 characters), so it is safe as a folder name.
func ValidStaging(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// HumanSize is a short size for notes: "512 B", "34 KB", "2.5 MB".
func HumanSize(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1000*1000:
		return strconv.FormatInt((n+500)/1000, 10) + " KB"
	case n < 1000*1000*1000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + " MB"
	}
	return strconv.FormatFloat(float64(n)/1e9, 'f', 1, 64) + " GB"
}
