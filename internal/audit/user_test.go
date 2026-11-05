package audit

import (
	"testing"
	"time"
)

func TestUserFilter(t *testing.T) {
	l, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_ = l.Log(Record{Actor: User("ana"), User: "ana", Route: "a"})
	_ = l.Log(Record{Actor: Owner("phone"), User: "bob", Route: "b"})
	_ = l.Log(Record{Actor: Owner("admin"), Route: "c"})
	for filter, want := range map[string]int{"": 3, "ana": 1, "bob": 1, NoUser: 1, "nobody": 0} {
		recs, err := l.QueryFilter(Filter{User: filter})
		if err != nil || len(recs) != want {
			t.Errorf("user %q: %d records, want %d (%v)", filter, len(recs), want, err)
		}
	}
	recs, _ := l.QueryFilter(Filter{Actor: "user", Since: time.Now().Add(-time.Hour)})
	if len(recs) != 1 || recs[0].User != "ana" {
		t.Fatalf("actor kind user: %+v", recs)
	}
}
