package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/sysmon"
)

type fakeMon struct {
	mu      sync.Mutex
	touched int
	watched int
	sigs    []string
	ch      chan struct{}
}

func (f *fakeMon) Snapshot() sysmon.Snapshot {
	return sysmon.Snapshot{CPU: sysmon.CPU{Pct: 12}, Power: sysmon.Power{WallW: 250, CPUSource: "model", Estimated: true}}
}
func (f *fakeMon) Processes() []sysmon.Proc {
	return []sysmon.Proc{{PID: 42, Name: "firefox", Group: "firefox", CPUPct: 5}, {PID: 43, Name: "curl", Cmd: "curl -H token SECRET-VALUE-1"}}
}
func (f *fakeMon) Groups() []sysmon.Group { return []sysmon.Group{{Name: "firefox", Count: 1}} }
func (f *fakeMon) Touch()                 { f.mu.Lock(); f.touched++; f.mu.Unlock() }
func (f *fakeMon) Watch() func() {
	f.mu.Lock()
	f.watched++
	f.mu.Unlock()
	return func() { f.mu.Lock(); f.watched--; f.mu.Unlock() }
}
func (f *fakeMon) Active() bool                         { return true }
func (f *fakeMon) Subscribe() (<-chan struct{}, func()) { return f.ch, func() {} }
func (f *fakeMon) Signal(pid int, sig string) error {
	f.sigs = append(f.sigs, sig)
	switch pid {
	case 1:
		return sysmon.ErrProtected
	case 2:
		return sysmon.ErrNoProcess
	}
	return nil
}
func (f *fakeMon) Renice(pid, nice int) error { f.sigs = append(f.sigs, "renice"); return nil }

type fakeEnergy struct{}

func (fakeEnergy) Totals() energy.Totals {
	return energy.Totals{Today: energy.KWh{Wall: 2}, Loaded: true}
}
func (fakeEnergy) Bill(energy.Totals) energy.BillSummary {
	return energy.BillSummary{TodayCost: 9.4, Currency: "MXN", TariffName: "CFE Tarifa 1", DACLimitKWh: 250}
}
func (fakeEnergy) Series(res string, since time.Time) []energy.Point {
	return []energy.Point{{T: since, Wall: 100}}
}
func (fakeEnergy) ModelEnergy(since time.Time) map[string]float64 {
	return map[string]float64{"m1": 3.6e6}
}
func (fakeEnergy) PricePerKWh() float64    { return 4.7 }
func (fakeEnergy) WallJ(j float64) float64 { return j }

type costModels struct{}

func (costModels) Models(ctx context.Context) []engine.ModelInfo {
	return []engine.ModelInfo{{ID: "m1"}, {ID: "m2"}}
}
func (costModels) Load(ctx context.Context, id string) error   { return nil }
func (costModels) Unload(ctx context.Context, id string) error { return nil }
func (costModels) Delete(ctx context.Context, id string) error { return nil }

func monitorServer(t *testing.T) (*server, *fakeMon, http.Handler) {
	cfg := config.Defaults()
	cfg.Server.Token = "tok"
	cfg.Server.AllowedHosts = []string{"example.com"} // httptest.NewRequest's Host
	s := newServer(Deps{Cfg: cfg})
	fm := &fakeMon{ch: make(chan struct{}, 1)}
	s.sysmon, s.energy, s.models = fm, fakeEnergy{}, costModels{}
	s.ping = time.Hour
	return s, fm, s.handler()
}

func authed(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer tok")
	r.RemoteAddr = "192.0.2.1:1234"
	return r
}

func TestMonitorEndpoints(t *testing.T) {
	_, fm, h := monitorServer(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor", ""))
	var top map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &top) != nil {
		t.Fatalf("monitor: %d %s", w.Code, w.Body)
	}
	for _, k := range []string{"snapshot", "power", "totals", "bill", "alerts"} {
		if _, ok := top[k]; !ok {
			t.Fatalf("missing %q in %s", k, w.Body)
		}
	}
	var bill map[string]any
	_ = json.Unmarshal(top["bill"], &bill)
	for _, k := range []string{"marginal", "today_cost", "month_cost", "projected_month_cost", "tariff_name", "currency", "verified", "dac_limit_kwh"} {
		if _, ok := bill[k]; !ok {
			t.Fatalf("bill missing %q: %v", k, bill)
		}
	}
	if !strings.Contains(string(top["alerts"]), `"disk_pct":90`) || !strings.Contains(string(top["alerts"]), `"nvme_temp_c":70`) {
		t.Fatalf("alerts = %s", top["alerts"])
	}
	if !strings.Contains(string(top["power"]), `"WallW":250`) {
		t.Fatalf("power = %s", top["power"])
	}
	if fm.touched != 0 {
		t.Fatal("a one-shot GET /api/monitor must stay passive")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor?watch=1", ""))
	if w.Code != 200 || fm.touched != 1 {
		t.Fatalf("GET /api/monitor?watch=1 must keep the monitor active: %d touched=%d", w.Code, fm.touched)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor/processes", ""))
	var pv map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &pv)
	if w.Code != 200 || !strings.Contains(string(pv["processes"]), `"PID":42`) || !strings.Contains(string(pv["groups"]), `"name":"firefox"`) {
		t.Fatalf("processes: %d %s", w.Code, w.Body)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor/series?res=hour&since=7d", ""))
	if w.Code != 200 || !strings.HasPrefix(strings.TrimSpace(w.Body.String()), "[") {
		t.Fatalf("series: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor/series?since=yesterday", ""))
	if w.Code != 400 {
		t.Fatalf("bad since: %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/models", ""))
	if !strings.Contains(w.Body.String(), `"EnergyJ24h":3600000`) || !strings.Contains(w.Body.String(), `"CostMXN24h":4.7`) {
		t.Fatalf("models with cost: %s", w.Body)
	}
}

func TestProcessActions(t *testing.T) {
	_, fm, h := monitorServer(t)
	cases := []struct {
		path, body string
		csrf       bool
		want       int
	}{
		{"/api/monitor/processes/42/signal", `{"sig":"term"}`, true, 200},
		{"/api/monitor/processes/1/signal", `{"sig":"kill"}`, true, 403},
		{"/api/monitor/processes/2/signal", `{"sig":"term"}`, true, 404},
		{"/api/monitor/processes/42/renice", `{"nice":10}`, true, 200},
		{"/api/monitor/processes/42/renice", `{}`, true, 400},
		{"/api/monitor/processes/abc/signal", `{"sig":"term"}`, true, 400},
		{"/api/monitor/processes/42/explode", `{}`, true, 404},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		r := authed("POST", c.path, c.body)
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.path, c.body, w.Code, c.want, w.Body)
		}
	}
	if len(fm.sigs) < 3 {
		t.Fatalf("calls = %v", fm.sigs)
	}
	// Cookie-authenticated writes need the CSRF header.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/monitor/processes/42/signal", strings.NewReader(`{"sig":"term"}`))
	r.AddCookie(&http.Cookie{Name: "cerberus_token", Value: "tok"})
	r.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cookie POST without X-Cerberus: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/monitor", nil))
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusFound {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
}

func TestMonitorStream(t *testing.T) {
	_, fm, h := monitorServer(t)
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/monitor/stream", nil)
	req.Header.Set("Authorization", "Bearer tok")
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	events := 0
	fm.ch <- struct{}{}
	for events < 2 {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "event: snapshot") {
			events++
		}
	}
	fm.mu.Lock()
	w := fm.watched
	fm.mu.Unlock()
	if w != 1 {
		t.Fatalf("stream must hold a watch, watched=%d", w)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fm.mu.Lock()
		w = fm.watched
		fm.mu.Unlock()
		if w == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("watch not released after disconnect")
}

type fakeMasker struct{}

func (fakeMasker) MaskSecrets(strs []string) []string {
	for i, v := range strs {
		strs[i] = strings.ReplaceAll(v, "SECRET-VALUE-1", "******")
	}
	return strs
}

func TestProcessCmdlinesMasked(t *testing.T) {
	s, _, h := monitorServer(t)
	s.masker = fakeMasker{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor/processes", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), "SECRET-VALUE-1") || !strings.Contains(w.Body.String(), "curl -H token ******") {
		t.Fatalf("processes: %d %s", w.Code, w.Body)
	}
}

func TestMonitorDisabledRefusesActions(t *testing.T) {
	s, fm, h := monitorServer(t)
	s.cfg.Monitor.Enabled = false
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authed("POST", "/api/monitor/processes/42/signal", `{"sig":"kill"}`))
	if w.Code != http.StatusNotFound || len(fm.sigs) != 0 {
		t.Fatalf("signal with the monitor off: %d calls=%v", w.Code, fm.sigs)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, authed("GET", "/api/monitor/processes", ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("processes with the monitor off: %d", w.Code)
	}
}
