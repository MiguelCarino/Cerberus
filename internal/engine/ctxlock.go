package engine

import (
	"context"
	"sync"
)

// ctxMutex is a mutex whose Lock can give up when a context ends. The
// manager's load and room locks are held for as long as a load waits for
// room (up to engines.wait_for_room_sec), so a request queued behind one
// must be able to leave when its caller does. The zero value is unlocked.
type ctxMutex struct {
	once sync.Once
	ch   chan struct{}
}

func (l *ctxMutex) c() chan struct{} {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	return l.ch
}

func (l *ctxMutex) Lock()   { l.c() <- struct{}{} }
func (l *ctxMutex) Unlock() { <-l.c() }

func (l *ctxMutex) TryLock() bool {
	select {
	case l.c() <- struct{}{}:
		return true
	default:
		return false
	}
}

// LockCtx locks, or returns ctx's error when ctx ends first. While it waits
// the wait hook in ctx hears reason, and "" when the wait ends.
func (l *ctxMutex) LockCtx(ctx context.Context, reason string) error {
	if l.TryLock() {
		return nil
	}
	hook := waitHook(ctx)
	hook(reason)
	defer hook("")
	select {
	case l.c() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
