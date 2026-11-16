package engine

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// scanDepth bounds how deep ModelDirs are walked (publisher/model/file.gguf
// layouts are three levels); it keeps a stray huge tree from costing much.
const scanDepth = 3

// shardRe matches split GGUF names such as "model-00001-of-00003".
var shardRe = regexp.MustCompile(`-(\d{5})-of-(\d{5})$`)

// ggufFile is one model found on disk.
type ggufFile struct {
	ID      string
	Path    string
	MMProj  string // projector in the same folder (vision), if exactly one
	SizeMiB int
}

// scanGGUF lists *.gguf models under dirs. Projector (mmproj) files are not
// models; a folder with exactly one projector marks its models as vision.
// Only the first shard of a split model is listed, with the set's total
// size, and only when every shard is there: llama-server cannot open a
// set with one missing (a download in progress, a copy cut short). A set
// left out is logged at debug level with the files it lacks (log may be
// nil).
func scanGGUF(dirs []string, log *slog.Logger) []ggufFile {
	var out []ggufFile
	seen := map[string]bool{}
	projectors := map[string]string{}
	mmproj := func(dir string) string {
		v, ok := projectors[dir]
		if !ok {
			v = findMMProj(dir)
			projectors[dir] = v
		}
		return v
	}
	for _, root := range dirs {
		root = filepath.Clean(root)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() && p != root {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if depth(root, p) >= scanDepth {
					return fs.SkipDir
				}
				return nil
			}
			f, missing, ok := ggufEntry(p, d, mmproj)
			if len(missing) > 0 && log != nil {
				log.Debug("split model left out: files missing", "model", p, "missing", strings.Join(missing, ", "))
			}
			if ok && !seen[f.ID] {
				seen[f.ID] = true
				out = append(out, f)
			}
			return nil
		})
	}
	return out
}

func depth(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

// ggufEntry turns one directory entry into a model, if it is one. For
// the first shard of a split set it sums the set's sizes; missing names
// the shards not on disk (then ok is false).
func ggufEntry(p string, d fs.DirEntry, mmproj func(dir string) string) (f ggufFile, missing []string, ok bool) {
	name := d.Name()
	if !strings.EqualFold(filepath.Ext(name), ".gguf") || isMMProj(name) {
		return ggufFile{}, nil, false
	}
	info, err := d.Info()
	if err != nil {
		return ggufFile{}, nil, false
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	size := info.Size()
	if m := shardRe.FindStringSubmatch(stem); m != nil {
		if m[1] != "00001" {
			return ggufFile{}, nil, false
		}
		stem = strings.TrimSuffix(stem, m[0])
		n, _ := strconv.Atoi(m[2])
		dir := filepath.Dir(p)
		for k := 2; k <= n; k++ {
			sib := fmt.Sprintf("%s-%05d-of-%s%s", stem, k, m[2], ext)
			st, err := os.Stat(filepath.Join(dir, sib))
			if err != nil || !st.Mode().IsRegular() {
				missing = append(missing, sib)
				continue
			}
			size += st.Size()
		}
		if len(missing) > 0 {
			return ggufFile{}, missing, false
		}
	}
	return ggufFile{ID: stem, Path: p, MMProj: mmproj(filepath.Dir(p)), SizeMiB: mib(size)}, nil, true
}

func isMMProj(name string) bool { return strings.Contains(strings.ToLower(name), "mmproj") }

// findMMProj returns the folder's projector when there is exactly one, so a
// folder holding several unrelated models never gets a wrong pairing.
func findMMProj(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !isMMProj(n) || !strings.EqualFold(filepath.Ext(n), ".gguf") {
			continue
		}
		if found != "" {
			return ""
		}
		found = filepath.Join(dir, n)
	}
	return found
}
