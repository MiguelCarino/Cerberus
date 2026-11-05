package agent

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxLocalFiles matches the daemon's limit on files per ask.
const maxLocalFiles = 8

// localFiles reads cerberus_ask's files on the client's side, for an MCP
// client (`cerberus mcp --client`): it has no workspace on the daemon, and
// reading here keeps the files' text out of the calling agent's context,
// which is the point of handing a job to a local model.
type localFiles struct {
	root   string   // relative paths start here (the MCP server's folder)
	deny   []string // protected paths (agent.deny_paths and the config files)
	maxKiB int      // each file is cut at this size
}

// LocalFiles makes cerberus_ask read its files here: relative paths from
// root, absolute ones as given, text files only, each cut at maxKiB, and
// never a path that deny protects. It returns c.
func (c *SpawnClient) LocalFiles(root string, deny []string, maxKiB int) *SpawnClient {
	c.local = &localFiles{root: root, deny: deny, maxKiB: max(maxKiB, 1)}
	return c
}

// read returns the files as the daemon's own ask would add them: each
// under a "--- path ---" line.
func (l *localFiles) read(names []string) (string, error) {
	if len(names) > maxLocalFiles {
		return "", fmt.Errorf("at most %d files per ask", maxLocalFiles)
	}
	limit := int64(l.maxKiB) << 10
	var b strings.Builder
	for _, name := range names {
		name = strings.TrimSpace(name)
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(l.root, p)
		}
		p = filepath.Clean(p)
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("%q: no such file", name)
			}
			return "", fmt.Errorf("%q: %v", name, err)
		}
		for _, q := range []string{p, real} {
			if pat := DeniedBy(l.deny, q); pat != "" {
				return "", fmt.Errorf("%q is a protected path (%s)", name, pat)
			}
		}
		f, err := os.Open(real)
		if err != nil {
			return "", fmt.Errorf("%q: %v", name, err)
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			f.Close()
			return "", fmt.Errorf("%q is not a file", name)
		}
		raw, err := io.ReadAll(io.LimitReader(f, limit+1))
		f.Close()
		if err != nil {
			return "", fmt.Errorf("%q: %v", name, err)
		}
		cut := int64(len(raw)) > limit
		if cut {
			raw = raw[:limit]
		}
		if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(bytes.ToValidUTF8(raw, nil)) {
			return "", fmt.Errorf("%q is not a text file", name)
		}
		fmt.Fprintf(&b, "\n\n--- %s", filepath.ToSlash(name))
		if cut {
			fmt.Fprintf(&b, " (first %d KiB of %d)", l.maxKiB, fi.Size()>>10)
		}
		b.WriteString(" ---\n")
		b.Write(bytes.ToValidUTF8(raw, []byte("\uFFFD")))
	}
	return b.String(), nil
}
