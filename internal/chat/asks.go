package chat

import (
	"slices"
	"strings"

	"carino.systems/cerberus/internal/agent"
)

// Open permission requests across every chat, so a page other than the
// chat itself (the bell, the nav badge) can show and answer them. Answers
// still go through Answer, with the same rules as the chat's own cards.

// OpenAsk is one open permission request and the chat it belongs to.
type OpenAsk struct {
	Chat  string           `json:"chat"`
	Title string           `json:"title"`
	Model string           `json:"model"`
	Req   agent.AskRequest `json:"req"`
}

// OpenAsks returns every open request, grouped by chat (chat id order),
// each chat's in the order they were asked.
func (m *Manager) OpenAsks() []OpenAsk { return m.OpenAsksWhere(nil) }

// OpenAsksWhere is OpenAsks of the chats whose owner keep accepts (nil: all).
func (m *Manager) OpenAsksWhere(keep func(owner string) bool) []OpenAsk {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []OpenAsk{}
	for _, l := range m.lives {
		if keep != nil && !keep(l.chat.Owner) {
			continue
		}
		for _, a := range l.asks {
			out = append(out, OpenAsk{Chat: l.chat.ID, Title: orStr(l.chat.Title, "(no words)"), Model: l.chat.Model, Req: a.req})
		}
	}
	slices.SortStableFunc(out, func(a, b OpenAsk) int { return strings.Compare(a.Chat, b.Chat) })
	return out
}

// AskKeys is the open requests as "chat/req" keys, sorted: a cheap
// signature for the events stream to tell whether anything changed.
func (m *Manager) AskKeys() []string { return m.AskKeysWhere(nil) }

// AskKeysWhere is AskKeys of the chats whose owner keep accepts (nil: all):
// each subscriber of the event stream sees its own chats' requests only.
func (m *Manager) AskKeysWhere(keep func(owner string) bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, l := range m.lives {
		if keep != nil && !keep(l.chat.Owner) {
			continue
		}
		for _, a := range l.asks {
			out = append(out, l.chat.ID+"/"+a.req.ID)
		}
	}
	slices.Sort(out)
	return out
}

// SubscribeAsks returns a channel that gets a poke whenever the open
// requests may have changed (pokes coalesce: the channel holds one), and
// a cancel.
func (m *Manager) SubscribeAsks() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	if m.askSubs == nil {
		m.askSubs = map[chan struct{}]struct{}{}
	}
	m.askSubs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		delete(m.askSubs, ch)
		m.mu.Unlock()
	}
}

// pokeAsksLocked tells the subscribers the open requests may have changed.
func (m *Manager) pokeAsksLocked() {
	for ch := range m.askSubs {
		select {
		case ch <- struct{}{}:
		default: // a poke is already waiting
		}
	}
}
