package api

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"carino.systems/cerberus/internal/users"
)

// Concurrent sign-ins reserve their attempt before the hash runs, so a
// burst from many addresses checks no more passwords for a name than the
// lockout allows, and a right guess late in the burst does not sign in.
func TestLoginLockoutBurst(t *testing.T) {
	e := newUsersEnv(t)
	t.Cleanup(users.SetCostForTest(100_000)) // slower hashes, as in production
	const n = 40
	var wg sync.WaitGroup
	codes := make([]int, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pw := fmt.Sprintf("wrong password %d", i)
			if i == n-1 {
				pw = "a long password"
			}
			addr := func(r *http.Request) { r.RemoteAddr = fmt.Sprintf("198.51.%d.%d:1", i/200, i%200+1) }
			<-start
			w := e.do("POST", "/api/login", `{"name":"bob","password":"`+pw+`"}`, loginReq("", addr)...)
			codes[i] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()
	hist := map[int]int{}
	for _, c := range codes {
		hist[c]++
	}
	if hist[http.StatusUnauthorized] > users.DefaultLockoutFailures {
		t.Fatalf("%d password guesses were checked (limit %d): %v", hist[http.StatusUnauthorized], users.DefaultLockoutFailures, hist)
	}
	if hist[http.StatusOK]+hist[http.StatusUnauthorized] > users.DefaultLockoutFailures {
		t.Fatalf("more attempts reached the hash than the lockout allows: %v", hist)
	}
}

// Behind a local reverse proxy every sign-in comes from 127.0.0.1: one
// person's typos must not lock everyone else out (the per-name lockout
// still applies to them).
func TestLoginLoopbackNotLimitedPerAddress(t *testing.T) {
	e := newUsersEnv(t)
	loop := func(r *http.Request) { r.RemoteAddr = "127.0.0.1:50000" }
	for i := range 8 {
		w := e.do("POST", "/api/login", fmt.Sprintf(`{"name":"cy","password":"typo %d"}`, i), loginReq("", loop)...)
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusTooManyRequests {
			t.Fatalf("typo %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := e.do("POST", "/api/login", `{"name":"bob","password":"a long password"}`, loginReq("", loop)...); w.Code != http.StatusOK {
		t.Fatalf("another user's sign-in from loopback after cy's typos: %d %s", w.Code, w.Body)
	}
	// cy herself is locked by name.
	if w := e.do("POST", "/api/login", `{"name":"cy","password":"a long password"}`, loginReq("", loop)...); w.Code != http.StatusTooManyRequests {
		t.Fatalf("cy after 8 typos: %d %s", w.Code, w.Body)
	}
	// From another address, the per-address limit still holds.
	far := func(r *http.Request) { r.RemoteAddr = "203.0.113.9:1" }
	blocked := false
	for i := range 8 {
		w := e.do("POST", "/api/login", fmt.Sprintf(`{"name":"name%d","password":"x"}`, i), loginReq("", far)...)
		blocked = blocked || w.Code == http.StatusTooManyRequests
	}
	if !blocked || e.do("POST", "/api/login", `{"name":"bob","password":"a long password"}`, loginReq("", far)...).Code != http.StatusTooManyRequests {
		t.Fatal("a remote address was not limited")
	}
}
