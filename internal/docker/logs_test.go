package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseLogLine(t *testing.T) {
	ts := time.Date(2026, 11, 16, 16, 0, 1, 123456789, time.UTC)
	tests := []struct {
		line string
		want LogLine
	}{
		{"web-app-1  | 2026-11-16T16:00:01.123456789Z GET / 200", LogLine{Container: "web-app-1", Time: ts, Text: "GET / 200"}},
		{"db-1 | 2026-11-16T16:00:01.123456789Z a | b", LogLine{Container: "db-1", Time: ts, Text: "a | b"}},
		{"db-1 | 2026-11-16T16:00:01.123456789Z", LogLine{Container: "db-1", Time: ts}},
		{"db-1 | no timestamp", LogLine{Container: "db-1", Text: "no timestamp"}},
		{"web-app-1 exited with code 0", LogLine{Text: "web-app-1 exited with code 0"}},
		{"two words | x", LogLine{Text: "two words | x"}},
		{"", LogLine{}},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			got := ParseLogLine(tt.line)
			if got.Container != tt.want.Container || !got.Time.Equal(tt.want.Time) || got.Text != tt.want.Text {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadLines(t *testing.T) {
	type line struct {
		s   string
		cut bool
	}
	tests := []struct {
		name string
		in   string
		want []line
	}{
		{"plain", "a\nb\n", []line{{"a", false}, {"b", false}}},
		{"crlf and last line without break", "a\r\nb", []line{{"a", false}, {"b", false}}},
		{"long line is cut", "0123456789abc\nnext\n", []line{{"01234567", true}, {"next", false}}},
		{"very long line", strings.Repeat("x", 50) + "\nok\n", []line{{"xxxxxxxx", true}, {"ok", false}}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []line
			err := readLines(strings.NewReader(tt.in), 8, func(s string, cut bool) error {
				got = append(got, line{s, cut})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("line %d: got %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// fakeDocker writes a script that answers the commands FollowLogs and
// Containers run, and records the arguments of "compose logs" in args.txt.
func fakeDocker(t *testing.T, stackDir, workingDir string) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	script := `#!/bin/sh
case "$*" in
  *" config --format json") echo '{"name":"web"}' ;;
  "ps --all --filter"*) printf 'web-app-1\t` + workingDir + `\n' ;;
  *" ps --all --format json") echo '{"ID":"0000000000d1","Name":"web-db-1","Project":"web","Service":"db","State":"exited"}'; echo '{"ID":"0000000000a1","Name":"web-app-1","Project":"web","Service":"app","State":"running"}'; echo '{"ID":"0000000000c1","Name":"custom","Project":"web","Service":"x","State":"running"}' ;;
  *" logs "*)
    echo "$*" > ` + argsFile + `
    echo 'web-app-1  | 2026-11-16T16:00:01.000000001Z first'
    echo 'web-db-1   | 2026-11-16T16:00:02Z second'
    echo 'web-app-1 exited with code 0'
    ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestFollowLogs(t *testing.T) {
	stack := t.TempDir()
	bin, argsFile := fakeDocker(t, stack, stack)
	c := &CLI{Binary: bin}
	ctx := context.Background()

	names, err := c.Containers(ctx, stack)
	if err != nil || strings.Join(names, ",") != "app-1,custom,db-1" {
		t.Fatalf("Containers: %v, %v", names, err)
	}

	var got []LogLine
	if err := c.FollowLogs(ctx, stack, 500, time.Time{}, func(l LogLine) error {
		got = append(got, l)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Container != "web-app-1" || got[0].Text != "first" ||
		got[1].Container != "web-db-1" || got[2].Container != "" {
		t.Fatalf("lines: %+v", got)
	}
	args, _ := os.ReadFile(argsFile)
	if want := "compose --project-directory " + stack + " logs --follow --timestamps --no-color --tail 500"; strings.TrimSpace(string(args)) != want {
		t.Fatalf("args %q, want %q", args, want)
	}

	// Resuming uses --since; an error from fn stops the stream and is returned.
	stop := errors.New("client gone")
	err = c.FollowLogs(ctx, stack, 500, got[0].Time, func(LogLine) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want fn's error", err)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.HasSuffix(strings.TrimSpace(string(args)), "--since 2026-11-16T16:00:01.000000001Z") {
		t.Fatalf("resume args %q", args)
	}
}

func TestFollowLogsRefusesForeignProject(t *testing.T) {
	stack := t.TempDir()
	bin, _ := fakeDocker(t, stack, "/somewhere/else/web")
	c := &CLI{Binary: bin}
	err := c.FollowLogs(context.Background(), stack, 10, time.Time{}, func(LogLine) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "another directory") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if _, err := c.Containers(context.Background(), stack); err == nil {
		t.Fatal("Containers accepted a foreign project")
	}
}

func TestStackContainers(t *testing.T) {
	stack := t.TempDir()
	bin, _ := fakeDocker(t, stack, stack)
	list, err := (&CLI{Binary: bin}).StackContainers(context.Background(), stack)
	if err != nil {
		t.Fatal(err)
	}
	want := []Container{
		{ID: "0000000000a1", Name: "app-1", Service: "app", State: "running"},
		{ID: "0000000000c1", Name: "custom", Service: "x", State: "running"},
		{ID: "0000000000d1", Name: "db-1", Service: "db", State: "exited"},
	}
	if len(list) != len(want) {
		t.Fatalf("got %+v", list)
	}
	for i := range want {
		if list[i] != want[i] {
			t.Errorf("container %d: got %+v, want %+v", i, list[i], want[i])
		}
	}
	if list[2].Running() || !list[0].Running() {
		t.Error("Running")
	}
}

func TestValidContainerID(t *testing.T) {
	for id, ok := range map[string]bool{
		"0123456789ab": true, strings.Repeat("a", 64): true, "0123456789a": false,
		"--privileged": false, "0123456789AB": false, strings.Repeat("a", 65): false,
	} {
		if ValidContainerID(id) != ok {
			t.Errorf("ValidContainerID(%q) != %v", id, ok)
		}
	}
}
