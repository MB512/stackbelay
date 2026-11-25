package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/store"
)

// Target is where messages go: an ntfy server, a topic, credentials.
type Target struct {
	ServerURL string
	Topic     string
	Auth      store.NotifyAuth
}

// TargetOf returns the target of the stored settings; ok is false while no
// topic is set (notifications off).
func TargetOf(ns store.NotifySettings) (Target, bool) {
	return Target{ServerURL: ns.ServerURL, Topic: ns.Topic, Auth: ns.Auth}, ns.Topic != ""
}

// topicPattern is what ntfy accepts as a topic name.
var topicPattern = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// ValidateTopic checks a topic name. The error messages are meant for the
// user.
func ValidateTopic(topic string) error {
	if !topicPattern.MatchString(topic) {
		return errors.New("the topic may only contain letters, digits, - and _ (at most 64 characters)")
	}
	return nil
}

// ValidateServerURL checks the ntfy server URL: http or https, a host, no
// credentials, query or fragment. The error messages are meant for the
// user.
func ValidateServerURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return errors.New("the server must be an http:// or https:// URL, e.g. https://ntfy.sh")
	case u.User != nil:
		return errors.New("the server URL must not contain credentials; use the authentication fields")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("the server URL must not have a query or fragment")
	}
	return nil
}

// NTFY sends messages to an ntfy server with its JSON publishing API, which
// carries UTF-8 titles (HTTP headers would not).
type NTFY struct {
	// Client defaults to one with a 15 s timeout.
	Client *http.Client
}

// Send publishes m to t.
func (n *NTFY) Send(ctx context.Context, t Target, m Message) error {
	body, err := json.Marshal(struct {
		Topic    string `json:"topic"`
		Title    string `json:"title"`
		Message  string `json:"message"`
		Priority int    `json:"priority"`
	}{t.Topic, m.Title, m.Body, m.Priority})
	if err != nil {
		return fmt.Errorf("encoding message: %w", err)
	}
	// JSON is posted to the server's root, the topic is in the body.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(t.ServerURL, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building ntfy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	switch {
	case t.Auth.Token != "":
		req.Header.Set("Authorization", "Bearer "+t.Auth.Token)
	case t.Auth.User != "":
		req.SetBasicAuth(t.Auth.User, t.Auth.Password)
	}
	client := n.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req) //nolint:gosec // G704: the server URL is set by an admin and validated
	if err != nil {
		// *url.Error names the URL, which holds no credentials (refused by
		// ValidateServerURL).
		return fmt.Errorf("sending to ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// ntfy explains errors in a short JSON body, e.g. {"error":"unauthorized"}.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ntfy answered %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // lets the connection be reused
	return nil
}
