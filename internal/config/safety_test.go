package config

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSetTokenStampsAndSaves: SetToken records when the token was made and
// Save persists both, so the doctor can report the real token age.
func TestSetTokenStampsAndSaves(t *testing.T) {
	c := Defaults()
	c.SetPath(filepath.Join(t.TempDir(), "config.json"))
	before := time.Now().Add(-time.Second)
	c.SetToken("tok-1")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenSnapshot() != "tok-1" || got.TokenCreatedAt().Before(before) {
		t.Fatalf("token %q created %v", got.TokenSnapshot(), got.TokenCreatedAt())
	}
}

// TestLoadFirstRunStampsToken: a new config gets a token and its time.
func TestLoadFirstRunStampsToken(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenSnapshot() == "" || c.TokenCreatedAt().IsZero() {
		t.Fatalf("first run: token %q created %v", c.TokenSnapshot(), c.TokenCreatedAt())
	}
}

// TestTokenAccessRace exercises a rotation racing token derivations and
// saves; run with -race.
func TestTokenAccessRace(t *testing.T) {
	c := Defaults()
	c.SetPath(filepath.Join(t.TempDir(), "config.json"))
	c.SetToken("a")
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				switch i {
				case 0:
					c.SetToken(NewToken())
				case 1:
					if tok := c.SessionAgentToken("s1"); tok != "" {
						c.VerifySessionAgentToken(tok)
					}
					_ = c.AgentToken()
				case 2:
					if j%10 == 0 {
						_ = c.Save()
					}
				}
			}
		}()
	}
	wg.Wait()
}
