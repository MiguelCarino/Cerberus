package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/platform"
)

// Learned VRAM footprints (SCOPE "Railguard contract and engine hardening
// (2026-11-15)").
//
// The need estimate (size × 1.15 + 512 + KV) is a rule of thumb. After a
// load the manager reads the GPU again and records what the load really
// took, keyed by (backend, model, ctx, slots). The next plan for the same
// key uses the median of the last three samples plus headroomMiB instead
// of the estimate (of two, the larger). The median drops one sample that
// the desktop inflated while the model loaded, and one that memory freed
// outside Cerberus during the load (LM Studio's own TTL, another client, a
// game closing) made too low: the smallest would plan the next loads short
// and push the card past railguard's line. A sample is not taken at all
// when another load or unload ran meanwhile, or the load had to evict
// first, and a full-offload sample below the model's weights is refused.
// A sample taken with a GPU split is
// scaled up to the whole model by the share that was offloaded, and only
// when that share is known exactly (LM Studio's --gpu ratio Cerberus
// passed). The samples persist in the data folder as engine-footprints.json.

const (
	footprintFile    = "engine-footprints.json"
	footprintSamples = 3
)

// footprintKey names one load shape.
func footprintKey(backend, id string, ctx, slots int) string {
	return fmt.Sprintf("%s|%s|%d|%d", backend, id, ctx, slots)
}

type footprintEntry struct {
	Samples []int `json:"samples"` // full-offload MiB, oldest first
	Time    int64 `json:"time"`    // unix s of the last sample
}

type footprintDoc struct {
	V      int                        `json:"v"`
	Models map[string]*footprintEntry `json:"models"`
}

// footprints is the learned store. path returns the file ("" or nil:
// memory only); it is read on first use, so building a Manager does no I/O.
type footprints struct {
	path func() string

	mu     sync.Mutex
	loaded bool
	file   string
	data   map[string]*footprintEntry
}

func newFootprints(path func() string) *footprints {
	return &footprints{path: path, data: map[string]*footprintEntry{}}
}

// dataDirFootprints keeps the store in Cerberus' data folder.
func dataDirFootprints() string {
	d, err := platform.DataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, footprintFile)
}

// loadLocked reads the file once. A missing or corrupt file is an empty
// store (the estimate applies). Caller holds f.mu.
func (f *footprints) loadLocked() {
	if f.loaded {
		return
	}
	f.loaded = true
	if f.path != nil {
		f.file = f.path()
	}
	if f.file == "" {
		return
	}
	raw, err := os.ReadFile(f.file)
	if err != nil {
		return
	}
	var doc footprintDoc
	if json.Unmarshal(raw, &doc) != nil || doc.V != 1 {
		return
	}
	for k, e := range doc.Models {
		if e == nil {
			continue
		}
		e.Samples = slices.DeleteFunc(e.Samples, func(n int) bool { return n <= 0 })
		if len(e.Samples) > 0 {
			f.data[k] = e
		}
	}
}

// need is the learned need for key: the median recent sample (of two, the
// larger) plus headroomMiB. ok is false when nothing was learned.
func (f *footprints) need(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	e := f.data[key]
	if e == nil || len(e.Samples) == 0 {
		return 0, false
	}
	sorted := slices.Sorted(slices.Values(e.Samples))
	return sorted[len(sorted)/2] + headroomMiB, true
}

// add records one full-offload sample for key and saves the store.
func (f *footprints) add(key string, fullMiB int, now time.Time) {
	if fullMiB <= 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	e := f.data[key]
	if e == nil {
		e = &footprintEntry{}
		f.data[key] = e
	}
	e.Samples = append(e.Samples, fullMiB)
	if n := len(e.Samples); n > footprintSamples {
		e.Samples = slices.Clone(e.Samples[n-footprintSamples:])
	}
	e.Time = now.Unix()
	if f.file == "" {
		return
	}
	raw, err := json.MarshalIndent(footprintDoc{V: 1, Models: f.data}, "", " ")
	if err == nil {
		_ = platform.WritePrivate(f.file, append(raw, '\n'))
	}
}

// footprintSample turns one load's VRAM readings into a full-offload
// sample: used is the free VRAM before minus after, share the part of the
// model on the GPU (1 = all). ok is false for a reading that cannot be
// this model: less than its weights (a full offload holds at least those;
// less means memory was freed outside Cerberus during the load), or more
// than twice the estimate plus 2 GiB.
func footprintSample(used int, share float64, sizeMiB, estimate int) (int, bool) {
	if used <= 0 || share <= 0 || share > 1 {
		return 0, false
	}
	full := int(float64(used) / share)
	if full < sizeMiB || full > 2*estimate+2048 {
		return 0, false
	}
	return full, true
}

// offloadShare is the part of info on the GPU after a load planned with
// need and budget, when it is known exactly: 1 when no split was passed,
// LM Studio's --gpu ratio when Cerberus passed one (or "max"). ok is false
// otherwise: llama.cpp's -ngl is rounded to whole layers and may be the
// owner's own, Ollama picks its own split, and an owner's LM Studio
// fraction or "off" is not the whole model.
func (m *Manager) offloadShare(info ModelInfo, need, budget int) (float64, bool) {
	r, split := gpuRatio(need, budget)
	if split && cpuOnly(need, info.SizeMiB, budget) {
		return 0, false // loaded with no GPU offload: nothing to learn
	}
	if info.Backend == BackendLMStudio {
		switch strings.TrimSpace(m.conf().Engines.LMStudio.GPU) {
		case "":
			if split {
				return math.Round(r*100) / 100, true // as lmsLoadArgs prints it
			}
			return 1, true
		case "max":
			return 1, true
		}
		return 0, false
	}
	if split {
		return 0, false
	}
	return 1, true
}

// learnFootprint records what a load took: used is the free VRAM before
// the load minus after it. Caller checked that no other load or unload ran.
func (m *Manager) learnFootprint(info ModelInfo, limit, need, budget, used int) {
	if m.footprints == nil {
		return
	}
	share, ok := m.offloadShare(info, need, budget)
	if !ok {
		return
	}
	ctx, slots := planShape(info, limit)
	est := estimateNeedMiB(info.SizeMiB, ctx, slots)
	full, ok := footprintSample(used, share, info.SizeMiB, est)
	if !ok {
		m.log.Debug("VRAM footprint sample dropped", "model", info.ID, "used_mib", used, "share", share, "estimate_mib", est)
		return
	}
	m.footprints.add(footprintKey(info.Backend, info.ID, ctx, slots), full, m.now())
	m.log.Info("learned VRAM footprint", "model", info.ID, "ctx", ctx, "slots", slots,
		"used_mib", used, "share", share, "full_mib", full, "estimate_mib", est)
}
