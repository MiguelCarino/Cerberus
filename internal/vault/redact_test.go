package vault

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A rotated or deleted value stays in Values: sessions started before the
// change still hold it and must keep masking it.
func TestValuesKeepRetiredSecrets(t *testing.T) {
	cfg, _ := testConfig(t, BackendFile)
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"a", "sk-old-111111"}, {"b", "tok-bbbbbbbb"}} {
		if err := v.Set(kv[0], kv[1], ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.Set("a", "sk-new-222222", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete("b"); err != nil {
		t.Fatal(err)
	}
	got := v.Values()
	for _, want := range []string{"sk-old-111111", "sk-new-222222", "tok-bbbbbbbb"} {
		if !slices.Contains(got, want) {
			t.Errorf("Values() lacks %q: %v", want, got)
		}
	}
	if s, err := v.Get("b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted secret still readable: %q %v", s, err)
	}
}

// After a failed full load, Values does not run secret-tool again on every
// call; it waits retryLoad.
func TestValuesBacksOffAfterFailedLoad(t *testing.T) {
	var fail atomic.Bool
	var searches atomic.Int32
	run := func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
		if args[0] == "search" {
			if fail.Load() {
				searches.Add(1)
				return nil, []byte("locked"), errors.New("exit status 2")
			}
		}
		return nil, nil, nil
	}
	cfg, _ := testConfig(t, BackendSecretService)
	v, err := Open(cfg, quietLog(), WithRunner(run))
	if err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	for range 5 {
		v.Values()
	}
	if n := searches.Load(); n != 1 {
		t.Fatalf("searches after 5 Values() = %d, want 1", n)
	}
	v.mu.Lock()
	v.loaded = time.Now().Add(-retryLoad - time.Second)
	v.mu.Unlock()
	v.Values()
	if n := searches.Load(); n != 2 {
		t.Fatalf("searches after the back-off = %d, want 2", n)
	}
}

// A cached Get answers while a slow backend call is in progress.
func TestCachedGetDoesNotWaitForBackend(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	entered := make(chan struct{})
	run := func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
		if args[0] == "lookup" {
			n := args[len(args)-1]
			if n == "slow" {
				once.Do(func() { close(entered) })
				<-release
			}
			return []byte("tok-" + n), nil, nil
		}
		return nil, nil, nil
	}
	cfg, _ := testConfig(t, BackendSecretService)
	v, err := Open(cfg, quietLog(), WithRunner(run))
	if err != nil {
		t.Fatal(err)
	}
	if v.Lookup("fast") != "tok-fast" {
		t.Fatal("prime")
	}
	done := make(chan struct{})
	go func() { v.Lookup("slow"); close(done) }()
	<-entered
	got := make(chan string, 1)
	go func() { got <- v.Lookup("fast") }()
	select {
	case s := <-got:
		if s != "tok-fast" {
			t.Fatalf("got %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cached Get blocked behind a backend call")
	}
	close(release)
	<-done
}
