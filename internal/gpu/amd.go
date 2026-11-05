package gpu

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sysfsDRM is where Linux exposes DRM devices. On other systems the glob
// finds nothing and the AMD backend reports no GPUs.
const sysfsDRM = "/sys/class/drm"

var cardRe = regexp.MustCompile(`^card(\d+)$`)

// queryAMD reads AMD GPUs from sysfs under root (usually /sys/class/drm).
// Only cards exposing mem_info_vram_total (amdgpu) are reported.
func queryAMD(root string) ([]GPU, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil // no sysfs: not an error, just no GPUs
	}
	type card struct {
		n   int
		dev string
	}
	var cards []card
	for _, e := range entries {
		m := cardRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		dev := filepath.Join(root, e.Name(), "device")
		if _, err := os.Stat(filepath.Join(dev, "mem_info_vram_total")); err != nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		cards = append(cards, card{n, dev})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].n < cards[j].n })

	gpus := make([]GPU, 0, len(cards))
	for i, c := range cards {
		gpus = append(gpus, readAMDCard(i, c.dev))
	}
	return gpus, nil
}

func readAMDCard(index int, dev string) GPU {
	g := GPU{
		Index:       index,
		Name:        "AMD GPU",
		Vendor:      "amd",
		MemTotalMiB: int(readNum(filepath.Join(dev, "mem_info_vram_total")) / (1 << 20)),
		MemUsedMiB:  int(readNum(filepath.Join(dev, "mem_info_vram_used")) / (1 << 20)),
		UtilPct:     int(readNum(filepath.Join(dev, "gpu_busy_percent"))),
		PowerW:      math.NaN(),
	}
	if name := readStr(filepath.Join(dev, "product_name")); name != "" {
		g.Name = name
	}
	hwmons, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	if len(hwmons) > 0 {
		h := hwmons[0]
		// power1_average and power1_cap are microwatts, temp1_input millidegrees.
		if uw, ok := readNumOK(filepath.Join(h, "power1_average")); ok {
			g.PowerW = uw / 1e6
		} else if uw, ok := readNumOK(filepath.Join(h, "power1_input")); ok {
			g.PowerW = uw / 1e6
		}
		g.PowerLimitW = readNum(filepath.Join(h, "power1_cap")) / 1e6
		g.TempC = int(readNum(filepath.Join(h, "temp1_input")) / 1000)
	}
	return g
}

func readStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readNumOK(path string) (float64, bool) {
	v, err := strconv.ParseFloat(readStr(path), 64)
	return v, err == nil
}

func readNum(path string) float64 {
	v, _ := readNumOK(path)
	return v
}
