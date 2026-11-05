package engine

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

// GPU split at load time (SCOPE "As built: model queue and room").
//
// LM Studio applies whatever GPU offload was last saved for a model, and a
// saved "max" forces every layer onto the GPU even when the weights plus the
// KV caches of all parallel slots cannot fit: the engine then dies with a
// CUDA out-of-memory on the first concurrent prompts. So when the manager's
// estimate says the model will not fit in the VRAM left after making room,
// Cerberus tells the engine how much to offload (LM Studio --gpu <ratio>,
// llama.cpp -ngl <layers>) instead of trusting a saved setting. When it does
// fit, nothing is passed and the engine decides.

// kvTokensPerWeight is the simple KV-cache estimate: the cache for this many
// tokens is about as large as the model's weights. For a ~17 GiB 27B model
// at Q4 that is ~2 GiB per 32k tokens, which is the right order for current
// GQA models (it overestimates small ones a little, which errs safe).
const kvTokensPerWeight = 256 * 1024

// lmsDefaultParallel is LM Studio's own default for concurrent predictions.
const lmsDefaultParallel = 4

// estimateNeedMiB is the VRAM a model needs with ctx tokens of context for
// each of parallel slots: the weights with the manager's headroom, plus KV.
func estimateNeedMiB(sizeMiB, ctx, parallel int) int {
	tokens := int64(max(ctx, 0)) * int64(max(parallel, 1))
	kv := int(int64(sizeMiB) * tokens / kvTokensPerWeight)
	return int(float64(sizeMiB)*headroomFactor) + headroomMiB + kv
}

// gpuRatio is the share of the model to offload when need does not fit in
// free: free/need clamped to [0.1, 0.95]. ok is false when it fits or free
// VRAM is unknown (pass nothing; the engine decides).
func gpuRatio(need, free int) (ratio float64, ok bool) {
	if free < 0 || need <= 0 || free >= need {
		return 0, false
	}
	return min(max(float64(free)/float64(need), 0.1), 0.95), true
}

// lmsLoadArgs builds the `lms load` command line for m (pure, for tests).
// ttl is the idle TTL in seconds (0: none); gpu is engines.lmstudio.gpu.
// --parallel is the manager's queue limit, so the engine never allocates KV
// for more slots than Cerberus will use. --gpu is the explicit setting when
// there is one, else a ratio only when the model does not fit.
func lmsLoadArgs(m ModelInfo, ttl int, gpu string) []string {
	args := []string{"load", m.ID}
	if m.Ctx > 0 {
		args = append(args, "--context-length", strconv.Itoa(m.Ctx))
	}
	if ttl > 0 {
		args = append(args, "--ttl", strconv.Itoa(ttl))
	}
	if m.MaxParallel > 0 {
		args = append(args, "--parallel", strconv.Itoa(m.MaxParallel))
	}
	if g := strings.TrimSpace(gpu); g != "" {
		args = append(args, "--gpu", g)
	} else if m.plan != nil {
		need := estimateNeedMiB(m.SizeMiB, m.Ctx, m.MaxParallel)
		if r, ok := gpuRatio(need, m.plan.FreeMiB); ok {
			args = append(args, "--gpu", fmt.Sprintf("%.2f", r))
		}
	}
	return append(args, "-y", "--identifier", m.ID)
}

// llamaNGL is the -ngl for a llama.cpp model of layers layers that does not
// fit: llama-server's -c is the total context shared by all slots, so the
// KV estimate uses ctx once. 0 means pass nothing (it fits, VRAM or the
// layer count is unknown, or the user's own args already set it).
func llamaNGL(sizeMiB, ctx, layers, free int, userArgs []string) int {
	if layers <= 0 || hasNGL(userArgs) {
		return 0
	}
	r, ok := gpuRatio(estimateNeedMiB(sizeMiB, ctx, 1), free)
	if !ok {
		return 0
	}
	return max(int(float64(layers)*r), 1)
}

func hasNGL(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		a, _, _ = strings.Cut(a, "=")
		return a == "-ngl" || a == "--n-gpu-layers" || a == "--gpu-layers"
	})
}

// ggufLayers reads the layer count (<arch>.block_count) from a GGUF file's
// metadata, or 0 when it cannot. It reads only the header key/values, and
// stops as soon as it has the answer.
func ggufLayers(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n, _ := readGGUFLayers(bufio.NewReaderSize(io.LimitReader(f, 256<<20), 64<<10))
	return n
}

var errGGUF = errors.New("not a readable gguf")

// readGGUFLayers parses GGUF v2/v3 metadata from r.
func readGGUFLayers(r io.Reader) (int, error) {
	le := binary.LittleEndian
	var hdr struct {
		Magic   [4]byte
		Version uint32
		Tensors uint64
		KVs     uint64
	}
	if err := binary.Read(r, le, &hdr); err != nil || string(hdr.Magic[:]) != "GGUF" || hdr.Version < 2 {
		return 0, errGGUF
	}
	arch := ""
	counts := map[string]int{} // arch -> block_count, for keys seen before the arch
	for i := uint64(0); i < hdr.KVs && i < 100000; i++ {
		key, err := ggufString(r)
		if err != nil {
			return 0, err
		}
		var typ uint32
		if err := binary.Read(r, le, &typ); err != nil {
			return 0, err
		}
		switch {
		case key == "general.architecture" && typ == 8:
			if arch, err = ggufString(r); err != nil {
				return 0, err
			}
			if n, ok := counts[arch]; ok {
				return n, nil
			}
			continue
		case strings.HasSuffix(key, ".block_count") && typ <= 12:
			v, err := ggufScalar(r, typ)
			if err != nil {
				return 0, err
			}
			prefix := strings.TrimSuffix(key, ".block_count")
			if arch != "" && prefix == arch {
				return int(v), nil
			}
			counts[prefix] = int(v)
			continue
		}
		if err := ggufSkip(r, typ, 0); err != nil {
			return 0, err
		}
	}
	return 0, errGGUF
}

func ggufString(r io.Reader) (string, error) {
	var n uint64
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n > 1<<20 {
		return "", errGGUF
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// ggufSizes is the byte size of each scalar value type (0 = not scalar).
var ggufSizes = [13]int{1, 1, 2, 2, 4, 4, 4, 1, 0, 0, 8, 8, 8}

// ggufScalar reads an integer-valued scalar of type typ.
func ggufScalar(r io.Reader, typ uint32) (uint64, error) {
	sz := ggufSizes[typ]
	if sz == 0 {
		return 0, errGGUF
	}
	var b [8]byte
	if _, err := io.ReadFull(r, b[:sz]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b[:]), nil
}

// ggufSkip skips one value of type typ.
func ggufSkip(r io.Reader, typ uint32, depth int) error {
	switch {
	case typ > 12 || depth > 2:
		return errGGUF
	case typ == 8:
		_, err := ggufString(r)
		return err
	case typ == 9:
		var et uint32
		var n uint64
		if err := binary.Read(r, binary.LittleEndian, &et); err != nil {
			return err
		}
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return err
		}
		if et <= 12 && ggufSizes[et] > 0 {
			_, err := io.CopyN(io.Discard, r, int64(n)*int64(ggufSizes[et]))
			return err
		}
		for range n {
			if err := ggufSkip(r, et, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := io.CopyN(io.Discard, r, int64(ggufSizes[typ]))
	return err
}
