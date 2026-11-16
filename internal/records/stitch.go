package records

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A line's text keeps only the new turn: what follows the request's last
// assistant message (Line.Prefix counts the messages before it). Stitch
// puts a conversation back together from the earlier lines of the same
// client and session.

// maxStitch bounds a stitched conversation's text.
const maxStitch = 4 << 20

// maxStitchLines bounds how many earlier lines a stitch walks back over.
const maxStitchLines = 5000

// Stitched is a line with the conversation before its new turn.
type Stitched struct {
	Line
	// Earlier is how many earlier lines were joined in.
	Earlier int `json:"earlier"`
	// Missing says the start of the conversation was not found (its lines
	// were swept, written by another client, or never recorded).
	Missing bool `json:"missing,omitempty"`
}

// Stitch returns l with Text.Messages holding the whole conversation as far
// as the records go: each step back takes the latest earlier line of the
// same session, client and system prompt whose message count plus its
// reply makes the count before l's new turn. It reads l's day and the day
// before (a conversation that crossed midnight UTC).
func (s *Store) Stitch(l Line) Stitched {
	out := Stitched{Line: l}
	if s == nil || l.Text == nil || l.Prefix <= 0 {
		return out
	}
	day := l.Time.UTC().Format(time.DateOnly)
	prev := l.Time.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	var cands []Line
	for _, d := range []string{prev, day} {
		cands = append(cands, s.linesOf(d, l)...)
	}
	parts := []string{l.Text.Messages}
	size := len(l.Text.Messages)
	cur := l
	for out.Earlier < maxStitchLines && cur.Prefix > 0 {
		k := pickEarlier(cands, cur)
		if k < 0 {
			out.Missing = true
			break
		}
		c := cands[k]
		cands = append(cands[:k:k], cands[k+1:]...)
		turn := c.Text.Messages
		if c.Text.Reply != "" {
			turn = strings.TrimRight(turn, "\n") + "\nassistant: " + c.Text.Reply
		}
		if gap := cur.Prefix - 1 - c.Messages; gap > 0 {
			turn += "\n…[" + strconv.Itoa(gap) + " messages not recorded]"
		}
		turn = strings.TrimLeft(turn, "\n")
		size += len(turn)
		if size > maxStitch {
			out.Missing = true
			break
		}
		parts = append(parts, turn)
		out.Earlier++
		cur = c
	}
	// parts runs newest first.
	var sb strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(parts[i])
	}
	t := *l.Text
	t.Messages = sb.String()
	out.Text = &t
	return out
}

// pickEarlier is the index of the line cur continues: before cur, with text,
// fewer messages than cur's prefix; an exact count (prefix-1: the earlier
// request plus its reply) wins, then the most messages, then the latest.
// -1 when there is none.
func pickEarlier(cands []Line, cur Line) int {
	best := -1
	for i, c := range cands {
		if c.Time.After(cur.Time) || c.Messages >= cur.Prefix {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := cands[best]
		ce, be := c.Messages == cur.Prefix-1, b.Messages == cur.Prefix-1
		switch {
		case ce != be:
			if ce {
				best = i
			}
		case c.Messages != b.Messages:
			if c.Messages > b.Messages {
				best = i
			}
		case c.Time.After(b.Time):
			best = i
		}
	}
	return best
}

// linesOf reads day's lines that could belong to l's conversation: same
// session, client and system prompt, with text, not after l.
func (s *Store) linesOf(day string, l Line) []Line {
	if !reDay.MatchString(day) {
		return nil
	}
	dir := filepath.Join(s.cfg.Dir, "turns", day)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Line
	for _, e := range ents {
		if !reFile.MatchString(e.Name()) {
			continue
		}
		fh, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		br := bufio.NewReaderSize(fh, 64<<10)
		for {
			b, rerr := br.ReadBytes('\n')
			if len(bytes.TrimSpace(b)) > 0 {
				var c Line
				if json.Unmarshal(b, &c) == nil && c.Text != nil && c.Session == l.Session && c.Client == l.Client &&
					c.Text.System == l.Text.System && !c.Time.After(l.Time) && !sameLine(c, l) {
					out = append(out, c)
				}
			}
			if rerr != nil {
				break
			}
		}
		fh.Close()
	}
	return out
}

// sameLine says c is l itself (read back from disk).
func sameLine(c, l Line) bool {
	return c.Time.Equal(l.Time) && c.Messages == l.Messages && c.Prefix == l.Prefix && c.Text.Messages == l.Text.Messages
}
