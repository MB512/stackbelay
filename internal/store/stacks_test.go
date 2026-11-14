package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestSyncStacks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	steps := []struct {
		discovered []string
		want       map[string]bool // name -> present
	}{
		{[]string{"web", "db"}, map[string]bool{"db": true, "web": true}},
		{[]string{"web"}, map[string]bool{"db": false, "web": true}},                          // db vanished: kept, not present
		{[]string{"web", "db", "new"}, map[string]bool{"db": true, "new": true, "web": true}}, // db is back
		{nil, map[string]bool{"db": false, "new": false, "web": false}},
	}
	ids := map[string]int64{}
	for i, step := range steps {
		if err := s.SyncStacks(ctx, step.discovered); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListStacks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, st := range list {
			got[st.Name] = st.Present
			if !st.Enabled {
				t.Errorf("step %d: %s not enabled by default", i, st.Name)
			}
			if id, ok := ids[st.Name]; ok && id != st.ID {
				t.Errorf("step %d: id of %s changed from %d to %d", i, st.Name, id, st.ID)
			}
			ids[st.Name] = st.ID
		}
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("step %d: got %v, want %v", i, got, step.want)
		}
	}
}

func TestStackByNameAndEnabled(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	st, err := s.StackByName(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStackEnabled(ctx, st.ID, false); err != nil {
		t.Fatal(err)
	}
	st, _ = s.StackByName(ctx, "web")
	if st.Enabled {
		t.Fatal("stack still enabled")
	}
	// Disabled state survives a sync.
	if err := s.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.StackByName(ctx, "web"); st.Enabled {
		t.Fatal("sync re-enabled the stack")
	}

	if _, err := s.StackByName(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.SetStackEnabled(ctx, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
