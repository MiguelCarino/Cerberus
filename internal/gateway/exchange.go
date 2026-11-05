package gateway

import (
	"encoding/json"
	"strings"
)

// maxCaptureText bounds the answer kept from one exchange.
const maxCaptureText = 256 << 10

// Exchange is one cloud chat completion that went through the gateway: the
// request's messages and the answer text. It feeds the fine-tuning examples
// (a stronger model as the teacher) when that is turned on.
type Exchange struct {
	Model    string
	Session  string
	Client   string
	Messages json.RawMessage
	Answer   string
}

// WithExchange calls fn after each successful cloud chat completion while
// on() is true (on is asked per request, so the setting applies live).
func WithExchange(on func() bool, fn func(Exchange)) Option {
	return func(g *Gateway) { g.exOn, g.exFn = on, fn }
}

// captureFor reports whether this call's answer should be kept.
func (g *Gateway) captureFor(local bool, path string) bool {
	return g.exFn != nil && g.exOn != nil && !local && path == "/chat/completions" && g.exOn()
}

// addText appends answer text, up to the cap.
func (c *call) addText(s string) {
	if s == "" || c.text.Len() >= maxCaptureText {
		return
	}
	if room := maxCaptureText - c.text.Len(); len(s) > room {
		s = s[:room]
	}
	c.text.WriteString(s)
}

// bodyText reads the answer of a non-streamed chat completion.
func bodyText(b []byte) string {
	var res struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &res) != nil || len(res.Choices) == 0 {
		return ""
	}
	return res.Choices[0].Message.Content
}

// deliver hands a finished capture to the callback.
func (g *Gateway) deliver(c *call) {
	if !c.capture || c.rec.Status != 200 || c.rec.Error != "" {
		return
	}
	answer := strings.TrimSpace(c.text.String())
	if answer == "" || len(c.messages) == 0 {
		return
	}
	g.exFn(Exchange{Model: c.rec.Model, Session: c.rec.SessionID, Client: c.rec.Client, Messages: c.messages, Answer: answer})
}
