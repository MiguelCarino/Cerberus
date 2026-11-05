package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/session"
)

// ---------- top ----------

// topCmd shows a refreshing terminal view: CPU, memory, GPU, power, cost,
// top processes and sessions. q (or Ctrl-C) quits; --once prints one frame.
func topCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var once bool
	var every time.Duration
	cfg, _, code := clientFlags("top", args, stderr, func(fs *flag.FlagSet) {
		fs.BoolVar(&once, "once", false, "print one frame and exit")
		fs.DurationVar(&every, "every", 2*time.Second, "refresh interval")
	})
	if cfg == nil {
		return code
	}
	if every <= 0 {
		fmt.Fprintln(stderr, "cerberus: --every must be a positive duration, such as 2s")
		return 2
	}
	c := newClient(cfg)
	frame := func(ctx context.Context) (string, error) {
		var mv api.MonitorView
		var pv api.ProcessView
		var ss []session.Info
		if err := c.do(ctx, "GET", "/api/monitor?watch=1", nil, &mv); err != nil {
			return "", err
		}
		_ = c.do(ctx, "GET", "/api/monitor/processes", nil, &pv)
		_ = c.do(ctx, "GET", "/api/sessions", nil, &ss)
		return renderTop(mv, pv, ss), nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if once || !isTerminal(os.Stdin) {
		if !once { // not a terminal: wait one interval so CPU and process numbers are real
			if _, err := frame(ctx); err != nil {
				fmt.Fprintln(stderr, "cerberus:", err)
				return 1
			}
			time.Sleep(every)
		}
		out, err := frame(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "cerberus:", err)
			return 1
		}
		fmt.Fprint(stdout, out)
		return 0
	}
	restore := rawTerminal()
	defer restore()
	quit := make(chan struct{})
	go func() {
		r := bufio.NewReader(stdin)
		for {
			b, err := r.ReadByte()
			if err != nil || b == 'q' || b == 'Q' || b == 3 {
				close(quit)
				return
			}
		}
	}()
	fmt.Fprint(stdout, "\x1b[?25l") // hide the cursor
	defer fmt.Fprint(stdout, "\x1b[?25h\n")
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		out, err := frame(ctx)
		if err != nil {
			out = "cerberus: " + err.Error() + "\n"
		}
		fmt.Fprint(stdout, "\x1b[H\x1b[2J"+strings.ReplaceAll(out, "\n", "\r\n")+"\r\nq quits")
		select {
		case <-ctx.Done():
			return 0
		case <-quit:
			return 0
		case <-t.C:
		}
	}
}

func renderTop(mv api.MonitorView, pv api.ProcessView, ss []session.Info) string {
	var b strings.Builder
	s, p := mv.Snapshot, mv.Power
	fmt.Fprintf(&b, "CPU  %5.1f%%  %s  %.0f °C  load %.2f %.2f %.2f\n", s.CPU.Pct, clean(s.CPU.Model), s.CPU.TempC, s.CPU.Load[0], s.CPU.Load[1], s.CPU.Load[2])
	fmt.Fprintf(&b, "MEM  %s used of %s   swap %s of %s\n", mib(s.Mem.TotalMiB-s.Mem.AvailMiB), mib(s.Mem.TotalMiB), mib(s.Mem.SwapUsedMiB), mib(s.Mem.SwapTotalMiB))
	for _, g := range s.GPU.GPUs {
		fmt.Fprintf(&b, "GPU  %3d%%  %s  %.0f W  %s of %s  %d °C\n", g.UtilPct, clean(g.Name), g.PowerW, mib(g.MemUsedMiB), mib(g.MemTotalMiB), g.TempC)
	}
	src := "measured"
	if p.Estimated {
		src = "estimated"
	}
	fmt.Fprintf(&b, "PWR  %.0f W at the wall  (CPU %.0f W %s, GPU %.0f W, rest %.0f W est.)\n", p.WallW, p.CPUW, src, p.GPUW, p.RestW)
	if bl := mv.Bill; bl != nil {
		fmt.Fprintf(&b, "COST today %.2f kWh ≈ %.2f %s   month %.1f kWh ≈ %.0f %s   projected +%.0f %s (%.2f/kWh, %s)\n",
			mv.Totals.Today.Wall, bl.TodayCost, bl.Currency, mv.Totals.MonthToDate.Wall, bl.MonthCost, bl.Currency,
			bl.ProjectedMonthCost, bl.Currency, bl.Marginal.PerKWh, clean(bl.Marginal.TierName))
	}
	for _, d := range s.Disks {
		if d.SizeB == 0 {
			continue
		}
		fmt.Fprintf(&b, "DISK %-16s %5.1f%%  %s free  r %s/s w %s/s\n", trunc(strings.Join(d.Mounts, ","), 16), d.UsedPct, bytesH(d.FreeB), bytesH(uint64(d.ReadBps)), bytesH(uint64(d.WriteBps)))
	}
	b.WriteString("\n  PID USER       CPU%   RSS     GPU  GROUP             NAME\n")
	procs := pv.Processes
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].CPUPct > procs[j].CPUPct })
	for i, pr := range procs {
		if i >= 15 {
			break
		}
		mark := " "
		if pr.Cerberus {
			mark = "*"
		}
		fmt.Fprintf(&b, "%s%5d %-9s %5.1f %6s %6s  %-16s  %s\n", mark, pr.PID, trunc(pr.User, 9), pr.CPUPct, mib(pr.RSSMiB), mib(pr.GPUMiB), trunc(pr.Group, 16), trunc(pr.Name, 24))
	}
	if !pv.Active && len(procs) == 0 {
		b.WriteString("  (processes appear after the next sample)\n")
	}
	live := 0
	for _, in := range ss {
		switch in.State {
		case session.StateRunning, session.StateWaiting, session.StateChecking, session.StateStarting, session.StateQueued:
			if live == 0 {
				b.WriteString("\nSESSIONS\n")
			}
			live++
			fmt.Fprintf(&b, "  %-9s %-28s %-10s %.4f kWh ≈ %.2f\n", in.State, trunc(in.Title, 28), clean(in.Agent), in.EnergyJ/3.6e6, in.CostMXN)
		}
	}
	return b.String()
}

func mib(n int) string {
	if n >= 10240 {
		return fmt.Sprintf("%.0fG", float64(n)/1024)
	}
	if n >= 1024 {
		return fmt.Sprintf("%.1fG", float64(n)/1024)
	}
	return fmt.Sprintf("%dM", n)
}

func bytesH(b uint64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(b)/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

// trunc shortens s to n runes, after clean.
func trunc(s string, n int) string {
	s = clean(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// clean replaces control characters (C0, DEL, C1, so ESC and the 8-bit CSI
// too) with '?': process names, cgroup names and mountpoints come from other
// processes and must not drive the terminal (OSC 52, title, screen spoofing).
func clean(s string) string {
	ok := true
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == utf8.RuneError {
			ok = false
			break
		}
	}
	if ok {
		return s // the common case: no copy
	}
	// strings.Map also turns invalid UTF-8 (a raw 0x9b CSI byte) into U+FFFD.
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}

// ---------- energy ----------

func energyCmd(args []string, stdout, stderr io.Writer) int {
	cfg, _, code := clientFlags("energy", args, stderr, nil)
	if cfg == nil {
		return code
	}
	var mv api.MonitorView
	if err := newClient(cfg).do(context.Background(), "GET", "/api/monitor", nil, &mv); err != nil {
		fmt.Fprintln(stderr, "cerberus:", err)
		return 1
	}
	printEnergy(stdout, mv)
	return 0
}

func printEnergy(w io.Writer, mv api.MonitorView) {
	t, p := mv.Totals, mv.Power
	src := "measured (RAPL)"
	if p.Estimated {
		src = "estimated from load (run `cerberus rapl-setup` for measured)"
	}
	fmt.Fprintf(w, "Now: %.0f W at the wall (CPU %.0f W %s, GPU %.0f W, rest %.0f W estimated)\n", p.WallW, p.CPUW, src, p.GPUW, p.RestW)
	row := func(name string, k energy.KWh) {
		fmt.Fprintf(w, "  %-14s %8.2f kWh   (CPU %.2f, GPU %.2f, rest %.2f)\n", name, k.Wall, k.CPU, k.GPU, k.Rest)
	}
	row("today", t.Today)
	row("yesterday", t.Yesterday)
	row("last 7 days", t.Last7Days)
	row("month to date", t.MonthToDate)
	row("last 30 days", t.Last30Days)
	if t.MonthsOfHistory > 0 {
		row(fmt.Sprintf("avg/month (%d)", t.MonthsOfHistory), t.MonthlyAvg12)
	}
	fmt.Fprintf(w, "  projected this month: %.1f kWh\n", t.ProjectedMonthKWh)
	if !t.Loaded {
		fmt.Fprintln(w, "  (history still loading)")
	}
	if b := mv.Bill; b != nil {
		m := b.Marginal
		fmt.Fprintf(w, "\n%s, %s per kWh at the margin: %.3f (%s tier)\n", clean(b.TariffName), clean(b.Currency), m.PerKWh, clean(m.TierName))
		fmt.Fprintf(w, "  today ≈ %.2f   month to date ≈ %.2f   this PC adds ≈ %.2f to this month's bill\n", b.TodayCost, b.MonthCost, b.ProjectedMonthCost)
		if !m.HouseholdSet {
			fmt.Fprintln(w, "  household kWh unknown: set tariff.household_kwh_month (receipt kWh ÷ 2) for the real marginal price")
		}
		switch m.DACRisk {
		case "near":
			fmt.Fprintf(w, "  DAC: the household average (%.0f kWh/month) is near the limit\n", m.DACAvgKWh)
		case "pushes":
			fmt.Fprintf(w, "  DAC: this PC pushes the household (%.0f kWh/month) over the DAC limit\n", m.DACAvgKWh)
		case "already":
			fmt.Fprintln(w, "  DAC: the household is over the DAC limit even without this PC")
		}
		if !b.Verified {
			fmt.Fprintln(w, "  tariff prices are approximate: check them against your receipt and set tariff.verified")
		}
	}
}

// ---------- rapl-setup ----------

// raplSetupCmd prints (never runs) the one-time root setup that lets
// Cerberus read the CPU package energy counter, then reports whether it is
// readable now.
func raplSetupCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rapl-setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	if fs.Parse(args) != nil {
		return 2
	}
	files, _ := filepath.Glob("/sys/class/powercap/intel-rapl:*/energy_uj")
	if len(files) == 0 {
		fmt.Fprintln(stdout, "This machine exposes no RAPL energy counter (/sys/class/powercap/intel-rapl:*).")
		fmt.Fprintln(stdout, "Cerberus estimates CPU watts from load instead (monitor.cpu_idle_w / cpu_max_w).")
		return 0
	}
	readable := true
	for _, f := range files {
		if _, err := os.ReadFile(f); err != nil {
			readable = false
		}
	}
	if readable {
		fmt.Fprintln(stdout, "The CPU energy counter is readable: Cerberus measures CPU watts (RAPL). Nothing to do.")
		return 0
	}
	_, _ = io.WriteString(stdout, `Linux lets only root read the CPU energy counter (RAPL) since 2020, because
on shared machines its fine-grained readings can leak information between
users (the "PLATYPUS" side channel). On a single-user desktop the risk is
negligible; making it readable lets Cerberus measure CPU watts instead of
estimating them.

Run these once (they need your password):

  # make it readable now
  sudo chmod 0444 /sys/class/powercap/intel-rapl:*/energy_uj

  # and at every boot
  echo 'ACTION=="add", SUBSYSTEM=="powercap", KERNEL=="intel-rapl:*", RUN+="/usr/bin/chmod 0444 /sys%p/energy_uj"' | \
    sudo tee /etc/udev/rules.d/90-cerberus-rapl.rules

To undo: sudo rm /etc/udev/rules.d/90-cerberus-rapl.rules and reboot.
Cerberus notices within a minute; no restart needed. Run this command again to check.
`)
	return 0
}
