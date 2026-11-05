package gpu

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const nvidiaTimeout = 5 * time.Second

var nvidiaArgs = []string{
	"--query-gpu=index,name,memory.total,memory.used,power.draw,power.limit,utilization.gpu,temperature.gpu,pstate",
	"--format=csv,noheader,nounits",
}

// queryNvidia runs nvidia-smi once and parses its CSV output.
func queryNvidia(ctx context.Context, bin string) ([]GPU, error) {
	ctx, cancel := context.WithTimeout(ctx, nvidiaTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, nvidiaArgs...).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseNvidia(string(out))
}

// parseNvidia parses one GPU per line. Unavailable values ("[N/A]",
// "[Not Supported]") become zero, or NaN for power.draw.
func parseNvidia(out string) ([]GPU, error) {
	var gpus []GPU
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 9 {
			return nil, fmt.Errorf("nvidia-smi: unexpected line %q", line)
		}
		// A comma inside the name would shift the fields: rejoin the middle.
		name := strings.Join(f[1:len(f)-7], ",")
		f = append([]string{f[0], name}, f[len(f)-7:]...)
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		idx, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, fmt.Errorf("nvidia-smi: bad index %q", f[0])
		}
		gpus = append(gpus, GPU{
			Index:       idx,
			Name:        f[1],
			Vendor:      "nvidia",
			MemTotalMiB: atoi(f[2]),
			MemUsedMiB:  atoi(f[3]),
			PowerW:      atofNaN(f[4]),
			PowerLimitW: zeroNaN(atofNaN(f[5])),
			UtilPct:     atoi(f[6]),
			TempC:       atoi(f[7]),
			PState:      na(f[8]),
		})
	}
	return gpus, nil
}

// atoi parses an integer, tolerating decimals and unavailable markers (→ 0).
func atoi(s string) int {
	return int(zeroNaN(atofNaN(s)))
}

// atofNaN parses a float; unavailable or malformed values become NaN.
func atofNaN(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return v
}

func zeroNaN(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// na blanks bracketed "not available" markers.
func na(s string) string {
	if strings.HasPrefix(s, "[") {
		return ""
	}
	return s
}
