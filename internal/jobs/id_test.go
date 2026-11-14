package jobs

import (
	"sort"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	var ids []string
	seen := map[string]bool{}
	for i := range 100 {
		id, err := NewID(t0.Add(time.Duration(i) * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if !ValidJobID(id) {
			t.Fatalf("invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids do not sort by time")
	}
	// Known timestamp prefix: the first 10 characters encode the milliseconds.
	id, _ := NewID(time.UnixMilli(0))
	if id[:10] != "0000000000" {
		t.Fatalf("epoch prefix: %s", id)
	}
	id, _ = NewID(time.UnixMilli(1))
	if id[:10] != "0000000001" {
		t.Fatalf("1ms prefix: %s", id)
	}
}
