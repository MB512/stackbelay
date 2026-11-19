package jobs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const testJobID = "01JCZ8G3T4S5V6W7X8Y9Z0ABCD"

func TestSpecRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Spec{ID: testJobID, Type: TypeRestore, Stack: "web", SnapshotID: "0123abcd", NoStart: true, PinImages: true,
		CarryOverJobs: "data/jobs", Trigger: "manual", Requester: "alice"}
	if err := WriteSpec(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpec(dir)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = 1
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestUpdateSpecRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Spec{ID: testJobID, Type: TypeUpdate, Stack: "web", OnlyIfNew: true, NoPreStop: true, CarryOverJobs: "data/jobs", Trigger: "manual"}
	if err := WriteSpec(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpec(dir)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = 1
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestComposeApplySpecRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Spec{ID: testJobID, Type: TypeComposeApply, Stack: "web", ComposeSHA256: strings.Repeat("a", 64),
		ComposeBaseSHA256: strings.Repeat("b", 64), NoBackup: true}
	if err := WriteSpec(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpec(dir)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = 1
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestControlSpecRoundTrip(t *testing.T) {
	for _, typ := range []Type{TypeStop, TypeStart, TypeRestart} {
		dir := t.TempDir()
		want := Spec{ID: testJobID, Type: typ, Stack: "web", Trigger: "manual", Requester: "carol"}
		if err := WriteSpec(dir, want); err != nil {
			t.Fatal(err)
		}
		got, err := ReadSpec(dir)
		if err != nil {
			t.Fatal(err)
		}
		want.Version = 1
		if !reflect.DeepEqual(got, want) || !got.Type.Control() {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
	if TypeBackup.Control() || TypeUpdate.Control() {
		t.Fatal("Control() true for a job with backup")
	}
}

func TestReadSpecRejects(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"not json", `{`},
		{"unknown field", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","command":"rm -rf /"}`},
		{"wrong version", `{"version":2,"id":"` + testJobID + `","type":"backup","stack":"web"}`},
		{"bad id", `{"version":1,"id":"../x","type":"backup","stack":"web"}`},
		{"bad type", `{"version":1,"id":"` + testJobID + `","type":"prune","stack":"web"}`},
		{"stack path", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"../etc"}`},
		{"reserved stack", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web.old-1"}`},
		{"restore without snapshot", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web"}`},
		{"restore with flag-like snapshot", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"--help"}`},
		{"backup with restore option", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","no_start":true}`},
		{"backup with pinned images", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","pin_images":true}`},
		{"only-if-new on a compose apply", `{"version":1,"id":"` + testJobID + `","type":"compose-apply","stack":"web","only_if_new":true,"compose_sha256":"` + strings.Repeat("a", 64) + `","compose_base_sha256":"` + strings.Repeat("b", 64) + `"}`},
		{"no-pre-stop on a backup", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","no_pre_stop":true}`},
		{"restore with backup option", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","pre_stop":true}`},
		{"carry-over outside the stack", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","carry_over_jobs":"../manager/data"}`},
		{"absolute carry-over", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","carry_over_jobs":"/data/jobs"}`},
		{"update with pre-stop option", `{"version":1,"id":"` + testJobID + `","type":"update","stack":"web","pre_stop":true}`},
		{"update with snapshot", `{"version":1,"id":"` + testJobID + `","type":"update","stack":"web","snapshot_id":"0123abcd"}`},
		{"update with bad carry-over", `{"version":1,"id":"` + testJobID + `","type":"update","stack":"web","carry_over_jobs":"../x"}`},
		{"no-backup on a backup", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","no_backup":true}`},
		{"no-backup on a restore", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","no_backup":true}`},
		{"compose-apply without checksums", `{"version":1,"id":"` + testJobID + `","type":"compose-apply","stack":"web"}`},
		{"compose checksum on an update", `{"version":1,"id":"` + testJobID + `","type":"update","stack":"web","compose_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{"compose-apply with bad checksum", `{"version":1,"id":"` + testJobID + `","type":"compose-apply","stack":"web","compose_sha256":"../x","compose_base_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{"carry-over on a backup", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","carry_over_jobs":"data/jobs"}`},
		{"stop with pre-stop option", `{"version":1,"id":"` + testJobID + `","type":"stop","stack":"web","pre_stop":true}`},
		{"start with snapshot", `{"version":1,"id":"` + testJobID + `","type":"start","stack":"web","snapshot_id":"0123abcd"}`},
		{"restart with carry-over", `{"version":1,"id":"` + testJobID + `","type":"restart","stack":"web","carry_over_jobs":"data/jobs"}`},
		{"restart without backup flag", `{"version":1,"id":"` + testJobID + `","type":"restart","stack":"web","no_backup":true}`},
		{"stop with update option", `{"version":1,"id":"` + testJobID + `","type":"stop","stack":"web","only_if_new":true}`},
		{"start with compose checksum", `{"version":1,"id":"` + testJobID + `","type":"start","stack":"web","compose_sha256":"` + strings.Repeat("a", 64) + `"}`},
		{"cleanup with a stack", `{"version":1,"id":"` + testJobID + `","type":"cleanup","stack":"web"}`},
		{"cleanup with stack options", `{"version":1,"id":"` + testJobID + `","type":"cleanup","pre_stop":true}`},
		{"cleanup with negative age", `{"version":1,"id":"` + testJobID + `","type":"cleanup","image_min_age_hours":-1}`},
		{"cleanup keeping a bad id", `{"version":1,"id":"` + testJobID + `","type":"cleanup","keep_images":["--force"]}`},
		{"cleanup options on a backup", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","image_min_age_hours":5}`},
		{"stack job without stack", `{"version":1,"id":"` + testJobID + `","type":"backup"}`},
		{"import on a backup", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","import":true}`},
		{"removed option in_place (#33)", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","in_place":true}`},
		{"import without safety snapshot", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","import":true,"skip_safety_snapshot":true}`},
		{"negative download limit", `{"version":1,"id":"` + testJobID + `","type":"restore","stack":"web","snapshot_id":"0123abcd","limit_download_kib":-5}`},
		{"download limit on an update", `{"version":1,"id":"` + testJobID + `","type":"update","stack":"web","limit_download_kib":5}`},
		{"bad trigger", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","trigger":"cron"}`},
		{"control chars in requester", `{"version":1,"id":"` + testJobID + `","type":"backup","stack":"web","requester":"a\nb"}`},
		{"too large", `{"version":1,"x":"` + strings.Repeat("a", 70<<10) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, SpecFile), []byte(tt.json), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSpec(dir); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := ReadSpec(t.TempDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing spec: want ErrNotExist, got %v", err)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadStatus(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("want ErrNotExist before first write, got %v", err)
	}
	started := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	want := Status{ID: testJobID, State: StateFailed, StartedAt: &started, FinishedAt: &started, Error: "boom", Snapshot: "abc"}
	if err := WriteStatus(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.Error != "boom" || !got.StartedAt.Equal(started) || got.Version != 1 {
		t.Fatalf("got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, StatusFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

func TestRedactingWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewRedactingWriter(&buf, []string{"s3cret-pw", "ab", ""})
	n, err := w.Write([]byte("password s3cret-pw, short ab\n"))
	if err != nil || n != 29 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "password [REDACTED], short ab\n" {
		t.Fatalf("got %q", got)
	}
}

func TestStateFinal(t *testing.T) {
	for s, want := range map[State]bool{StateQueued: false, StateRunning: false, StateSucceeded: true, StateFailed: true, StateCancelled: true} {
		if s.Final() != want {
			t.Errorf("%s.Final() = %v", s, !want)
		}
	}
}

func TestCleanupSpecRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Spec{ID: testJobID, Type: TypeCleanup, ImageMinAgeHours: 168,
		KeepImages: []string{"sha256:" + strings.Repeat("a", 64)}, Trigger: "schedule"}
	if err := WriteSpec(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSpec(dir)
	want.Version = handoffVersion
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !got.Type.Host() || TypeBackup.Host() {
		t.Fatal("Host wrong")
	}
	tooMany := want
	tooMany.KeepImages = make([]string, MaxKeepImages+1)
	for i := range tooMany.KeepImages {
		tooMany.KeepImages[i] = want.KeepImages[0]
	}
	if err := WriteSpec(t.TempDir(), tooMany); err == nil {
		t.Fatal("too many images to keep accepted")
	}
}
