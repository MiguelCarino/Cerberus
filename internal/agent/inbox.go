package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"carino.systems/cerberus/internal/files/fsafe"
	"carino.systems/cerberus/internal/platform"
)

// Inbox (day phase 2, File drop): files people drop into a session land in
// <cwd>/<inbox>. At the start of every run (a new task, a follow-up or a
// resume) the agent tells the model which inbox files are new since its last
// run, and attaches new images as image parts when the model may take them.
// What was already announced is kept in $CERBERUS_SESSION_DIR/inbox-seen.json.

const (
	inboxSeenFile  = "inbox-seen.json"
	inboxMaxFiles  = 500     // files looked at in the inbox
	inboxListed    = 30      // files named in the note; the rest are counted
	imageMaxBytes  = 5 << 20 // larger images are only mentioned
	imageMaxCount  = 4       // images attached per run
	visionProbeFor = 5 * time.Second
)

// imageTypes are the image formats vision models commonly accept.
var imageTypes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp"}

// inboxFile is one new file in the inbox.
type inboxFile struct {
	Rel  string // relative to the inbox, slash separated
	Size int64
	Mod  time.Time
}

// stamp identifies a version of a file in the seen ledger.
func (f inboxFile) stamp() string { return fmt.Sprintf("%d:%d", f.Size, f.Mod.UnixNano()) }

// scanInbox lists the regular files under cwd/inbox (no symlinks, no dot
// folders, nothing deny_paths protects) and returns those that are new or
// changed since the ledger at seenPath, recording them there at once. With
// seenPath "" (a run outside a session) every file counts as new. The agent
// itself uses peekInbox and writes the ledger only once the model got the
// note (commitInbox).
func scanInbox(cwd, inbox, seenPath string, deny []string) []inboxFile {
	fresh, ledger := peekInbox(cwd, inbox, seenPath, deny)
	commitInbox(seenPath, ledger)
	return fresh
}

// peekInbox is scanInbox without writing the ledger: it returns the new
// files and the ledger to write once they were announced (nil when nothing
// is new).
func peekInbox(cwd, inbox, seenPath string, deny []string) ([]inboxFile, map[string]string) {
	if inbox == "" {
		return nil, nil
	}
	dir := filepath.Join(cwd, filepath.FromSlash(inbox))
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		return nil, nil
	}
	seen := map[string]string{}
	if seenPath != "" {
		if raw, err := os.ReadFile(seenPath); err == nil {
			_ = json.Unmarshal(raw, &seen)
		}
	}
	var all, fresh []inboxFile
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") || DeniedBy(deny, p) != "" {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		f := inboxFile{Rel: filepath.ToSlash(rel), Size: st.Size(), Mod: st.ModTime()}
		all = append(all, f)
		if seen[f.Rel] != f.stamp() {
			fresh = append(fresh, f)
		}
		if len(all) >= inboxMaxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	var ledger map[string]string
	if len(fresh) > 0 {
		ledger = map[string]string{}
		for _, f := range all {
			ledger[f.Rel] = f.stamp()
		}
	}
	slices.SortFunc(fresh, func(a, b inboxFile) int { return strings.Compare(a.Rel, b.Rel) })
	return fresh, ledger
}

// commitInbox writes ledger to seenPath (nothing when either is empty).
func commitInbox(seenPath string, ledger map[string]string) {
	if seenPath == "" || ledger == nil {
		return
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(seenPath), 0o700); err != nil {
		return
	}
	_ = platform.WritePrivate(seenPath, raw)
}

// inboxNote is the sentence telling the model about new files:
// "The user added files to ./inbox: a.png (34 KB), notes.txt (2 KB)".
func inboxNote(inbox string, files []inboxFile, attached int) string {
	if len(files) == 0 {
		return ""
	}
	names := make([]string, 0, min(len(files), inboxListed))
	for i, f := range files {
		if i == inboxListed {
			break
		}
		names = append(names, f.Rel+" ("+fsafe.HumanSize(f.Size)+")")
	}
	note := "The user added files to ./" + inbox + ": " + strings.Join(names, ", ")
	if extra := len(files) - len(names); extra > 0 {
		note += fmt.Sprintf(" and %d more", extra)
	}
	note += "."
	switch {
	case attached == 1:
		note += " The image is attached."
	case attached > 1:
		note += fmt.Sprintf(" %d images are attached.", attached)
	}
	return note
}

// loadImages reads new image files (at most imageMaxCount, each at most
// imageMaxBytes) as data: URLs. It opens them through an os.Root on cwd, so
// a symlink swapped in after the scan cannot lead outside.
func loadImages(cwd, inbox string, files []inboxFile) []string {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil
	}
	defer root.Close()
	var out []string
	for _, f := range files {
		if len(out) == imageMaxCount {
			break
		}
		mt := imageTypes[strings.ToLower(path.Ext(f.Rel))]
		if mt == "" || f.Size <= 0 || f.Size > imageMaxBytes {
			continue
		}
		fh, err := root.Open(filepath.FromSlash(path.Join(inbox, f.Rel)))
		if err != nil {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(fh, imageMaxBytes+1))
		fh.Close()
		if err != nil || len(raw) > imageMaxBytes {
			continue
		}
		out = append(out, "data:"+mt+";base64,"+base64.StdEncoding.EncodeToString(raw))
	}
	return out
}

// visionKnown is what the server's model list says about images.
type visionKnown int

const (
	visionUnknown visionKnown = iota // not listed, or a remote model: try, and fall back on refusal
	visionYes
	visionNo // a local model the gateway says takes no images
)

// modelVision asks GET {base}/models whether the model takes images.
// Cerberus' gateway marks local vision models "vision": true; a local model
// without it takes none. Anything else is unknown.
func (c *client) modelVision(ctx context.Context, model string) visionKnown {
	ctx, cancel := context.WithTimeout(ctx, visionProbeFor)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
	if err != nil {
		return visionUnknown
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return visionUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return visionUnknown
	}
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Local  bool   `json:"local"`
			Vision *bool  `json:"vision"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list) != nil {
		return visionUnknown
	}
	for _, m := range list.Data {
		if m.ID != model {
			continue
		}
		switch {
		case m.Vision != nil && *m.Vision:
			return visionYes
		case m.Local:
			return visionNo
		}
	}
	return visionUnknown
}

// imageRefusal reports whether err looks like the server refusing image
// parts: it says so (image, vision, multimodal, image_url, content parts),
// or a plain 400/415/422/500 that blames nothing else (context size, auth,
// rate limits never count).
func imageRefusal(err error) bool {
	var se *ServerError
	if !errors.As(err, &se) {
		return false
	}
	text := strings.ToLower(se.Message)
	switch {
	case se.Status == 401 || se.Status == 403 || se.Status == 429 || reNotTools.MatchString(text):
		return false
	case strings.Contains(text, "image"), strings.Contains(text, "vision"), strings.Contains(text, "multimodal"),
		strings.Contains(text, "content part"), strings.Contains(text, "array"):
		return true
	}
	switch se.Status {
	case 400, 415, 422, 500:
		return true
	}
	return false
}

// dropImages removes every image part from msgs, telling the model in each
// such message that the images could not be shown. It reports whether
// there were any.
func dropImages(msgs []message) bool {
	found := false
	for i := range msgs {
		if len(msgs[i].Images) == 0 {
			continue
		}
		n := len(msgs[i].Images)
		msgs[i].Images = nil
		text := reAttached.ReplaceAllString(msgs[i].Content, "")
		msgs[i].Content = strings.TrimSpace(text + "\n\n" + fmt.Sprintf(
			"(%d image%s could not be shown to you: this model takes no images. Read the files from the inbox if you need them.)",
			n, plural(n)))
		found = true
	}
	return found
}

// reAttached is the sentence inboxNote adds about attached images.
var reAttached = regexp.MustCompile(` (The image is|\d+ images are) attached\.`)

// hasImages reports whether any message carries image parts.
func hasImages(msgs []message) bool {
	return slices.ContainsFunc(msgs, func(m message) bool { return len(m.Images) > 0 })
}
