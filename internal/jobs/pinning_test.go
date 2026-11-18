package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const dbDigest = "postgres@sha256:ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56ef56"

// pinMetadata has a pinnable app and db and a locally built tool.
const pinMetadata = "# stackvault-metadata v1\ncreated_at\t2026-11-01T00:00:00Z\nstack\tweb\n" +
	"app\trunning\tnextcloud:29\t" + testDigest + "\t-\t-\n" +
	"db\trunning\tpostgres:16\t" + dbDigest + "\t-\t-\n" +
	"tool\trunning\t-\t-\t-\t-\n"

func TestRestorePinnedImages(t *testing.T) {
	e := restoreEnv(t)
	e.restic.restoreFiles[MetadataFile] = pinMetadata
	e.restic.restoreFiles["compose.override.yml"] = "services: {}\n"

	if _, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8], PinImages: true}); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{
		"restic.CheckRepo",
		"restic.Snapshots " + snapID[:8],
		"docker.Services",
		"docker.Stop",
		"restic.Backup tag=pre-restore parent= host=host-a",
		"restic.Restore " + snapID + ":/home/docker/web",
		"restic.Node " + snapID + ":/home/docker/web",
		"docker.EnsureImage " + testDigest,
		"docker.EnsureImage " + dbDigest,
		"docker.Services", // the restored file's services (orphans are not started)
		"docker.Up app,db,tool",
		"docker.WaitHealthy",
	})
	f := e.docker.upFiles
	if len(f) != 3 || f[0] != filepath.Join(e.stack.Dir, "compose.yml") ||
		f[1] != filepath.Join(e.stack.Dir, "compose.override.yml") {
		t.Fatalf("compose files = %v", f)
	}
	var got pinnedOverride
	if err := json.Unmarshal([]byte(e.docker.upOverride), &got); err != nil {
		t.Fatalf("override is not JSON/YAML: %v\n%s", err, e.docker.upOverride)
	}
	want := pinnedOverride{Services: map[string]pinnedService{
		"app": {Image: testDigest, PullPolicy: "never"},
		"db":  {Image: dbDigest, PullPolicy: "never"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("override = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(f[2]); !os.IsNotExist(err) {
		t.Errorf("override file %s not removed after the job: %v", f[2], err)
	}
}

func TestRestorePinnedImagesFailsBeforeSwap(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(e *env)
		wantErr  string
		wantLast string // last call before the rollback starts the old stack
	}{
		{"pull fails", func(e *env) {
			e.restic.restoreFiles[MetadataFile] = pinMetadata
			e.docker.failOn["EnsureImage"] = errBoom
		}, "getting " + testDigest, "docker.EnsureImage " + testDigest},
		{"no metadata", func(e *env) { delete(e.restic.restoreFiles, MetadataFile) }, "has no metadata.txt", "restic.Node"},
		{"invalid digest in metadata", func(e *env) {
			e.restic.restoreFiles[MetadataFile] = strings.Replace(pinMetadata, testDigest, "nextcloud:latest@sha256:x", 1)
		}, "invalid repo_digest", "restic.Node"},
		{"COMPOSE_FILE in .env", func(e *env) {
			e.restic.restoreFiles[MetadataFile] = pinMetadata
			e.restic.restoreFiles[".env"] = "# files\nexport COMPOSE_FILE=a.yml:b.yml\n"
		}, "COMPOSE_FILE", "restic.Node"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			tt.setup(e)
			_, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8], PinImages: true})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			calls := e.calls.list()
			n := len(calls)
			if n < 3 || !strings.HasPrefix(calls[n-3], tt.wantLast) ||
				calls[n-2] != "docker.Start app,db" || calls[n-1] != "docker.WaitHealthy" {
				t.Fatalf("calls do not end with %q and the rollback start:\n  %s", tt.wantLast, strings.Join(calls, "\n  "))
			}
			if got := readData(t, e.stack.Dir); got != "original" {
				t.Errorf("stack dir was replaced: %q", got)
			}
			assertNoStaging(t, e)
		})
	}
}

func TestRestorePinnedWithoutDigestsUsesComposeFiles(t *testing.T) {
	e := restoreEnv(t) // its metadata has no repo digests
	if _, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8], PinImages: true}); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(e.stack.Dir, "compose.yml")}; !reflect.DeepEqual(e.docker.upFiles, want) {
		t.Errorf("compose files = %v, want %v", e.docker.upFiles, want)
	}
}

func TestRestorePinnedNoStartSkipsPinning(t *testing.T) {
	e := restoreEnv(t)
	e.restic.restoreFiles[MetadataFile] = pinMetadata
	if _, err := e.p.Restore(context.Background(), e.stack,
		RestoreOptions{SnapshotID: snapID[:8], PinImages: true, NoStart: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls.list() {
		if strings.HasPrefix(c, "docker.EnsureImage") || strings.HasPrefix(c, "docker.Up") {
			t.Errorf("unexpected call %q", c)
		}
	}
}

func TestComposeFiles(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string // names under the final directory
		err   string
	}{
		{"yml only", map[string]string{"compose.yml": ""}, []string{"compose.yml"}, ""},
		{"yaml preferred", map[string]string{"compose.yml": "", "compose.yaml": ""}, []string{"compose.yaml"}, ""},
		{"with override", map[string]string{"compose.yaml": "", "compose.override.yaml": "", "docker-compose.override.yml": ""},
			[]string{"compose.yaml", "compose.override.yaml"}, ""},
		{".env without COMPOSE_FILE", map[string]string{"compose.yml": "", ".env": "COMPOSE_PROJECT_NAME=x\nCOMPOSE_FILES_NOTE=1\n"},
			[]string{"compose.yml"}, ""},
		{".env with COMPOSE_FILE", map[string]string{"compose.yml": "", ".env": " COMPOSE_FILE = a.yml\n"}, nil, "COMPOSE_FILE"},
		{"docker-compose.yml", map[string]string{"docker-compose.yml": ""}, []string{"docker-compose.yml"}, ""},
		{"docker-compose.yaml", map[string]string{"docker-compose.yaml": ""}, []string{"docker-compose.yaml"}, ""},
		{"compose.yml preferred over docker-compose.yml", map[string]string{"docker-compose.yml": "", "compose.yml": ""},
			[]string{"compose.yml"}, ""},
		{"docker-compose.yml with its override", map[string]string{"docker-compose.yml": "", "docker-compose.override.yml": ""},
			[]string{"docker-compose.yml", "docker-compose.override.yml"}, ""},
		{"docker-compose.yaml with compose.override.yml", map[string]string{"docker-compose.yaml": "", "compose.override.yml": "", "docker-compose.override.yaml": ""},
			[]string{"docker-compose.yaml", "compose.override.yml"}, ""},
		{"no compose file", map[string]string{"stack.yml": ""}, nil, "no compose file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := composeFiles(src, "/stacks/web")
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, n := range tt.want {
				want = append(want, filepath.Join("/stacks/web", n))
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}
