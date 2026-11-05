package flows

// The flows engine's only coupling to the model engine (internal/engine):
// its wait hook (a flow call shows "waiting for the model" while the
// engine queues it) and its rule for which failures are worth a retry.

import (
	"fmt"

	"carino.systems/cerberus/internal/engine"
)

// withWaitHook returns ctx carrying f: the engine calls f(reason) while
// it holds a call made with ctx and f("") once the call proceeds or gives
// up. The ModelCaller must hand ctx to the gateway as the request context.
// A variable so tests can see the hook from a fake ModelCaller.
var withWaitHook = engine.WithWaitHook

// engineTransient is engine.IsTransient.
func engineTransient(err error) bool { return engine.IsTransient(err) }

// engineTransientText is engine.IsTransientText.
func engineTransientText(status int, msg string) bool { return engine.IsTransientText(status, msg) }

// ModelError is a model call the gateway answered with an error: its
// HTTP status and message. A ModelCaller should return one (the runner
// asks the engine's rule whether it is worth a retry).
type ModelError struct {
	Status  int
	Message string
}

func (m *ModelError) Error() string {
	return fmt.Sprintf("model call failed (%d): %s", m.Status, m.Message)
}
