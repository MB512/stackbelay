package jobs

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	testDigest  = "nextcloud@sha256:ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	testImageID = "sha256:cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34"
	testHash    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func sampleMetadata() *Metadata {
	return &Metadata{
		CreatedAt:     time.Date(2026, 11, 12, 20, 15, 0, 0, time.UTC),
		Stack:         "nextcloud",
		ComposeSHA256: testHash,
		Services: []ServiceRecord{
			{Name: "app", State: "running", ImageRef: "nextcloud:29", RepoDigest: testDigest, ImageID: testImageID, ContainerID: "9f8e9f8e9f8e"},
			{Name: "web", State: "exited", ImageID: testImageID, ContainerID: "1a2b1a2b1a2b"},
			{Name: "never", State: "-"},
		},
	}
}

func TestMetadataRoundTrip(t *testing.T) {
	m := sampleMetadata()
	var buf bytes.Buffer
	if err := WriteMetadata(&buf, m); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "# stackvault-metadata v1\n") {
		t.Fatalf("missing header:\n%s", buf.String())
	}
	got, err := ParseMetadata(&buf, "nextcloud")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip mismatch:\ngot  %+v\nwant %+v", got, m)
	}
	if r := got.RunningServices(); !reflect.DeepEqual(r, []string{"app"}) {
		t.Fatalf("RunningServices = %v", r)
	}
}

func TestWriteMetadataRejectsInvalid(t *testing.T) {
	m := sampleMetadata()
	m.Services[0].RepoDigest = "registry:5000/x@sha256:" + testHash // ':' not allowed by pattern
	if err := WriteMetadata(&bytes.Buffer{}, m); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseMetadataRejects(t *testing.T) {
	valid := "# stackvault-metadata v1\ncreated_at\t2026-11-12T20:15:00Z\nstack\tnextcloud\ncompose_sha256\t-\n"
	svc := func(line string) string { return valid + line + "\n" }

	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"wrong header", "# stackvault-metadata v2\n"},
		{"wrong stack", strings.Replace(valid, "stack\tnextcloud", "stack\tother", 1)},
		{"missing stack", "# stackvault-metadata v1\ncreated_at\t2026-11-12T20:15:00Z\n"},
		{"bad time", strings.Replace(valid, "2026-11-12T20:15:00Z", "yesterday", 1)},
		{"duplicate key", valid + "stack\tnextcloud\n"},
		{"bad hash", strings.Replace(valid, "compose_sha256\t-", "compose_sha256\tzz", 1)},
		{"wrong field count", svc("app\trunning\t-")},
		{"bad service name", svc("../app\trunning\t-\t-\t-\t-")},
		{"bad state", svc("app\thacked\t-\t-\t-\t-")},
		{"bad digest", svc("app\trunning\tx\tx@sha256:short\t-\t-")},
		{"digest with tag injection", svc("app\trunning\tx\tx:latest\t-\t-")},
		{"bad image id", svc("app\trunning\tx\t-\tsha256:XYZ\t-")},
		{"bad image ref", svc("app\trunning\t-rm\t-\t-\t-")},
		{"bad container id", svc("app\trunning\tx\t-\t-\tnot-hex")},
		{"duplicate service", svc("app\trunning\t-\t-\t-\t-") + "app\texited\t-\t-\t-\t-\n"},
		{"too large", valid + strings.Repeat("# x\n", 300_000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseMetadata(strings.NewReader(tt.input), "nextcloud"); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseMetadataIgnoresUnknownKeysAndComments(t *testing.T) {
	in := "# stackvault-metadata v1\ncreated_at\t2026-11-12T20:15:00Z\nstack\tnextcloud\nfuture_key\tvalue\n# comment\n\napp\trunning\tnextcloud:29\t-\t-\t-\n"
	m, err := ParseMetadata(strings.NewReader(in), "nextcloud")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Services) != 1 || m.Services[0].Name != "app" {
		t.Fatalf("unexpected services %+v", m.Services)
	}
}
