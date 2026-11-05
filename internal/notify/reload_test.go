package notify

import (
	"slices"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func TestReload(t *testing.T) {
	n, rec, _ := newTest(config.NotifyConfig{Disabled: true, Command: []string{"/bin/old"}})
	n.Send(Message{Title: "a"})
	n.wg.Wait()
	if len(rec.calls) != 0 {
		t.Fatalf("disabled sent %q", rec.calls)
	}
	cfg := config.Defaults()
	cfg.Notify = config.NotifyConfig{Command: []string{"/bin/new", "{title}"}}
	n.Reload(cfg)
	cfg.Notify.Command[0] = "/bin/mutated" // Reload keeps its own copy
	n.Send(Message{Title: "b"})
	n.wg.Wait()
	if len(rec.calls) != 1 || !slices.Equal(rec.calls[0], []string{"/bin/new", "b"}) {
		t.Fatalf("calls after reload = %q", rec.calls)
	}
	n.Reload(nil)
	var nilN *Notifier
	nilN.Reload(cfg)
}
