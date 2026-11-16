package downloads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// splitFake serves a split GGUF set the way Hugging Face does: each
// /resolve/ link redirects to the CDN with X-Linked-Etag/X-Linked-Size.
type splitFake struct {
	files map[string][]byte
	mu    sync.Mutex
	fail  map[string]int // GETs of a file that answer 500 first
	slow  string         // this file streams 64 KiB, then hangs
	gets  []string       // "file range" of each CDN GET
}

func (f *splitFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	content, ok := f.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.Contains(r.URL.Path, "/resolve/"):
		s := sha256.Sum256(content)
		w.Header().Set("X-Linked-Etag", `"`+hex.EncodeToString(s[:])+`"`)
		w.Header().Set("X-Linked-Size", fmt.Sprint(len(content)))
		http.Redirect(w, r, "https://cdn-lfs.hf.co/repos/"+name, http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/repos/"):
		if r.Method == http.MethodGet {
			f.mu.Lock()
			f.gets = append(f.gets, name+" "+r.Header.Get("Range"))
			failing := f.fail[name] > 0
			if failing {
				f.fail[name]--
			}
			f.mu.Unlock()
			if failing {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if name == f.slow {
				w.Header().Set("Content-Length", fmt.Sprint(len(content)))
				w.WriteHeader(http.StatusOK)
				w.Write(content[:64<<10])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
		}
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(content))
	default:
		http.NotFound(w, r)
	}
}

func (e *env) serveSplit(f *splitFake) {
	srv := httptest.NewServer(f)
	e.t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	e.m.hfTransport = rewrite{target: u, rt: srv.Client().Transport}
}

func shardName(k int) string { return fmt.Sprintf("big-Q4_K_M-%05d-of-00003.gguf", k) }

func newSplit() (*splitFake, int64) {
	f := &splitFake{files: map[string][]byte{}, fail: map[string]int{}}
	var total int64
	for k, n := range []int{300 << 10, 300 << 10, 123 << 10} {
		b, _ := blob(n + k)
		f.files[shardName(k+1)] = b
		total += int64(len(b))
	}
	return f, total
}

func (e *env) shardDest(k int) string {
	return filepath.Join(e.models, "acme", "big-GGUF", shardName(k))
}

const splitURL = "https://huggingface.co/acme/big-GGUF/blob/main/Q4_K_M/big-Q4_K_M-00002-of-00003.gguf"

func TestHFShards(t *testing.T) {
	src, err := parseHFURL(splitURL)
	if err != nil {
		t.Fatal(err)
	}
	set := hfShards(src)
	if len(set) != 3 || set[0].file != shardName(1) || set[2].file != shardName(3) ||
		set[0].url != "https://huggingface.co/acme/big-GGUF/resolve/main/Q4_K_M/"+shardName(1) {
		t.Fatalf("set %+v", set)
	}
	for _, plain := range []string{"https://hf.co/a/b/resolve/main/m.gguf", "https://hf.co/a/b/resolve/main/m-00001-of-00001.gguf",
		"https://hf.co/a/b/resolve/main/m-00004-of-00003.gguf"} {
		s, _ := parseHFURL(plain)
		if got := hfShards(s); len(got) != 1 || got[0] != s {
			t.Errorf("%s: %+v", plain, got)
		}
	}
}

// Any file's link queues the whole set as one job; every file lands, and
// only once all are complete.
func TestHFSplitSet(t *testing.T) {
	e := newEnv(t, nil)
	f, total := newSplit()
	e.serveSplit(f)
	it, err := e.m.Add(Request{Ref: splitURL})
	if err != nil {
		t.Fatal(err)
	}
	if it.Name != shardName(1) || !strings.HasSuffix(it.Ref, "/"+shardName(1)) {
		t.Fatalf("item %+v", it)
	}
	if dup, _ := e.m.Add(Request{Ref: strings.Replace(splitURL, "00002-of", "00003-of", 1)}); dup.ID != it.ID {
		t.Fatal("another file of the same set queued a second job")
	}
	it = e.final(it.ID)
	if it.State != StateDone || !it.Verified || it.Total != total || it.Bytes != total || it.Path != e.shardDest(1) {
		t.Fatalf("final %+v (total %d)", it, total)
	}
	for k := 1; k <= 3; k++ {
		got, err := os.ReadFile(e.shardDest(k))
		if err != nil || !bytes.Equal(got, f.files[shardName(k)]) {
			t.Fatalf("shard %d differs (%v)", k, err)
		}
		if _, err := os.Stat(e.shardDest(k) + ".part"); !os.IsNotExist(err) {
			t.Fatalf("shard %d .part left behind", k)
		}
	}
	if src := HFSource(e.shardDest(1)); !strings.HasSuffix(src, "/Q4_K_M/"+shardName(1)) {
		t.Fatalf("source note %q", src)
	}
}

// A failure on the second file keeps the first file's .part and renames
// nothing; asking again resumes and does not fetch the first file again.
func TestHFSplitFailureKeepsParts(t *testing.T) {
	e := newEnv(t, nil)
	f, _ := newSplit()
	f.fail[shardName(2)] = 1
	e.serveSplit(f)
	it, err := e.m.Add(Request{Ref: splitURL})
	if err != nil {
		t.Fatal(err)
	}
	it = e.final(it.ID)
	if it.State != StateFailed || !strings.Contains(it.Error, "file 2 of 3") {
		t.Fatalf("final %+v", it)
	}
	for k := 1; k <= 3; k++ {
		if _, err := os.Stat(e.shardDest(k)); !os.IsNotExist(err) {
			t.Fatalf("shard %d renamed before the set was complete", k)
		}
	}
	if st, err := os.Stat(e.shardDest(1) + ".part"); err != nil || st.Size() != int64(len(f.files[shardName(1)])) {
		t.Fatalf("first .part not kept whole: %v %v", st, err)
	}

	it, err = e.m.Add(Request{Ref: splitURL})
	if err != nil {
		t.Fatal(err)
	}
	if it = e.final(it.ID); it.State != StateDone || !it.Verified {
		t.Fatalf("retry %+v", it)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	firsts := 0
	for _, g := range f.gets {
		if strings.HasPrefix(g, shardName(1)+" ") {
			firsts++
			if firsts == 2 && g != fmt.Sprintf("%s bytes=%d-", shardName(1), len(f.files[shardName(1)])) {
				t.Errorf("the first file was fetched again: %q", g)
			}
		}
	}
	if firsts != 2 {
		t.Errorf("first file GETs %q", f.gets)
	}
}

// The disk check counts the whole set, not one file.
func TestHFSplitDiskRefusal(t *testing.T) {
	e := newEnv(t, nil)
	f, total := newSplit()
	e.serveSplit(f)
	e.free = DiskHeadroom + uint64(total) - 1 // room for any one file, not the set
	it, err := e.m.Add(Request{Ref: splitURL})
	if err != nil {
		t.Fatal(err)
	}
	it = e.final(it.ID)
	if it.State != StateFailed || !strings.Contains(it.Error, ErrNoSpace.Error()) ||
		!strings.Contains(it.Error, "3 files") {
		t.Fatalf("final %+v", it)
	}
	for k := 1; k <= 3; k++ {
		if _, err := os.Stat(e.shardDest(k) + ".part"); !os.IsNotExist(err) {
			t.Fatal("wrote despite the refusal")
		}
	}
}

// A user cancel in the middle of the second file throws the whole set away.
func TestHFSplitCancel(t *testing.T) {
	e := newEnv(t, nil)
	f, _ := newSplit()
	f.slow = shardName(2)
	e.serveSplit(f)
	it, err := e.m.Add(Request{Ref: splitURL})
	if err != nil {
		t.Fatal(err)
	}
	first := int64(len(f.files[shardName(1)]))
	e.wait(it.ID, func(it Item) bool { return it.Bytes > first })
	if err := e.m.Cancel(it.ID); err != nil {
		t.Fatal(err)
	}
	if it = e.final(it.ID); it.State != StateCanceled {
		t.Fatalf("final %+v", it)
	}
	for k := 1; k <= 3; k++ {
		for _, p := range []string{e.shardDest(k), e.shardDest(k) + ".part"} {
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Fatalf("%s kept after a cancel", p)
			}
		}
	}
}

// A checksum names one file: with a split set it must come with the first
// file's link.
func TestHFSplitChecksumNeedsFirstFile(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.m.Add(Request{Ref: splitURL, SHA256: strings.Repeat("ab", 32)}); !errors.Is(err, ErrBadRef) ||
		!strings.Contains(err.Error(), shardName(1)) {
		t.Fatalf("err %v", err)
	}
}
