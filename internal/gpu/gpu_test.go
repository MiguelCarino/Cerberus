package gpu

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseNvidia(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []GPU
		nan  bool // first GPU's power is unknown
	}{
		{
			name: "rtx",
			in:   "0, NVIDIA GeForce RTX 4090, 24564, 2600, 59.12, 450.00, 13, 41, P0\n",
			want: []GPU{{Index: 0, Name: "NVIDIA GeForce RTX 4090", Vendor: "nvidia", MemTotalMiB: 24564, MemUsedMiB: 2600, PowerW: 59.12, PowerLimitW: 450, UtilPct: 13, TempC: 41, PState: "P0"}},
		},
		{
			name: "unavailable values",
			in:   "1, Tesla K80, 11441, 0, [N/A], [Not Supported], [N/A], 30, [Not Supported]\n",
			want: []GPU{{Index: 1, Name: "Tesla K80", Vendor: "nvidia", MemTotalMiB: 11441, TempC: 30}},
			nan:  true,
		},
		{
			name: "comma in name and two gpus",
			in:   "0, Odd, Name, 100, 50, 10.5, 20, 1, 2, P8\n1, B, 200, 0, 1, 2, 3, 4, P2\n\n",
			want: []GPU{
				{Index: 0, Name: "Odd, Name", Vendor: "nvidia", MemTotalMiB: 100, MemUsedMiB: 50, PowerW: 10.5, PowerLimitW: 20, UtilPct: 1, TempC: 2, PState: "P8"},
				{Index: 1, Name: "B", Vendor: "nvidia", MemTotalMiB: 200, PowerW: 1, PowerLimitW: 2, UtilPct: 3, TempC: 4, PState: "P2"},
			},
		},
		{name: "empty", in: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNvidia(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d gpus, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if tc.nan && i == 0 {
					if !math.IsNaN(got[i].PowerW) {
						t.Fatalf("power = %v, want NaN", got[i].PowerW)
					}
					got[i].PowerW = 0
				}
				if got[i] != tc.want[i] {
					t.Errorf("gpu %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
	if _, err := parseNvidia("garbage line\n"); err == nil {
		t.Error("want error for short line")
	}
}

func TestEnergyTrapezoid(t *testing.T) {
	m := New(config.GPUConfig{}, quiet())
	t0 := time.Unix(1000, 0)
	g := func(p ...float64) []GPU {
		var out []GPU
		for i, w := range p {
			out = append(out, GPU{Index: i, PowerW: w, MemTotalMiB: 1000, MemUsedMiB: 400})
		}
		return out
	}
	m.record(t0, g(100, 50))                            // first point: nothing integrated
	m.record(t0.Add(2*time.Second), g(200, 50))         // (150+250)/2*2 = 400
	m.record(t0.Add(3*time.Second), nil)                // query failed: gap
	m.record(t0.Add(4*time.Second), g(100))             // gap end: nothing integrated
	m.record(t0.Add(5*time.Second), g(math.NaN()))      // unknown power: gap
	m.record(t0.Add(6*time.Second), g(100))             // after unknown: nothing
	m.record(t0.Add(7*time.Second), g(300, math.NaN())) // partial known: (100+300)/2*1 = 200
	if e := m.EnergyJ(); math.Abs(e-600) > 1e-9 {
		t.Fatalf("energy = %v, want 600", e)
	}
	s := m.Snapshot()
	if s.EnergyJ != m.EnergyJ() || len(s.GPUs) != 2 || s.GPUs[1].PowerW != 0 {
		t.Fatalf("snapshot = %+v", s)
	}
	if f := m.FreeVRAMMiB(); f != 1200 {
		t.Fatalf("free = %d, want 1200", f)
	}
}

func TestFreeVRAMUnknown(t *testing.T) {
	m := New(config.GPUConfig{}, quiet())
	if f := m.FreeVRAMMiB(); f != -1 {
		t.Fatalf("free with no gpus = %d", f)
	}
	m.record(time.Now(), []GPU{{MemTotalMiB: 0}})
	if f := m.FreeVRAMMiB(); f != -1 {
		t.Fatalf("free with unknown total = %d", f)
	}
}

func TestRunSetActiveWakes(t *testing.T) {
	m := New(config.GPUConfig{ActiveIntervalMs: 5, IdleIntervalMs: 3_600_000}, quiet())
	var calls atomic.Int32
	m.query = func(context.Context) ([]GPU, error) {
		calls.Add(1)
		return []GPU{{PowerW: 100, MemTotalMiB: 10}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitFor(t, func() bool { return calls.Load() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if c := calls.Load(); c != 1 {
		t.Fatalf("idle loop ticked %d times, want 1", c)
	}
	m.SetActive(true)
	waitFor(t, func() bool { return calls.Load() >= 4 })
	if m.EnergyJ() <= 0 {
		t.Fatal("no energy integrated")
	}
	cancel()
	<-done
}

func TestRunQueryError(t *testing.T) {
	m := New(config.GPUConfig{}, quiet())
	m.query = func(context.Context) ([]GPU, error) { return nil, errors.New("boom") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx) // one tick then exit
	if s := m.Snapshot(); len(s.GPUs) != 0 || s.Time.IsZero() {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestQueryAMD(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "card1", "device")
	hw := filepath.Join(dev, "hwmon", "hwmon3")
	write := func(p, v string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dev, "mem_info_vram_total"), "17163091968")
	write(filepath.Join(dev, "mem_info_vram_used"), "1073741824")
	write(filepath.Join(dev, "gpu_busy_percent"), "42")
	write(filepath.Join(hw, "power1_average"), "123000000")
	write(filepath.Join(hw, "power1_cap"), "300000000")
	write(filepath.Join(hw, "temp1_input"), "55000")
	// A connector and a non-amdgpu card are ignored.
	write(filepath.Join(root, "card1-DP-1", "status"), "connected")
	write(filepath.Join(root, "card0", "device", "vendor"), "0x10de")

	gpus, err := queryAMD(root)
	if err != nil {
		t.Fatal(err)
	}
	want := GPU{Index: 0, Name: "AMD GPU", Vendor: "amd", MemTotalMiB: 16368, MemUsedMiB: 1024, PowerW: 123, PowerLimitW: 300, UtilPct: 42, TempC: 55}
	if len(gpus) != 1 || gpus[0] != want {
		t.Fatalf("got %+v, want %+v", gpus, want)
	}

	if g, err := queryAMD(filepath.Join(root, "missing")); err != nil || g != nil {
		t.Fatalf("missing root: %v %v", g, err)
	}
}

func TestDetectNone(t *testing.T) {
	m := New(config.GPUConfig{Backend: "none"}, quiet())
	g, err := m.detect(context.Background())(context.Background())
	if err != nil || g != nil {
		t.Fatalf("none backend: %v %v", g, err)
	}
	m = New(config.GPUConfig{Backend: "auto", NvidiaSmi: "/nonexistent/nvidia-smi"}, quiet())
	if q := m.detect(context.Background()); q == nil {
		t.Fatal("auto returned nil query")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestIdleReadsOnDemand(t *testing.T) {
	m := New(config.GPUConfig{ActiveIntervalMs: 5, IdleIntervalMs: 50}, quiet())
	var calls atomic.Int32
	m.query = func(context.Context) ([]GPU, error) {
		calls.Add(1)
		return []GPU{{PowerW: 10, MemTotalMiB: 100, MemUsedMiB: 40}}, nil
	}
	if f := m.FreeVRAMMiB(); f != 60 || calls.Load() != 1 {
		t.Fatalf("first read: free %d, %d queries", f, calls.Load())
	}
	m.Snapshot()
	m.FreeVRAMMiB()
	if c := calls.Load(); c != 1 {
		t.Fatalf("fresh reading re-queried: %d queries", c)
	}
	time.Sleep(60 * time.Millisecond)
	m.Snapshot()
	if c := calls.Load(); c != 2 {
		t.Fatalf("stale reading not refreshed: %d queries", c)
	}
	m.Refresh(context.Background())
	if c := calls.Load(); c != 3 {
		t.Fatalf("Refresh did not query: %d", c)
	}
}

func TestRunIdleDoesNotPoll(t *testing.T) {
	m := New(config.GPUConfig{ActiveIntervalMs: 5, IdleIntervalMs: 5}, quiet())
	var calls atomic.Int32
	m.query = func(context.Context) ([]GPU, error) {
		calls.Add(1)
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitFor(t, func() bool { return calls.Load() >= 1 })
	time.Sleep(40 * time.Millisecond)
	if c := calls.Load(); c != 1 {
		t.Fatalf("idle loop polled %d times, want 1", c)
	}
	cancel()
	<-done
}

func TestEnergyExtrapolates(t *testing.T) {
	m := New(config.GPUConfig{ActiveIntervalMs: 1000}, quiet())
	now := time.Now()
	m.record(now.Add(-500*time.Millisecond), []GPU{{PowerW: 100}})
	e := m.EnergyJ()
	if e < 45 || e > 80 {
		t.Fatalf("energy = %v, want about 50 J (100 W for 0.5 s)", e)
	}
	m.record(now.Add(-time.Hour), []GPU{{PowerW: 100}})
	m.mu.Lock()
	m.energyJ, m.lastTime = 7, now.Add(-time.Hour)
	m.mu.Unlock()
	if e := m.EnergyJ(); e != 7 {
		t.Fatalf("energy = %v, want 7 (no extrapolation past the horizon)", e)
	}
}
