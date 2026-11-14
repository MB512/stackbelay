package restic

import (
	"errors"
	"strings"
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
	repo := Repo{Env: []string{"RESTIC_PASSWORD=s3cret-pw", "RESTIC_REST_PASSWORD=ab", "EMPTY=",
		"RESTIC_REST_USERNAME=demo", "AWS_SECRET_ACCESS_KEY=aws-secret-key", "AWS_ACCESS_KEY_ID=AKIDEXAMPLE"}}
	got := redact("wrong password s3cret-pw for ab: user demo, stack demo-web, key AKIDEXAMPLE/aws-secret-key", repo)
	want := "wrong password [REDACTED] for ab: user demo, stack demo-web, key AKIDEXAMPLE/[REDACTED]"
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

func TestParseNodeLine(t *testing.T) {
	snapshotLine := `{"time":"2026-11-13T21:23:47+01:00","paths":["/s/web"],"id":"e918","message_type":"snapshot","struct_type":"snapshot"}`
	if _, ok := parseNodeLine([]byte(snapshotLine)); ok {
		t.Fatal("snapshot line parsed as node")
	}
	line := `{"name":"web","type":"dir","path":"/s/web","uid":1000,"gid":1001,"mode":2147484141,"permissions":"drwxr-xr-x","mtime":"2026-11-13T21:23:45.51053436+01:00","message_type":"node","struct_type":"node"}`
	n, ok := parseNodeLine([]byte(line))
	if !ok {
		t.Fatal("node line not parsed")
	}
	if n.Path != "/s/web" || n.Type != "dir" || n.UID != 1000 || n.GID != 1001 ||
		!n.Mode.IsDir() || n.Mode.Perm() != 0o755 || n.ModTime.IsZero() {
		t.Fatalf("unexpected node %+v", n)
	}
}

func TestEnviron(t *testing.T) {
	own := map[string]string{
		"PATH":                  "/usr/bin",
		"HOME":                  "/root",
		"STACKVAULT_SECRET_KEY": "master-key",
		"RESTIC_PASSWORD":       "from-process",
		"AWS_SECRET_ACCESS_KEY": "from-process",
	}
	lookup := func(k string) (string, bool) { v, ok := own[k]; return v, ok }

	env, err := environ(Repo{Location: "rest:https://x/r", Env: []string{"RESTIC_PASSWORD=pw", "AWS_ACCESS_KEY_ID=id"}}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "HOME=/root", "RESTIC_REPOSITORY=rest:https://x/r", "RESTIC_PASSWORD=pw", "AWS_ACCESS_KEY_ID=id"}
	if strings.Join(env, "|") != strings.Join(want, "|") {
		t.Fatalf("env = %v, want %v", env, want)
	}

	for _, bad := range []string{"RESTIC_PASSWORD_COMMAND=sh -c id", "LD_PRELOAD=/x.so", "PATH=/tmp", "NOEQUALS", "RESTIC_REPOSITORY=/other"} {
		if _, err := environ(Repo{Env: []string{bad}}, lookup); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestErrorMessage(t *testing.T) {
	exitErr := errors.New("exit status 12")
	tests := []struct {
		stderr, want string
	}{
		{`{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}` + "\n",
			"Fatal: wrong password or no key found"},
		{"some warning\n" + `{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repo"}`, "Fatal: unable to open repo"},
		{"Fatal: plain text error\n", "Fatal: plain text error"},
		{"", "exit status 12"},
	}
	for _, tc := range tests {
		e := &Error{Op: "snapshots", Err: exitErr, Stderr: tc.stderr}
		if got := e.Message(); got != tc.want {
			t.Errorf("Message() = %q, want %q", got, tc.want)
		}
	}
}
