package main

import (
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/gateway"
)

// finetuneDir is where examples are kept: finetune.dir, else DATA/finetune.
func finetuneDir(cfg *config.Config, data string) string {
	if d := strings.TrimSpace(cfg.Finetune.Dir); d != "" {
		return config.ExpandHome(d)
	}
	return filepath.Join(data, "finetune")
}

// openFinetune opens the example store (the folder is made 0700).
func openFinetune(cfg *config.Config, data string, secrets func() []string) (*finetune.Store, error) {
	return finetune.New(finetuneDir(cfg, data), cfg.Finetune.MaxRowKiB, secrets)
}

// keepExchange stores a cloud answer as an example. Conversations with tool
// calls or tool results are skipped: they are agent traces, not chat.
func keepExchange(ft *finetune.Store, ex gateway.Exchange, log *slog.Logger) {
	var raw []struct {
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	if json.Unmarshal(ex.Messages, &raw) != nil || len(raw) == 0 {
		return
	}
	msgs := make([]finetune.Message, 0, len(raw))
	for _, m := range raw {
		if m.Role == "tool" || (len(m.ToolCalls) > 0 && string(m.ToolCalls) != "null") {
			return
		}
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		msgs = append(msgs, finetune.Message{Role: m.Role, Content: text})
	}
	if len(msgs) == 0 {
		return
	}
	if _, err := ft.Add(finetune.Example{Kind: finetune.KindCloud, Model: ex.Model, Session: ex.Session,
		Messages: msgs, Answer: ex.Answer}); err != nil && err != finetune.ErrTooBig {
		log.Warn("finetune: example not kept", "err", err)
	}
}

// contentText reads a message's content: a string, or the text parts of an
// array (images are left out).
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(b.String())
}
