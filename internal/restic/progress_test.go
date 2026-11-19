package restic

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeResticWithStatus is a restic stand-in that prints status lines like
// "restic backup/restore --json" and fails without RESTIC_PROGRESS_FPS=1.
const fakeResticWithStatus = `#!/bin/sh
[ "$RESTIC_PROGRESS_FPS" = 1 ] || { echo "RESTIC_PROGRESS_FPS=$RESTIC_PROGRESS_FPS" >&2; exit 1; }
case "$*" in
  *" backup "*)
    echo '{"message_type":"status","percent_done":0.25,"total_files":4,"files_done":1,"total_bytes":400,"bytes_done":100}'
    echo '{"message_type":"status","seconds_elapsed":2,"seconds_remaining":2,"percent_done":0.5,"total_files":4,"files_done":2,"total_bytes":400,"bytes_done":200}'
    echo '{"message_type":"summary","snapshot_id":"0123456789abcdef"}' ;;
  *" restore "*)
    echo '{"message_type":"status","seconds_elapsed":3,"percent_done":0.75,"total_files":4,"files_restored":2,"files_skipped":1,"total_bytes":400,"bytes_restored":200,"bytes_skipped":100}'
    echo '{"message_type":"summary","total_files":4}' ;;
esac
`

func TestProgress(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "restic")
	if err := os.WriteFile(bin, []byte(fakeResticWithStatus), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	var got []Progress
	c := &CLI{Binary: bin, Progress: func(p Progress) { got = append(got, p) }}
	repo := Repo{Location: "/r"}
	ctx := context.Background()

	if _, err := c.Backup(ctx, repo, "/s/web", BackupOptions{Host: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Restore(ctx, repo, "0123abcd", "/s/web", "/t", nil); err != nil {
		t.Fatal(err)
	}
	want := []Progress{
		{Percent: 0.25, BytesDone: 100, TotalBytes: 400, FilesDone: 1, TotalFiles: 4},
		{Percent: 0.5, BytesDone: 200, TotalBytes: 400, FilesDone: 2, TotalFiles: 4, SecondsElapsed: 2, SecondsRemaining: 2},
		// restored + skipped count as done
		{Percent: 0.75, BytesDone: 300, TotalBytes: 400, FilesDone: 3, TotalFiles: 4, SecondsElapsed: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("progress %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	// Without a Progress callback restic is not throttled: the fake
	// fails without RESTIC_PROGRESS_FPS.
	c.Progress = nil
	if _, err := c.Backup(ctx, repo, "/s/web", BackupOptions{Host: "h"}); err == nil {
		t.Fatal("RESTIC_PROGRESS_FPS set without Progress")
	}
}
