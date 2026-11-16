package evals

// The evals engine's only coupling to the model engine (internal/engine):
// its rule for which judge failures are worth a retry.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"carino.systems/cerberus/internal/engine"
)

// ModelError is a judge or feedback call the gateway answered with an
// error: its HTTP status and message. A ModelCaller should return one, so
// judgeOne can ask the engine's rule whether a retry may work.
type ModelError struct {
	Status  int
	Message string
}

func (m *ModelError) Error() string {
	return fmt.Sprintf("model call failed (%d): %s", m.Status, m.Message)
}

// transientModelErr: err is a typed gateway answer the engine calls
// temporary (engine.IsTransientText: 502/503 unless the model never fits
// or a budget ran out, a crash text). Anything else, a cancel included,
// is not.
func transientModelErr(err error) bool {
	var me *ModelError
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &me) {
		return false
	}
	return engine.IsTransientText(me.Status, me.Message)
}

// sleepCtx waits d; false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
