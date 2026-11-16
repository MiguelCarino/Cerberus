package railguard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// PressureFile is the ask railguard writes before it ends a managed
// engine for VRAM (SCOPE "Railguard contract and engine hardening"). It is
// written atomically (tmp + rename) and deleted when the card is back under
// the line or after railguard acted.
const PressureFile = "railguard-pressure.json"

// PressureMaxAge is how old a pressure file may be before a reader ignores
// it (railguard rewrites it while it waits).
const PressureMaxAge = 60 * time.Second

// Pressure is one railguard ask, version 1:
//
//	{"v":1,"time":<unix s>,"resource":"vram","used_mib":N,"total_mib":N,
//	 "target_mib":N,"act_at":<unix s>,"managed_units":[...]}
type Pressure struct {
	V            int      `json:"v"`
	Time         float64  `json:"time"`
	Resource     string   `json:"resource"`
	UsedMiB      int      `json:"used_mib"`
	TotalMiB     int      `json:"total_mib"`
	TargetMiB    int      `json:"target_mib"`
	ActAt        float64  `json:"act_at"`
	ManagedUnits []string `json:"managed_units"`
}

// At is the time railguard wrote the ask.
func (p Pressure) At() time.Time { return unixTime(p.Time) }

// ReadPressure returns the current ask in dir. ok is false when there is
// none, or the file is malformed, of another version or resource, or older
// than PressureMaxAge at now.
func ReadPressure(dir string, now time.Time) (Pressure, bool) {
	if dir == "" {
		return Pressure{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, PressureFile))
	if err != nil {
		return Pressure{}, false
	}
	return parsePressure(raw, now)
}

func parsePressure(raw []byte, now time.Time) (Pressure, bool) {
	var p Pressure
	if json.Unmarshal(raw, &p) != nil || p.V != 1 || p.Resource != "vram" || p.Time <= 0 || p.TargetMiB <= 0 {
		return Pressure{}, false
	}
	age := now.Sub(p.At())
	if age > PressureMaxAge || age < -PressureMaxAge {
		return Pressure{}, false
	}
	return p, true
}

// Entry is one line of railguard-history.jsonl: what railguard did, to
// which process, and why.
type Entry struct {
	Time   time.Time
	Kind   string // memory, vram, game-vram, stuck, panic, agents, vram-asked, vram-shed
	Why    string // "VRAM 97% full"
	Name   string // process name ("llama-server")
	PID    int
	Unit   string // its systemd unit ("cerberus.service")
	GB     float64
	VRAMGB float64
}

// historyTail is how much of the end of the history file is read: the
// file is append-only and only recent lines matter.
const historyTail = 64 << 10

// History returns the history entries in dir with from <= time <= to,
// oldest first. A missing or unreadable file gives none; bad lines are
// skipped.
func History(dir string, from, to time.Time) []Entry {
	if dir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, HistoryFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > historyTail {
		if _, err := f.Seek(-historyTail, io.SeekEnd); err != nil {
			return nil
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, historyTail))
	if err != nil {
		return nil
	}
	return parseHistory(raw, from, to)
}

func parseHistory(raw []byte, from, to time.Time) []Entry {
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 4096), historyTail)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue // empty, or the cut first line of a tail
		}
		var l struct {
			Time   float64 `json:"time"`
			Kind   string  `json:"kind"`
			Why    string  `json:"why"`
			Name   string  `json:"name"`
			PID    int     `json:"pid"`
			Unit   string  `json:"unit"`
			GB     float64 `json:"gb"`
			VRAMGB float64 `json:"vram_gb"`
		}
		if json.Unmarshal(line, &l) != nil || l.Time <= 0 {
			continue
		}
		t := unixTime(l.Time)
		if t.Before(from) || t.After(to) {
			continue
		}
		out = append(out, Entry{Time: t, Kind: l.Kind, Why: l.Why, Name: l.Name, PID: l.PID,
			Unit: l.Unit, GB: l.GB, VRAMGB: l.VRAMGB})
	}
	return out
}

func unixTime(s float64) time.Time {
	sec := int64(s)
	return time.Unix(sec, int64((s-float64(sec))*1e9))
}
