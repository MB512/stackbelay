package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/stacks"
)

func imageID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func TestPlanImages(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	opts := CleanupOptions{ImageMinAge: 7 * day, KeepImages: []string{imageID('k')}}
	tests := []struct {
		name       string
		img        docker.DanglingImage
		wantRemove bool
		wantSince  time.Time
	}{
		{"pulled long ago", docker.DanglingImage{ID: imageID('a'), Created: now.Add(-400 * day), LastTagTime: now.Add(-30 * day)},
			true, now.Add(-30 * day)},
		{"built long ago, pulled yesterday", docker.DanglingImage{ID: imageID('b'), Created: now.Add(-400 * day), LastTagTime: now.Add(-day)},
			false, now.Add(-day)},
		{"no tag time: build time counts", docker.DanglingImage{ID: imageID('c'), Created: now.Add(-8 * day)},
			true, now.Add(-8 * day)},
		{"no tag time, built recently", docker.DanglingImage{ID: imageID('d'), Created: now.Add(-2 * day)},
			false, now.Add(-2 * day)},
		{"replaced by an update recently", docker.DanglingImage{ID: imageID('k'), Created: now.Add(-400 * day), LastTagTime: now.Add(-90 * day)},
			false, now.Add(-90 * day)},
		{"exactly the minimum age", docker.DanglingImage{ID: imageID('e'), LastTagTime: now.Add(-7 * day)},
			true, now.Add(-7 * day)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanImages([]docker.DanglingImage{tt.img}, opts, now)
			if len(got) != 1 || got[0].Remove != tt.wantRemove || !got[0].Since.Equal(tt.wantSince) || (got[0].Reason == "") != tt.wantRemove {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestPlanOrphans(t *testing.T) {
	got := PlanOrphans("web", []docker.OrphanContainer{
		{ID: "1", State: "exited"}, {ID: "2", State: "running"}, {ID: "3", State: "created"},
		{ID: "4", State: "paused"}, {ID: "5", State: "restarting"}, {ID: "6", State: "dead"},
	})
	var removed []string
	for _, o := range got {
		if o.Stack != "web" {
			t.Fatalf("stack %q", o.Stack)
		}
		if o.Remove {
			removed = append(removed, o.ID)
		}
	}
	if !slices.Equal(removed, []string{"1", "3", "6"}) {
		t.Fatalf("removed %v, want the stopped ones", removed)
	}
}

// fakeCleaner records removals. Removing an orphan makes its image
// dangling (orphanImage), like on a real host.
type fakeCleaner struct {
	orphans     map[string][]docker.OrphanContainer // by stack dir
	orphanErr   map[string]error
	images      []docker.DanglingImage
	orphanImage map[string]docker.DanglingImage // container id -> image freed by removing it
	failImage   string
	removed     []string
}

func (f *fakeCleaner) DanglingImages(context.Context) ([]docker.DanglingImage, error) {
	return slices.Clone(f.images), nil
}

func (f *fakeCleaner) StackOrphans(_ context.Context, dir string) ([]docker.OrphanContainer, error) {
	return f.orphans[dir], f.orphanErr[dir]
}

func (f *fakeCleaner) RemoveImage(_ context.Context, id string) error {
	if id == f.failImage {
		return errors.New("conflict: image is being used")
	}
	f.removed = append(f.removed, "image "+id)
	return nil
}

func (f *fakeCleaner) RemoveStoppedContainer(_ context.Context, id string) error {
	f.removed = append(f.removed, "container "+id)
	if img, ok := f.orphanImage[id]; ok {
		f.images = append(f.images, img)
	}
	return nil
}

func TestCleanupPipeline(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	list := []stacks.Stack{{Name: "web", Dir: "/s/web"}, {Name: "shared", Dir: "/s/shared"}, {Name: "db", Dir: "/s/db"}}
	newFake := func() *fakeCleaner {
		return &fakeCleaner{
			orphans: map[string][]docker.OrphanContainer{
				"/s/web": {{ID: "c-old", Name: "web-old-1", Service: "old", State: "exited"},
					{ID: "c-cron", Name: "web-cron-1", Service: "cron", State: "running"}},
				"/s/db": {{ID: "c-db", Name: "db-legacy-1", Service: "legacy", State: "exited"}},
			},
			orphanErr: map[string]error{"/s/shared": errors.New("compose project is also used by containers from another directory")},
			images: []docker.DanglingImage{
				{ID: imageID('a'), LastTagTime: old, Size: 100},
				{ID: imageID('k'), LastTagTime: old, Size: 50},
				{ID: imageID('n'), LastTagTime: now.Add(-time.Hour), Size: 70},
			},
			// The image of web's stopped orphan only becomes dangling and
			// unused once the container is gone.
			orphanImage: map[string]docker.DanglingImage{"c-old": {ID: imageID('o'), LastTagTime: old, Size: 5}},
		}
	}
	opts := CleanupOptions{ImageMinAge: 7 * 24 * time.Hour, KeepImages: []string{imageID('k')}}

	f := newFake()
	p := &Pipeline{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
	res, err := p.Cleanup(context.Background(), f, list, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"container c-old", "container c-db", "image " + imageID('a'), "image " + imageID('o')}
	if !slices.Equal(f.removed, want) {
		t.Fatalf("removed %v\nwant %v", f.removed, want)
	}
	if res != (CleanupResult{RemovedContainers: 2, RemovedImages: 2, FreedBytes: 105}) {
		t.Fatalf("result %+v", res)
	}

	// A removal that fails does not stop the cleanup, but fails the job.
	f = newFake()
	f.failImage = imageID('a')
	res, err = p.Cleanup(context.Background(), f, list, opts)
	if err == nil || !strings.Contains(err.Error(), "1 removals failed") {
		t.Fatalf("err = %v", err)
	}
	if res.RemovedImages != 1 || res.RemovedContainers != 2 {
		t.Fatalf("result after a failure %+v", res)
	}

	// Cancelled: nothing more is removed.
	f = newFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Cleanup(ctx, f, list, opts); !errors.Is(err, context.Canceled) || len(f.removed) != 0 {
		t.Fatalf("cancelled: %v, removed %v", err, f.removed)
	}

	// The preview lists the same without removing anything.
	f = newFake()
	plan, err := ListCleanup(context.Background(), f, list, opts, now)
	if err != nil || len(f.removed) != 0 {
		t.Fatalf("preview: %v, removed %v", err, f.removed)
	}
	if len(plan.Orphans) != 3 || len(plan.Images) != 3 || len(plan.Problems) != 1 ||
		!strings.HasPrefix(plan.Problems[0], "shared: ") || plan.RemovableBytes() != 100 {
		t.Fatalf("plan %+v", plan)
	}
}
