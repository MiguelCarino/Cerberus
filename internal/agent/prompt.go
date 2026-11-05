package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// repoFileMax caps AGENTS.md / CLAUDE.md: they are the project's own rules,
// so they get more room than the extra context files.
const repoFileMax = 64 * 1024

// contextDoc is one file whose text goes into the system prompt.
type contextDoc struct{ title, text string }

// loadContext reads AGENTS.md and CLAUDE.md from the repo root, then each
// configured extra file clipped to maxBytes. Missing files are skipped.
func loadContext(cwd string, extra []string, maxBytes int) []contextDoc {
	root := repoRoot(cwd)
	var docs []contextDoc
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		if text, ok := readClipped(filepath.Join(root, name), repoFileMax); ok {
			docs = append(docs, contextDoc{"Project instructions (" + name + ")", text})
		}
	}
	for _, f := range extra {
		p := resolve(f, cwd)
		if text, ok := readClipped(p, maxBytes); ok {
			docs = append(docs, contextDoc{"Extra context (" + p + ")", text})
		}
	}
	return docs
}

// repoRoot is the nearest folder at or above cwd holding .git (cwd if none).
func repoRoot(cwd string) string {
	for d := cwd; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return cwd
		}
		d = parent
	}
}

func readClipped(p string, maxBytes int) (string, bool) {
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	text := string(raw)
	if len(raw) > maxBytes {
		text = strings.ToValidUTF8(text[:maxBytes], "") + fmt.Sprintf("\n[... clipped at %d bytes ...]", maxBytes)
	}
	return strings.TrimSpace(text), true
}

// systemPrompt is rebuilt per request: tools come and go (fallback, last step).
func systemPrompt(cwd, approve string, tools bool, docs []contextDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are Cerberus' built-in coding agent, running headless in a terminal. Today is %s. "+
		"The working directory is %s: a disposable git worktree of the project. The user reviews your changes "+
		"there and accepts or discards them, so do not commit, create branches or push.",
		time.Now().Format("Monday 2006-01-02 15:04"), cwd)
	if tools {
		b.WriteString(" You have tools to read files, list folders, search, run bash commands (non-interactive) " +
			"and write or edit files; paths are relative to the working directory. You may read anywhere except " +
			"protected paths, and write only inside the working directory. Work on your own: nobody answers " +
			"questions mid-task. Read a file before editing it, prefer edit_file for small changes, and run the " +
			"project's tests or build when there are any.")
		if approve == "ask" {
			b.WriteString(" Commands and file changes need the user's approval. If one is refused, respect " +
				"that and do not retry it; take any reason given into account.")
		}
	} else {
		b.WriteString(" No tools are available in this conversation: answer with what you know, and give " +
			"file changes as complete code blocks with their paths.")
	}
	b.WriteString(" When you are done, reply with a short summary of what you changed and anything left to do.")
	for _, d := range docs {
		fmt.Fprintf(&b, "\n\n# %s\n\n%s", d.title, d.text)
	}
	return b.String()
}
