package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"carino.systems/cerberus/internal/engine"
)

// brokenClient is a client connection that went away: every body write
// fails with a broken pipe, before the request context notices.
type brokenClient struct{ h http.Header }

func (b *brokenClient) Header() http.Header { return b.h }
func (b *brokenClient) WriteHeader(int)     {}
func (b *brokenClient) Write([]byte) (int, error) {
	return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
}

// A client hanging up mid-reply is not the engine failing: the model must
// not be marked crashed (that unloads a healthy model and cuts its limit).
func TestClientWriteErrorIsNotACrash(t *testing.T) {
	for _, stream := range []bool{true, false} {
		gw, src, _ := crashGateway(t, true, func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"content":"a"}}]}`)
		})
		body := `{"model":"qwen","messages":[]}`
		if stream {
			body = `{"model":"qwen","stream":true,"messages":[]}`
		}
		gw.Handler().ServeHTTP(&brokenClient{h: http.Header{}},
			httptest.NewRequest("POST", "/chat/completions", strings.NewReader(body)))
		if n := src.crashed.Load(); n != 0 {
			t.Fatalf("stream=%v: client hang-up marked the engine crashed (%d)", stream, n)
		}
	}
}

// genSource hands out endpoints of load 7 and records the crash reports.
type genSource struct {
	fakeSource
	gens chan uint64
}

func (g *genSource) Acquire(ctx context.Context, id string) (engine.Endpoint, func(), error) {
	ep, rel, err := g.fakeSource.Acquire(ctx, id)
	ep.Gen = 7
	return ep, rel, err
}

func (g *genSource) MarkCrashed(string) bool {
	panic("the gateway must report the load (MarkCrashedGen)")
}

func (g *genSource) MarkCrashedGen(_ string, gen uint64) bool {
	g.gens <- gen
	return true
}

// The gateway reports a crash against the load that served the request, so
// the engine can ignore a late report from before a reload.
func TestCrashReportCarriesLoadGen(t *testing.T) {
	var _ genCrashMarker = (*engine.Manager)(nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"terminated"}`)
	}))
	t.Cleanup(up.Close)
	src := &genSource{fakeSource: fakeSource{base: up.URL, local: true}, gens: make(chan uint64, 1)}
	gw := newGateway(src, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rec := postChat(gw, `{"model":"qwen","messages":[]}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d", rec.Code)
	}
	select {
	case gen := <-src.gens:
		if gen != 7 {
			t.Fatalf("gen %d, want 7", gen)
		}
	default:
		t.Fatal("no crash reported")
	}
}
