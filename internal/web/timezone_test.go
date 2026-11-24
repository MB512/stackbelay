package web

import (
	"context"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zone database, independent of the test machine

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/restic"
)

// All times are shown in the manager's time zone: snapshots, jobs, the
// schedule's next run, job log lines, container log lines, image dates.
func TestTimesInManagerTimeZone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, true, true, func(s *Server) { s.Location = berlin })
	web, _ := e.setupStacks()
	ctx := context.Background()
	repo, err := setRepo(ctx, e.st, web, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC) // 04:00 CET
	if err := e.st.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: t0, Hostname: "h", Paths: []string{"/s/web"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()

	r := e.get("/stacks/web/backups")
	if !strings.Contains(r.body, "2026-11-13 04:00 CET") || strings.Contains(r.body, "03:00 UTC") {
		t.Fatalf("snapshot time not in CET:\n%s", r.body)
	}
	r = e.get("/stacks/web/settings")
	if !strings.Contains(r.body, "Europe/Berlin (CET, &#43;01:00)") {
		t.Fatalf("settings tab does not name the zone:\n%s", r.body)
	}

	// Container log lines and the hint.
	e.setupLogs() // lines at 16:00:01 UTC = 17:00:01 CET
	r = e.get("/stacks/web/logs")
	if !strings.Contains(r.body, "Times are in Europe/Berlin.") {
		t.Fatalf("logs page:\n%s", r.body)
	}
	r = e.get("/stacks/web/logs/events")
	if !strings.Contains(r.body, `<span class="ts">2026-11-16 17:00:01</span>`) ||
		!strings.Contains(r.body, `data-t="2026-11-16T16:00:01.000000500Z"`) { // ids stay UTC
		t.Fatalf("log stream:\n%s", r.body)
	}

	// Job log lines (the worker logs UTC).
	if l := parseLogLine(`{"time":"2026-11-14T10:00:00Z","level":"INFO","msg":"x"}`, berlin); l.Time != "11:00:00" {
		t.Fatalf("job log time %q", l.Time)
	}
	// Image dates: 23:30 UTC is already the next day in Berlin.
	v := docker.ImageVersion{Created: time.Date(2026, 1, 2, 23, 30, 0, 0, time.UTC)}
	if got := imageVersion(v, berlin); got != "2026-01-03" {
		t.Fatalf("image date %q", got)
	}
}

// Without a configured zone the server uses time.Local; an unnamed local
// zone is shown by its abbreviation.
func TestZoneName(t *testing.T) {
	s := &Server{Location: time.UTC}
	if got := s.zoneName(); got != "UTC" {
		t.Fatalf("zoneName = %q", got)
	}
	s = &Server{Location: time.FixedZone("", 0), Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }}
	if got := s.zoneName(); got != "+0000" {
		t.Fatalf("zoneName of an unnamed zone = %q", got)
	}
	if (&Server{}).loc() != time.Local {
		t.Fatal("default zone is not time.Local")
	}
}
