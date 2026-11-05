package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"syscall"
)

// ErrRoomTimeout is returned when a local model could not be loaded because
// other loaded models stayed busy past engines.wait_for_room_sec. It wraps
// ErrNoRoom (so callers that map ErrNoRoom keep working) and, unlike a plain
// ErrNoRoom, it is transient: a later try may find the GPU free.
var ErrRoomTimeout = fmt.Errorf("%w: timed out waiting for busy models to finish", ErrNoRoom)

type waitHookKey struct{}

// WithWaitHook returns ctx carrying f. Acquire calls f(reason) when the
// request starts waiting (reason e.g. "waiting for qwen/qwen3.8-27b: 1 of 1
// slots busy" or "waiting for room: gemma busy") and f("") once it proceeds
// or gives up. f must be cheap and non-blocking; it may be called from
// Acquire's goroutine.
func WithWaitHook(ctx context.Context, f func(reason string)) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, waitHookKey{}, f)
}

// waitHook is ctx's hook, or a no-op.
func waitHook(ctx context.Context) func(string) {
	if f, ok := ctx.Value(waitHookKey{}).(func(string)); ok && f != nil {
		return f
	}
	return func(string) {}
}

// reCrash matches the words a local engine (or the gateway on its behalf)
// uses when it fell over rather than refused: LM Studio's "Channel Error" and
// "terminated", llama.cpp's CUDA aborts, a dropped connection, and the
// gateway's own 503 text and room timeout.
var reCrash = regexp.MustCompile(`(?i)channel error|\bterminated\b|cuda error|out of memory|` +
	`connection (reset|refused)|broken pipe|unexpected eof|\beof\b|server disconnected|` +
	`local engine failed|exited during load|timed out waiting for busy models`)

// reStatus finds the HTTP status in errors shaped "... (502): message".
var reStatus = regexp.MustCompile(`\((\d{3})\)`)

// IsTransient reports whether err (from a gateway call to a LOCAL engine)
// is a temporary engine failure worth one retry after reload: engine
// crash/"Channel Error"/"terminated", connection reset/refused/EOF from a
// local backend, HTTP 502/503 from the local engine, and ErrRoomTimeout.
// Not: 4xx validation errors, context cancellation by the caller, limits.
func IsTransient(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, ErrRoomTimeout):
		return true
	case errors.Is(err, ErrNoRoom), errors.Is(err, ErrUnknownModel):
		return false
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	msg := err.Error()
	status := 0
	if m := reStatus.FindStringSubmatch(msg); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	return IsTransientText(status, msg)
}

// IsTransientText is IsTransient for a caller that only has a gateway
// reply: its HTTP status (0 when there was none) and error message. 502 and
// 503 are transient unless the message says the model can never fit or no
// engine is configured; 429 (limits), 499 (cancelled) and other 4xx are not,
// except a 4xx whose text is an engine crash (LM Studio answers a crashed
// llama.cpp with 400 "terminated").
func IsTransientText(status int, msg string) bool {
	switch status {
	case http.StatusTooManyRequests, 499, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusPaymentRequired:
		return false
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		if reNeverFits.MatchString(msg) && !reCrash.MatchString(msg) {
			return false
		}
		return true
	}
	return reCrash.MatchString(msg)
}

// reNeverFits matches 503s a retry cannot fix: a model too big for VRAM and
// RAM together (not the room timeout, which reCrash matches), no engine, or
// the caller's own context ending (the gateway answers a load cut short by
// a deadline with 503).
var reNeverFits = regexp.MustCompile(`(?i)not enough free VRAM|no model engine configured|budget|spend cap|context canceled|context deadline exceeded`)
