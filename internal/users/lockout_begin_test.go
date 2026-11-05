package users

import (
	"testing"
	"time"
)

// Begin counts attempts in flight toward the limit.
func TestLockoutBeginEnd(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := NewLockout(3, time.Minute, func() time.Time { return now })
	for i := range 3 {
		if ok, _ := l.Begin("Bob"); !ok {
			t.Fatalf("begin %d refused", i)
		}
	}
	if ok, _ := l.Begin("bob"); ok {
		t.Fatal("a 4th attempt in flight was allowed")
	}
	l.End("bob", true)
	l.End("bob", true)
	if locked, _ := l.End("bob", true); !locked {
		t.Fatal("3 failures did not lock")
	}
	if ok, _ := l.Begin("bob"); ok {
		t.Fatal("begin while locked")
	}
	now = now.Add(2 * time.Minute)
	if ok, _ := l.Begin("bob"); !ok {
		t.Fatal("begin after the lock")
	}
	l.End("bob", false)
	l.Clear("bob")
	if len(l.m) != 0 {
		t.Fatalf("entries left: %v", l.m)
	}
}
