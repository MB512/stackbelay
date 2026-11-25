package notify

import (
	"fmt"
	"strings"
	"time"
)

// StackDetail is what the stack monitor saw (stored as JSON with the
// event).
type StackDetail struct {
	Running  int      `json:"running,omitempty"`  // services running
	Total    int      `json:"total,omitempty"`    // services of the compose file
	Problems []string `json:"problems,omitempty"` // "redis is not running", "app-1 is unhealthy"
	// Recovered: the bad state that ended and how long it lasted.
	Was        string `json:"was,omitempty"`
	ForSeconds int64  `json:"for_seconds,omitempty"`
}

// formatStackEvent builds the message of a single monitor event.
func formatStackEvent(host string, ev Event) Message {
	info, _ := Info(ev.Key)
	d := ev.Detail
	title := fmt.Sprintf("%s %s · %s", info.Emoji, ev.subject(), lowerFirst(info.Label))
	lines := []string{host}
	switch ev.Key {
	case KeyStackDegraded:
		if d.Total > 0 {
			lines = append(lines, fmt.Sprintf("%d of %d services running", d.Running, d.Total))
		}
		if len(d.Problems) > 0 {
			lines = append(lines, strings.Join(d.Problems, "; "))
		}
	case KeyStackStopped:
		lines = append(lines, "No service runs, and no StackBelay job stopped it.")
	case KeyStackRecovered:
		if ev.Stack == "" {
			title = fmt.Sprintf("%s stack states readable again · %s", info.Emoji, host)
			lines = append(lines, "Docker answers again.")
			break
		}
		line := "All services running"
		if d.Was != "" && d.ForSeconds > 0 {
			line += fmt.Sprintf(" · %s for %s", d.Was, formatDuration(time.Duration(d.ForSeconds)*time.Second))
		}
		lines = append(lines, line)
	case KeyStackUnreadable:
		if ev.Stack == "" {
			title = fmt.Sprintf("%s cannot read stack states · %s", info.Emoji, host)
			lines = append(lines, "Docker did not answer for any stack.")
		} else {
			lines = append(lines, "Reading the stack's state from Docker failed.")
		}
	case KeyStackMissing:
		lines = append(lines, "The stack directory is gone.")
	}
	return Message{Title: title, Body: limitBody(lines), Priority: info.Priority}
}

// short is the part in parentheses of a line of a combined message.
func (d StackDetail) short(key Key) string {
	switch key {
	case KeyStackDegraded:
		return strings.Join(d.Problems, "; ")
	case KeyStackRecovered:
		if d.Was != "" && d.ForSeconds > 0 {
			return fmt.Sprintf("%s for %s", d.Was, formatDuration(time.Duration(d.ForSeconds)*time.Second))
		}
	}
	return ""
}
