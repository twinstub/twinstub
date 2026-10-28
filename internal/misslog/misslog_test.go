package misslog

import (
	"testing"
	"time"
)

func TestRingKeepsNewestFirst(t *testing.T) {
	l := New(3)
	for i := 1; i <= 5; i++ {
		l.Add(time.Now(), map[string]any{"n": i})
	}
	got := l.List(0)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []int{5, 4, 3} {
		if got[i]["n"] != want {
			t.Errorf("entry %d = %v, want %d", i, got[i]["n"], want)
		}
		if _, ok := got[i]["at"].(time.Time); !ok {
			t.Errorf("entry %d has no time", i)
		}
	}
	if got := l.List(1); len(got) != 1 || got[0]["n"] != 5 {
		t.Errorf("List(1) = %v", got)
	}
}

func TestAddCopiesTheBody(t *testing.T) {
	l := New(2)
	body := map[string]any{"path": "/a"}
	l.Add(time.Now(), body)
	body["path"] = "/changed"
	if got := l.List(0)[0]["path"]; got != "/a" {
		t.Errorf("stored path = %v, the caller's map leaked in", got)
	}
	if _, ok := body["at"]; ok {
		t.Error("Add wrote into the caller's map")
	}
}

func TestClear(t *testing.T) {
	l := New(4)
	l.Add(time.Now(), map[string]any{})
	l.Add(time.Now(), map[string]any{})
	if n := l.Clear(); n != 2 {
		t.Errorf("Clear = %d, want 2", n)
	}
	if got := l.List(0); len(got) != 0 {
		t.Errorf("after Clear: %v", got)
	}
}
