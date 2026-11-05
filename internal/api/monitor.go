package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/sysmon"
)

// monitorService is what the Monitor endpoints use of sysmon.
type monitorService interface {
	Snapshot() sysmon.Snapshot
	Processes() []sysmon.Proc
	Groups() []sysmon.Group
	Touch()
	Watch() func()
	Active() bool
	Subscribe() (<-chan struct{}, func())
	Signal(pid int, sig string) error
	Renice(pid, nice int) error
}

// energyService is what they use of energy accounting.
type energyService interface {
	Totals() energy.Totals
	Bill(energy.Totals) energy.BillSummary
	Series(res string, since time.Time) []energy.Point
	ModelEnergy(since time.Time) map[string]float64
	PricePerKWh() float64
	WallJ(j float64) float64
}

// MonitorView is GET /api/monitor and each stream event.
type MonitorView struct {
	Snapshot sysmon.Snapshot     `json:"snapshot"`
	Power    sysmon.Power        `json:"power"`
	Totals   energy.Totals       `json:"totals"`
	Bill     *energy.BillSummary `json:"bill"`
	Alerts   config.AlertConfig  `json:"alerts"` // the configured limits, so the UI colours against them
}

// ProcessView is GET /api/monitor/processes.
type ProcessView struct {
	Processes []sysmon.Proc  `json:"processes"`
	Groups    []sysmon.Group `json:"groups"`
	Active    bool           `json:"active"` // false right after the first poll: data arrives within a sample
}

func (s *server) monitorView() MonitorView {
	snap := s.sysmon.Snapshot()
	v := MonitorView{Snapshot: snap, Power: snap.Power}
	if s.cfg != nil {
		v.Alerts = s.conf().Monitor.Alerts
	}
	if s.energy != nil {
		v.Totals = s.energy.Totals()
		b := s.energy.Bill(v.Totals)
		v.Bill = &b
	}
	return v
}

// secretMasker hides vault values in text the API would otherwise return
// verbatim (process command lines); the session manager implements it.
type secretMasker interface {
	MaskSecrets(strs []string) []string
}

// monitorOff reports (and answers 404 for) a Monitor turned off in the config:
// switching it off also switches off the remote process actions.
func (s *server) monitorOff(w http.ResponseWriter) bool {
	if s.cfg != nil && !s.conf().Monitor.Enabled {
		writeError(w, http.StatusNotFound, "the monitor is turned off (monitor.enabled)")
		return true
	}
	return false
}

// handleMonitor: a one-shot read stays passive (`cerberus energy`, a status
// readout); ?watch=1 (a watching panel or `cerberus top`) switches sampling
// to active mode for the next 60 s.
func (s *server) handleMonitor(w http.ResponseWriter, r *http.Request) {
	if s.sysmon == nil {
		unavailable(w, "monitor")
		return
	}
	if r.URL.Query().Get("watch") == "1" {
		s.sysmon.Touch()
	}
	writeJSON(w, http.StatusOK, s.monitorView())
}

func (s *server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	if s.sysmon == nil {
		unavailable(w, "monitor")
		return
	}
	if s.monitorOff(w) {
		return
	}
	s.sysmon.Touch()
	procs := nonNil(s.sysmon.Processes())
	if s.masker != nil && len(procs) > 0 {
		cmds := make([]string, len(procs))
		for i := range procs {
			cmds[i] = procs[i].Cmd
		}
		for i, c := range s.masker.MaskSecrets(cmds) {
			procs[i].Cmd = c
		}
	}
	writeJSON(w, http.StatusOK, ProcessView{Processes: procs, Groups: nonNil(s.sysmon.Groups()),
		Active: s.sysmon.Active()})
}

// handleSeries: res = minute | hour | day; since = RFC 3339, a Go duration
// ("90m"), or days ("7d"). Defaults: minute → 1 h, hour → 7 d, day → 30 d.
func (s *server) handleSeries(w http.ResponseWriter, r *http.Request) {
	if s.energy == nil {
		unavailable(w, "energy")
		return
	}
	res := r.URL.Query().Get("res")
	def := map[string]time.Duration{"minute": time.Hour, "hour": 7 * 24 * time.Hour, "day": 30 * 24 * time.Hour}
	if _, ok := def[res]; !ok {
		res = "minute"
	}
	since, err := parseSince(r.URL.Query().Get("since"), def[res])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, nonNil(s.energy.Series(res, since)))
}

func parseSince(v string, def time.Duration) (time.Time, error) {
	now := time.Now()
	switch {
	case v == "":
		return now.Add(-def), nil
	case strings.HasSuffix(v, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 0 || n > 3660 {
			return time.Time{}, errors.New("bad since")
		}
		return now.AddDate(0, 0, -n), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("since must be RFC 3339, a duration like 90m, or days like 7d")
}

// handleMonitorStream sends a snapshot event after every sample while the
// client is connected (and keeps the monitor in active mode meanwhile).
func (s *server) handleMonitorStream(w http.ResponseWriter, r *http.Request) {
	if s.sysmon == nil {
		unavailable(w, "monitor")
		return
	}
	release := s.sysmon.Watch()
	defer release()
	ch, cancel := s.sysmon.Subscribe()
	defer cancel()
	rc := startSSE(w)
	send := func() bool {
		b, err := json.Marshal(s.monitorView())
		return err == nil && sendEvent(w, rc, "snapshot", b) == nil
	}
	if !send() {
		return
	}
	ping := time.NewTicker(s.ping)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if sendPing(w, rc) != nil {
				return
			}
		case <-ch:
			if !send() {
				return
			}
		}
	}
}

// handleProcessAction: POST /api/monitor/processes/{pid}/{signal|renice}.
func (s *server) handleProcessAction(w http.ResponseWriter, r *http.Request) {
	if s.sysmon == nil {
		unavailable(w, "monitor")
		return
	}
	if s.monitorOff(w) {
		return
	}
	pid, err := strconv.Atoi(r.PathValue("pid"))
	if err != nil || pid <= 0 {
		writeError(w, http.StatusBadRequest, "bad pid")
		return
	}
	var body struct {
		Sig  string `json:"sig"`
		Nice *int   `json:"nice"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, bodyErrStatus(err), "bad request body")
		return
	}
	actor := principalOf(r).Actor
	as, attributed := s.sysmon.(actorProcessActions)
	switch r.PathValue("action") {
	case "signal":
		noteAudit(r, "signal "+body.Sig)
		if attributed {
			err = as.SignalAs(actor, pid, body.Sig)
		} else {
			err = s.sysmon.Signal(pid, body.Sig)
		}
	case "renice":
		if body.Nice == nil {
			writeError(w, http.StatusBadRequest, "nice is required")
			return
		}
		noteAudit(r, "renice "+strconv.Itoa(*body.Nice))
		if attributed {
			err = as.ReniceAs(actor, pid, *body.Nice)
		} else {
			err = s.sysmon.Renice(pid, *body.Nice)
		}
	default:
		writeError(w, http.StatusNotFound, "unknown action")
		return
	}
	if err != nil {
		writeError(w, processErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// actorProcessActions is the audited form of Signal and Renice
// (*sysmon.Monitor): the host-process record names who asked.
type actorProcessActions interface {
	SignalAs(actor string, pid int, sig string) error
	ReniceAs(actor string, pid, nice int) error
}

func processErrStatus(err error) int {
	switch {
	case errors.Is(err, sysmon.ErrProtected), errors.Is(err, sysmon.ErrNotOwned):
		return http.StatusForbidden
	case errors.Is(err, sysmon.ErrNoProcess):
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// withModelCost fills each model's gateway energy and cost (24 h and month).
func (s *server) withModelCost(ms []engine.ModelInfo) []engine.ModelInfo {
	if s.energy == nil || len(ms) == 0 {
		return ms
	}
	now := time.Now()
	day := s.energy.ModelEnergy(now.Add(-24 * time.Hour))
	month := s.energy.ModelEnergy(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()))
	price := s.energy.PricePerKWh()
	for i := range ms {
		ms[i].EnergyJ24h = s.energy.WallJ(day[ms[i].ID])
		ms[i].EnergyJMonth = s.energy.WallJ(month[ms[i].ID])
		ms[i].CostMXN24h = ms[i].EnergyJ24h / 3.6e6 * price
		ms[i].CostMXNMonth = ms[i].EnergyJMonth / 3.6e6 * price
	}
	return ms
}
