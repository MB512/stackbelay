package restic

import (
	"testing"
	"time"
)

func TestParseBackupLine(t *testing.T) {
	tests := []struct {
		line   string
		ok     bool
		typ    string
		snapID string
	}{
		{`{"message_type":"status","percent_done":0.5}`, true, "status", ""},
		{`{"message_type":"summary","files_new":3,"snapshot_id":"1f2e3d4c"}`, true, "summary", "1f2e3d4c"},
		{`{"message_type":"error","error":{"message":"permission denied"},"during":"archival","item":"/x"}`, true, "error", ""},
		{`not json`, false, "", ""},
		{``, false, "", ""},
	}
	for _, tt := range tests {
		m, ok := parseBackupLine([]byte(tt.line))
		if ok != tt.ok || m.MessageType != tt.typ || m.SnapshotID != tt.snapID {
			t.Errorf("%q: got ok=%v %+v", tt.line, ok, m)
		}
	}
}

func TestParseSnapshots(t *testing.T) {
	in := `[{"time":"2026-11-12T20:15:00.123456789Z","parent":"aaaa","tree":"t","paths":["/home/docker/nextcloud"],"hostname":"host-a","username":"root","tags":["final"],"id":"0123456789abcdef","short_id":"01234567"}]`
	snaps, err := parseSnapshots([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	s := snaps[0]
	if s.ID != "0123456789abcdef" || s.ShortID != "01234567" || s.Hostname != "host-a" ||
		s.Paths[0] != "/home/docker/nextcloud" || s.Tags[0] != "final" || s.Parent != "aaaa" {
		t.Fatalf("unexpected snapshot %+v", s)
	}
	if !s.Time.Equal(time.Date(2026, 11, 12, 20, 15, 0, 123456789, time.UTC)) {
		t.Fatalf("unexpected time %v", s.Time)
	}
}

func TestRedact(t *testing.T) {
	repo := Repo{Env: []string{"RESTIC_PASSWORD=s3cret-pw", "SHORT=ab", "EMPTY="}}
	got := redact("wrong password s3cret-pw for ab", repo)
	want := "wrong password [REDACTED] for ab"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSnapshotIDPattern(t *testing.T) {
	for id, valid := range map[string]bool{
		"01234567":       true,
		"0123456789abcd": true,
		"0123456":        false,
		"latest":         false,
		"ABCDEF01":       false,
		"0123abcd:/etc":  false,
		"--help1234":     false,
	} {
		if SnapshotIDPattern.MatchString(id) != valid {
			t.Errorf("%q: want valid=%v", id, valid)
		}
	}
}
