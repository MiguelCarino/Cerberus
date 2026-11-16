package downloads

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Split GGUF sets. Hugging Face caps a file at 50 GB, so most large quants
// are split into model-00001-of-0000N.gguf ... model-0000N-of-0000N.gguf,
// and llama-server needs every file beside the first. One download job
// brings the whole set: each file to its .part (resumable), and the files
// are renamed only once every one is complete, so a half set never shows
// as a model.

// shardRe matches a split GGUF stem such as "model-00001-of-00003" (the
// engine's scan uses the same pattern).
var shardRe = regexp.MustCompile(`-(\d{5})-of-(\d{5})$`)

// maxShards bounds a split set (Hugging Face sets have a few dozen at most).
const maxShards = 512

// hfShards returns every file of src's split set, first to last; just src
// when its name is not a split name (or names a set of one).
func hfShards(src hfSource) []hfSource {
	ext := filepath.Ext(src.file)
	stem := strings.TrimSuffix(src.file, ext)
	m := shardRe.FindStringSubmatch(stem)
	if m == nil {
		return []hfSource{src}
	}
	n, _ := strconv.Atoi(m[2])
	k, _ := strconv.Atoi(m[1])
	if n <= 1 || n > maxShards || k < 1 || k > n {
		return []hfSource{src}
	}
	base := strings.TrimSuffix(stem, m[0])
	cut := strings.LastIndexByte(src.url, '/')
	out := make([]hfSource, 0, n)
	for i := 1; i <= n; i++ {
		s := src
		s.file = fmt.Sprintf("%s-%05d-of-%s%s", base, i, m[2], ext)
		s.url = src.url[:cut+1] + url.PathEscape(s.file)
		out = append(out, s)
	}
	return out
}

// firstShard is src's set's first file (src itself when it is not split):
// a set is queued and named by its first file, so asking for any file of
// it twice is one job.
func firstShard(src hfSource) hfSource {
	return hfShards(src)[0]
}

// hfSize asks Hugging Face a file's size without downloading it (-1 when
// it does not say).
func (m *Manager) hfSize(ctx context.Context, rawURL string) int64 {
	if m.checkURL(rawURL, false) != nil {
		return -1
	}
	info := &linkedInfo{}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return -1
	}
	req.Header.Set("User-Agent", "cerberus")
	resp, err := m.hfClient(info).Do(req)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	info.observe(resp.Header)
	if info.size > 0 {
		return info.size
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return -1
}

// runHFSplit downloads every file of a split set into dir.
func (m *Manager) runHFSplit(ctx context.Context, j *job, src hfSource, set []hfSource, dir string) error {
	dests := make([]string, len(set))
	for i, s := range set {
		dests[i] = filepath.Join(dir, s.file)
		if _, err := os.Lstat(dests[i]); err == nil {
			return fmt.Errorf("%s already exists", dests[i])
		}
	}
	m.update(j, true, func(it *Item) { it.Path = dests[0] })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	removeParts := func() {
		for _, d := range dests {
			_ = os.Remove(d + ".part")
		}
	}

	// Sizes first: the progress is the set's, and the disk must hold the
	// whole set (what is still to come) plus the headroom.
	sizes := make([]int64, len(set))
	var grand, need int64
	known := true
	for i, s := range set {
		sizes[i] = m.hfSize(ctx, s.url)
		if sizes[i] <= 0 {
			known = false
			continue
		}
		grand += sizes[i]
		need += sizes[i]
		if st, err := os.Lstat(dests[i] + ".part"); err == nil && st.Mode().IsRegular() && st.Size() <= sizes[i] {
			need -= st.Size()
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !known {
		grand = 0 // each file checks the disk for itself
	} else {
		m.update(j, true, func(it *Item) { it.Total = grand })
		if free, err := m.opt.FreeSpace(dir); err == nil && free < uint64(need+DiskHeadroom) {
			return fmt.Errorf("%w: need %s (%d files) + 2 GiB, %s free in %s", ErrNoSpace, gib(need), len(set), gib(int64(free)), dir)
		}
	}

	verified := true
	var base int64
	for i, s := range set {
		want := ""
		if s.file == src.file {
			want = j.req.SHA256 // a checksum given with the link is the named file's
		}
		ok, err := m.fetchHF(ctx, j, hfFetch{url: s.url, part: dests[i] + ".part", dir: dir, want: want,
			split: true, base: base, grand: grand})
		if err != nil {
			if m.discarded(j) {
				removeParts() // the user threw it away; a failure or shutdown keeps them to resume
			}
			if len(set) > 1 && !errors.Is(err, context.Canceled) {
				err = fmt.Errorf("file %d of %d (%s): %w", i+1, len(set), s.file, err)
			}
			return err
		}
		verified = verified && ok
		if st, err := os.Stat(dests[i] + ".part"); err == nil {
			base += st.Size()
		}
		m.update(j, true, func(it *Item) { it.State = StateRunning })
	}
	if m.discarded(j) {
		removeParts()
		return context.Canceled
	}
	// Every file is complete: only now do they take their names.
	for _, d := range dests {
		if _, err := os.Lstat(d); err == nil {
			return fmt.Errorf("%s already exists", d)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for i, d := range dests {
		if err := os.Rename(d+".part", d); err != nil {
			// Undo the renames done so far so the set stays whole as .part.
			for _, back := range dests[:i] {
				_ = os.Rename(back, back+".part")
			}
			return err
		}
	}
	m.update(j, true, func(it *Item) { it.Verified = verified })
	// Where it came from, for exports (a failure here costs only that).
	_ = os.WriteFile(dests[0]+SourceSuffix, []byte(set[0].url+"\n"), 0o600)
	return nil
}
