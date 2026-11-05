package session

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"carino.systems/cerberus/internal/platform"
)

const (
	maxNameRunes = 80
	maxTags      = 8
	minPriority  = -10
	maxPriority  = 10
	maxBulkIDs   = 100
	maxLimitMin  = 7 * 24 * 60 // a week
	maxLimitMiB  = 1 << 20     // a TiB of output
	scopeKillTO  = 5 * time.Second
)

var (
	tagRe       = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)
	memoryMaxRe = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?[KMGT]?|[0-9]{1,3}(?:\.[0-9]+)?%|infinity)$`)
)

// Bulk operation kinds.
const (
	BulkInput    = "input"
	BulkFollowUp = "followup"
	BulkStop     = "stop"
	BulkResume   = "resume"
	BulkAccept   = "accept"
	BulkDiscard  = "discard"
	BulkDelete   = "delete"
	BulkChecks   = "checks"
	BulkPriority = "priority"
	BulkTag      = "tag"
	BulkUntag    = "untag"
	BulkPin      = "pin"
	BulkUnpin    = "unpin"
)

// Bulk results.
const (
	BulkOK      = "ok"
	BulkSkipped = "skipped"
	BulkError   = "error"
)

// BulkOp is one action for many sessions.
type BulkOp struct {
	Kind     string // input|followup|stop|resume|accept|discard|delete|checks|priority|tag|untag|pin|unpin
	Data     string // input bytes, follow-up or resume text
	Push     bool   // accept: push after merging
	Force    bool   // delete: also sessions whose worktree has unaccepted work
	Message  string // accept: commit message (default each session's title)
	Priority int    // priority
	Tag      string // tag, untag
}

// BulkResult is what Bulk did with one session.
type BulkResult struct {
	ID     string
	Result string // ok | skipped | error
	Detail string
}

// queueCompare orders queued sessions: pinned first, then higher priority,
// then older.
func queueCompare(a, b Info) int {
	if a.Pinned != b.Pinned {
		if a.Pinned {
			return -1
		}
		return 1
	}
	if a.Priority != b.Priority {
		return b.Priority - a.Priority
	}
	if c := a.Created.Compare(b.Created); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// normalizeTags lowercases and trims tags, drops duplicates and empties,
// and checks the [a-z0-9-]{1,24} form and the count.
func normalizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || slices.Contains(out, t) {
			continue
		}
		if !tagRe.MatchString(t) {
			return nil, fmt.Errorf("invalid tag %q: use 1-24 of a-z, 0-9 and -", t)
		}
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, fmt.Errorf("at most %d tags per session", maxTags)
	}
	return out, nil
}

// normalizeName trims a label and refuses control characters and long ones.
func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if strings.ContainsFunc(name, unicode.IsControl) {
		return "", errors.New("name must be a single line of text")
	}
	if n := len([]rune(name)); n > maxNameRunes {
		return "", fmt.Errorf("name is too long (%d characters, at most %d)", n, maxNameRunes)
	}
	return name, nil
}

func validatePriority(p int) error {
	if p < minPriority || p > maxPriority {
		return fmt.Errorf("priority %d is out of range (%d..%d)", p, minPriority, maxPriority)
	}
	return nil
}

// validateLimits checks a session's limits (0 / "" = default).
func validateLimits(l Limits) error {
	if l.MaxMinutes < 0 || l.MaxMinutes > maxLimitMin {
		return fmt.Errorf("max minutes must be 0..%d", maxLimitMin)
	}
	if l.MaxOutputMiB < 0 || l.MaxOutputMiB > maxLimitMiB {
		return fmt.Errorf("max output must be 0..%d MiB", maxLimitMiB)
	}
	if l.MemoryMax != "" && !memoryMaxRe.MatchString(l.MemoryMax) {
		return fmt.Errorf("invalid memory limit %q: use a size like 4G or 512M, a percentage, or infinity", l.MemoryMax)
	}
	return nil
}

// normalizeMeta cleans and checks the metadata of a new session.
func normalizeMeta(name *string, tags *[]string, priority int, limits Limits) error {
	n, err := normalizeName(*name)
	if err != nil {
		return err
	}
	*name = n
	t, err := normalizeTags(*tags)
	if err != nil {
		return err
	}
	if len(t) == 0 {
		t = nil
	}
	*tags = t
	if err := validatePriority(priority); err != nil {
		return err
	}
	return validateLimits(limits)
}

// Update changes a session's name, tags, pin, priority or limits. It
// persists to meta.json, emits a state event and records the change in the
// feed. New limits apply to the running agent at once where they can (time
// and output); the memory cap applies from the next run.
func (m *Manager) Update(id string, p Patch) (Info, error) {
	var name string
	var tags []string
	var err error
	if p.Name != nil {
		if name, err = normalizeName(*p.Name); err != nil {
			return Info{}, err
		}
	}
	if p.Tags != nil {
		if tags, err = normalizeTags(*p.Tags); err != nil {
			return Info{}, err
		}
		if len(tags) == 0 {
			tags = nil
		}
	}
	if p.Priority != nil {
		if err := validatePriority(*p.Priority); err != nil {
			return Info{}, err
		}
	}
	if p.Limits != nil {
		if err := validateLimits(*p.Limits); err != nil {
			return Info{}, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return Info{}, err
	}
	var what []string
	requeue := false
	if p.Name != nil && name != s.info.Name {
		s.info.Name = name
		what = append(what, "renamed to "+orDefault(name, s.info.Title))
	}
	if p.Tags != nil && !slices.Equal(tags, s.info.Tags) {
		s.info.Tags = tags
		what = append(what, "tags "+orDefault(strings.Join(tags, ", "), "cleared"))
	}
	if p.Pinned != nil && *p.Pinned != s.info.Pinned {
		s.info.Pinned = *p.Pinned
		requeue = true
		what = append(what, map[bool]string{true: "pinned", false: "unpinned"}[s.info.Pinned])
	}
	if p.Priority != nil && *p.Priority != s.info.Priority {
		s.info.Priority = *p.Priority
		requeue = true
		what = append(what, fmt.Sprintf("priority %d", s.info.Priority))
	}
	if p.Limits != nil && *p.Limits != s.info.Limits {
		s.info.Limits = *p.Limits
		what = append(what, "limits "+limitsText(s.info.Limits))
		if s.run != nil {
			m.applyLimitsLocked(s, s.run)
		}
	}
	if len(what) > 0 {
		m.changedLocked(s)
		m.recordLocked(s, FeedUpdate, strings.Join(what, "; "))
		if requeue && s.info.State == StateQueued {
			m.signal()
		}
	}
	return m.snapshotLocked(s), nil
}

// limitsText describes limits for the feed.
func limitsText(l Limits) string {
	var parts []string
	if l.MaxMinutes > 0 {
		parts = append(parts, fmt.Sprintf("%d min", l.MaxMinutes))
	}
	if l.MemoryMax != "" {
		parts = append(parts, "memory "+l.MemoryMax)
	}
	if l.MaxOutputMiB > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB output", l.MaxOutputMiB))
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, ", ")
}

// Kill ends a stuck agent at once: SIGKILL to its process group, every
// process left in its terminal session and, when it runs in a systemd
// scope, the whole scope. Running checks are cancelled; a queued session is
// taken out of the queue. The session ends as stopped with reason "killed".
func (m *Manager) Kill(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	switch {
	case s.run != nil:
		s.stopping = true
		s.stopReason = "killed"
		killRun(s.run)
	case s.checkCancel != nil:
		s.stopping = true
		s.stopReason = "killed"
		s.checkCancel()
	case s.info.State == StateQueued:
		s.info.WaitingReason = pendingStopReason(s, "killed")
		m.setStateLocked(s, StateStopped)
	case live(s.info.State):
		s.stopping = true
		s.stopReason = "killed"
	default:
		return ErrNotRunning
	}
	return nil
}

// killRun sends SIGKILL to r's whole tree now (see Kill).
func killRun(r *run) {
	select {
	case <-r.waitDone:
		return // reaped: wait() already killed the session
	default:
	}
	if runtime.GOOS == "windows" {
		_ = r.cmd.Process.Kill()
		return
	}
	platform.KillSession(r.cmd.Process.Pid)
	if r.unit != "" {
		unit := r.unit + ".scope"
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), scopeKillTO)
			defer cancel()
			_ = exec.CommandContext(ctx, "systemctl", "--user", "kill", "--signal=SIGKILL", unit).Run()
		}()
	}
}

// Bulk runs one action on up to 100 sessions and reports each outcome.
// Actions run one session after another, except input and follow-up, which
// go to every session at once. Accept is strictly sequential and never runs
// alongside a fleet accept, so pushes to one remote never race.
func (m *Manager) Bulk(ctx context.Context, ids []string, op BulkOp) []BulkResult {
	uniq := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(uniq, id) {
			uniq = append(uniq, id)
		}
	}
	res := make([]BulkResult, len(uniq))
	for i, id := range uniq {
		res[i].ID = id
	}
	fail := func(msg string) []BulkResult {
		for i := range res {
			res[i].Result, res[i].Detail = BulkError, msg
		}
		return res
	}
	if len(uniq) > maxBulkIDs {
		return fail(fmt.Sprintf("at most %d sessions per bulk action", maxBulkIDs))
	}
	var tag string
	switch op.Kind {
	case BulkInput, BulkFollowUp:
		if strings.TrimSpace(op.Data) == "" && (op.Kind == BulkFollowUp || op.Data == "") {
			return fail(op.Kind + " text is empty")
		}
		var wg sync.WaitGroup
		for i := range res {
			wg.Go(func() { m.bulkOne(ctx, &res[i], op, "") })
		}
		wg.Wait()
		return res
	case BulkPriority:
		if err := validatePriority(op.Priority); err != nil {
			return fail(err.Error())
		}
	case BulkTag, BulkUntag:
		t, err := normalizeTags([]string{op.Tag})
		if err != nil {
			return fail(err.Error())
		}
		if len(t) == 0 {
			return fail("tag is empty")
		}
		tag = t[0]
	case BulkAccept:
		m.acceptMu.Lock()
		defer m.acceptMu.Unlock()
	case BulkStop, BulkResume, BulkDiscard, BulkDelete, BulkChecks, BulkPin, BulkUnpin:
	default:
		return fail(fmt.Sprintf("unknown bulk action %q", op.Kind))
	}
	for i := range res {
		if ctx.Err() != nil {
			res[i].Result, res[i].Detail = BulkSkipped, "cancelled"
			continue
		}
		m.bulkOne(ctx, &res[i], op, tag)
	}
	return res
}

// bulkOne applies op to one session and fills r.
func (m *Manager) bulkOne(ctx context.Context, r *BulkResult, op BulkOp, tag string) {
	info, ok := m.Get(r.ID)
	if !ok {
		r.Result, r.Detail = BulkError, ErrNotFound.Error()
		return
	}
	skip := func(detail string) { r.Result, r.Detail = BulkSkipped, detail }
	var err error
	switch op.Kind {
	case BulkInput:
		hidden := m.InputHidden(r.ID) // before the input: a prompt restores echo on Enter
		if err = m.Input(r.ID, []byte(op.Data)); err == nil {
			for _, text := range inputTexts(op.Data, hidden) {
				m.Record(r.ID, FeedInput, text)
			}
		}
	case BulkFollowUp:
		err = m.FollowUp(ctx, r.ID, op.Data)
	case BulkStop:
		err = m.Stop(r.ID)
	case BulkResume:
		switch info.State {
		case StatePassed, StateFailed, StateStopped, StateError:
			err = m.Resume(ctx, r.ID, op.Data)
		default:
			skip("session is " + info.State)
			return
		}
	case BulkAccept:
		if info.State != StatePassed {
			skip("session is " + info.State)
			return
		}
		if info.Workspace != WorkspaceRepo {
			skip("a " + info.Workspace + " session has nothing to accept")
			return
		}
		var sha string
		sha, err = m.Accept(ctx, r.ID, AcceptOpts{Message: op.Message, Push: op.Push})
		if err != nil && sha != "" { // accepted; only the push failed
			r.Result, r.Detail = BulkOK, err.Error()
			return
		}
		if err == nil {
			r.Detail = sha
		}
	case BulkDiscard:
		if final(info.State) {
			skip("session is " + info.State)
			return
		}
		err = m.Discard(r.ID)
	case BulkDelete:
		err = m.Delete(ctx, r.ID, op.Force)
		if IsUnaccepted(err) {
			skip(err.Error())
			return
		}
	case BulkChecks:
		err = m.RunChecks(ctx, r.ID)
	case BulkPriority:
		_, err = m.Update(r.ID, Patch{Priority: &op.Priority})
	case BulkPin, BulkUnpin:
		pin := op.Kind == BulkPin
		_, err = m.Update(r.ID, Patch{Pinned: &pin})
	case BulkTag, BulkUntag:
		tags := slices.Clone(info.Tags)
		has := slices.Contains(tags, tag)
		switch {
		case op.Kind == BulkTag && has, op.Kind == BulkUntag && !has:
			r.Result = BulkOK // already as asked
			return
		case op.Kind == BulkTag:
			tags = append(tags, tag)
		default:
			tags = slices.DeleteFunc(tags, func(t string) bool { return t == tag })
		}
		_, err = m.Update(r.ID, Patch{Tags: &tags})
	}
	switch {
	case err == nil:
		r.Result = BulkOK
	case errors.Is(err, ErrNotRunning):
		skip("not running")
	case errors.Is(err, ErrBusy):
		skip(err.Error())
	default:
		r.Result, r.Detail = BulkError, err.Error()
	}
}
