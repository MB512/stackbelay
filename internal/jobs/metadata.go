package jobs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// MetadataFile is the name of the metadata file written into the stack
// directory before every backup (and thus contained in every snapshot).
const MetadataFile = "metadata.txt"

const (
	metadataHeader = "# stackbelay-metadata v1"
	// metadataHeaderLegacy was written under StackVault, the project's
	// early working title (issue #44). Snapshots in append-only
	// repositories keep it for good, so the parser accepts it too. The
	// format is the same.
	metadataHeaderLegacy = "# stackvault-metadata v1"
	metadataColumns      = "# service\tstate\timage_ref\trepo_digest\timage_id\tcontainer_id"
	metadataMaxBytes     = 1 << 20 // 1 MiB; a real file is a few KiB at most
	metadataMaxLines     = 10_000
	none                 = "-"
)

// Validation patterns. metadata.txt comes from a snapshot and must be treated
// as untrusted input, so every field is checked against a strict pattern.
var (
	serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	imageRefPattern    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:@-]{0,511}$`)
	// RepoDigestPattern is the only accepted form for image references used
	// for pinning (see CLAUDE.md).
	RepoDigestPattern  = regexp.MustCompile(`^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$`)
	imageIDPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
	sha256HexPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	validStates        = map[string]bool{
		"running": true, "exited": true, "created": true, "paused": true,
		"restarting": true, "dead": true, "removing": true, none: true,
	}
)

// Metadata is the content of metadata.txt (format v1).
type Metadata struct {
	CreatedAt     time.Time
	Stack         string
	ComposeSHA256 string // hex, or "" if unknown
	Services      []ServiceRecord
}

// ServiceRecord is one service line. Empty strings are written as "-".
type ServiceRecord struct {
	Name        string
	State       string // container state, "-" if no container exists
	ImageRef    string // e.g. "nextcloud:29"
	RepoDigest  string // e.g. "nextcloud@sha256:..." (pullable), "" if none
	ImageID     string // e.g. "sha256:..." (local only)
	ContainerID string
}

// RunningServices returns the names of all services recorded as running.
func (m *Metadata) RunningServices() []string {
	var out []string
	for _, s := range m.Services {
		if s.State == "running" {
			out = append(out, s.Name)
		}
	}
	return out
}

// WriteMetadata serializes m in format v1. It validates m first so that we
// never write a file our own parser would reject.
func WriteMetadata(w io.Writer, m *Metadata) error {
	if err := m.validate(m.Stack); err != nil {
		return fmt.Errorf("invalid metadata: %w", err)
	}
	var b bytes.Buffer
	fmt.Fprintln(&b, metadataHeader)
	fmt.Fprintf(&b, "created_at\t%s\n", m.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "stack\t%s\n", m.Stack)
	fmt.Fprintf(&b, "compose_sha256\t%s\n", dash(m.ComposeSHA256))
	fmt.Fprintln(&b, metadataColumns)
	for _, s := range m.Services {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, dash(s.State), dash(s.ImageRef), dash(s.RepoDigest), dash(s.ImageID), dash(s.ContainerID))
	}
	_, err := w.Write(b.Bytes())
	return err
}

// ParseMetadata parses and validates metadata.txt. wantStack is the stack
// name the file must belong to. The input is untrusted.
func ParseMetadata(r io.Reader, wantStack string) (*Metadata, error) {
	data, err := io.ReadAll(io.LimitReader(r, metadataMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}
	if len(data) > metadataMaxBytes {
		return nil, errors.New("metadata file too large")
	}

	m := &Metadata{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo > metadataMaxLines {
			return nil, errors.New("metadata file has too many lines")
		}
		line := sc.Text()
		if lineNo == 1 {
			if line != metadataHeader && line != metadataHeaderLegacy {
				return nil, fmt.Errorf("unsupported metadata header %q", truncate(line))
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		switch len(fields) {
		case 2:
			key, val := fields[0], fields[1]
			if seen[key] {
				return nil, fmt.Errorf("line %d: duplicate key %q", lineNo, truncate(key))
			}
			seen[key] = true
			switch key {
			case "created_at":
				t, err := time.Parse(time.RFC3339, val)
				if err != nil {
					return nil, fmt.Errorf("line %d: invalid created_at", lineNo)
				}
				m.CreatedAt = t.UTC()
			case "stack":
				m.Stack = val
			case "compose_sha256":
				m.ComposeSHA256 = undash(val)
			default:
				// Unknown keys are ignored so that minor, backward-compatible
				// additions do not break old readers.
			}
		case 6:
			m.Services = append(m.Services, ServiceRecord{
				Name:        fields[0],
				State:       fields[1],
				ImageRef:    undash(fields[2]),
				RepoDigest:  undash(fields[3]),
				ImageID:     undash(fields[4]),
				ContainerID: undash(fields[5]),
			})
		default:
			return nil, fmt.Errorf("line %d: expected 2 or 6 tab-separated fields, got %d", lineNo, len(fields))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning metadata: %w", err)
	}
	if lineNo == 0 {
		return nil, errors.New("metadata file is empty")
	}
	for _, k := range []string{"created_at", "stack"} {
		if !seen[k] {
			return nil, fmt.Errorf("metadata is missing %q", k)
		}
	}
	if err := m.validate(wantStack); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Metadata) validate(wantStack string) error {
	if m.Stack != wantStack {
		return fmt.Errorf("metadata belongs to stack %q, expected %q", truncate(m.Stack), wantStack)
	}
	if m.ComposeSHA256 != "" && !sha256HexPattern.MatchString(m.ComposeSHA256) {
		return errors.New("invalid compose_sha256")
	}
	names := map[string]bool{}
	for i, s := range m.Services {
		if !serviceNamePattern.MatchString(s.Name) {
			return fmt.Errorf("service %d: invalid name %q", i, truncate(s.Name))
		}
		if names[s.Name] {
			return fmt.Errorf("service %q listed twice", s.Name)
		}
		names[s.Name] = true
		if !validStates[dash(s.State)] {
			return fmt.Errorf("service %q: invalid state %q", s.Name, truncate(s.State))
		}
		checks := []struct {
			field, val string
			re         *regexp.Regexp
		}{
			{"image_ref", s.ImageRef, imageRefPattern},
			{"repo_digest", s.RepoDigest, RepoDigestPattern},
			{"image_id", s.ImageID, imageIDPattern},
			{"container_id", s.ContainerID, containerIDPattern},
		}
		for _, c := range checks {
			if c.val != "" && !c.re.MatchString(c.val) {
				return fmt.Errorf("service %q: invalid %s %q", s.Name, c.field, truncate(c.val))
			}
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return none
	}
	return s
}

func undash(s string) string {
	if s == none {
		return ""
	}
	return s
}

// truncate shortens untrusted values before they are put into error messages.
func truncate(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
