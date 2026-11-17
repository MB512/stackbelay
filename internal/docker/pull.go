package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// PullStatus is the progress of "docker compose pull" (see CLI.Pulling).
type PullStatus struct {
	// BytesDone and BytesTotal add up the layers whose size is known (a
	// layer's size is known once its download starts).
	BytesDone, BytesTotal uint64
	SecondsElapsed        int64
	Images                []ImagePull // in the order compose reported them
}

// ImagePull is the progress of one image.
type ImagePull struct {
	Image                 string
	BytesDone, BytesTotal uint64
	Done                  bool
}

// Summary describes the images for the job page, e.g.
// "nginx:1.27 12.3 MiB of 20.1 MiB, alpine:3.20 done".
func (s PullStatus) Summary() string {
	parts := make([]string, 0, len(s.Images))
	for _, im := range s.Images {
		switch {
		case im.Done:
			parts = append(parts, im.Image+" done")
		case im.BytesTotal > 0:
			parts = append(parts, fmt.Sprintf("%s %s of %s", im.Image, mib(im.BytesDone), mib(im.BytesTotal)))
		default:
			parts = append(parts, im.Image+" waiting")
		}
	}
	return strings.Join(parts, ", ")
}

func mib(n uint64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// pullLine is one line of "docker compose --progress json pull" (stderr).
// Image lines have id "Image <ref>"; layer lines carry the image in
// parent_id and, while downloading, current and total bytes.
type pullLine struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Status   string `json:"status"`
	Text     string `json:"text"`
	Current  uint64 `json:"current"`
	Total    uint64 `json:"total"`
}

const imagePrefix = "Image "

type layerState struct {
	current, total uint64
	done           bool
}

type imageState struct {
	done   bool
	layers map[string]*layerState
}

// pullTracker adds up the lines of a compose pull.
type pullTracker struct {
	start  time.Time
	order  []string
	images map[string]*imageState
}

func newPullTracker(start time.Time) *pullTracker {
	return &pullTracker{start: start, images: map[string]*imageState{}}
}

func (t *pullTracker) image(name string) *imageState {
	im, ok := t.images[name]
	if !ok {
		im = &imageState{layers: map[string]*layerState{}}
		t.images[name] = im
		t.order = append(t.order, name)
	}
	return im
}

// add processes one line; it reports whether an image finished (worth an
// immediate report). Lines that are not pull progress are ignored.
func (t *pullTracker) add(raw []byte) (imageDone bool) {
	var l pullLine
	if json.Unmarshal(raw, &l) != nil {
		return false
	}
	switch {
	case l.ParentID == "" && strings.HasPrefix(l.ID, imagePrefix):
		im := t.image(strings.TrimPrefix(l.ID, imagePrefix))
		if l.Status == "Done" && !im.done {
			im.done = true
			return true
		}
	case strings.HasPrefix(l.ParentID, imagePrefix) && l.ID != "":
		im := t.image(strings.TrimPrefix(l.ParentID, imagePrefix))
		layer, ok := im.layers[l.ID]
		if !ok {
			layer = &layerState{}
			im.layers[l.ID] = layer
		}
		switch l.Text {
		case "Downloading":
			if l.Total > 0 {
				layer.current, layer.total = l.Current, l.Total
			}
		case "Download complete", "Pull complete", "Already exists":
			layer.done = true
			layer.current = layer.total
		}
	}
	return false
}

func (t *pullTracker) status(now time.Time) PullStatus {
	s := PullStatus{SecondsElapsed: int64(now.Sub(t.start) / time.Second)}
	for _, name := range t.order {
		im := t.images[name]
		ip := ImagePull{Image: name, Done: im.done}
		for _, l := range im.layers {
			ip.BytesDone += l.current
			ip.BytesTotal += l.total
		}
		s.BytesDone += ip.BytesDone
		s.BytesTotal += ip.BytesTotal
		s.Images = append(s.Images, ip)
	}
	return s
}

// pullWithProgress runs "compose --progress json pull" and reports its
// progress to c.Pulling: at most once per second, at once when an image
// is done, and once at the end.
func (c *CLI) pullWithProgress(ctx context.Context, dir string, services []string) error {
	args := append([]string{"compose", "--project-directory", dir, "--progress", "json",
		"pull", "--ignore-buildable", "--"}, services...)
	cmd := exec.CommandContext(ctx, c.binary(), args...)
	cmd.Dir = dir
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("docker compose pull: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker compose pull: %w", err)
	}
	now := c.now
	tracker := newPullTracker(now())
	var last time.Time
	var other []string // non-progress output, for the error message
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !json.Valid(line) {
			other = append(other, string(line))
			continue
		}
		var l pullLine
		if json.Unmarshal(line, &l) == nil && l.Status == "Error" {
			other = append(other, strings.TrimSpace(l.ID+": "+l.Text))
		}
		imageDone := tracker.add(line)
		if t := now(); imageDone || t.Sub(last) >= time.Second {
			last = t
			c.Pulling(tracker.status(t))
		}
	}
	err = cmd.Wait()
	c.Pulling(tracker.status(now()))
	if err != nil {
		return fmt.Errorf("docker compose pull: %w: %s", err, tail(strings.Join(other, "\n")))
	}
	return nil
}
