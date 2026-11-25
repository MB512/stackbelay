package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MB512/stackbelay/internal/store"
)

func TestNTFYSend(t *testing.T) {
	var gotAuth, gotPath string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") == "Bearer wrong" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":40101,"error":"unauthorized"}`))
		}
	}))
	defer srv.Close()

	n := &NTFY{Client: srv.Client()}
	msg := Message{Title: "❌ Backup failed · immich", Body: "nas01", Priority: PriorityHigh}
	err := n.Send(context.Background(), Target{ServerURL: srv.URL + "/", Topic: "backups", Auth: store.NotifyAuth{Token: "tk_1"}}, msg)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/" || gotAuth != "Bearer tk_1" || got["topic"] != "backups" || got["title"] != msg.Title ||
		got["message"] != "nas01" || got["priority"] != float64(PriorityHigh) {
		t.Errorf("request: path %q, auth %q, body %v", gotPath, gotAuth, got)
	}

	if err := n.Send(context.Background(), Target{ServerURL: srv.URL, Topic: "t", Auth: store.NotifyAuth{User: "u", Password: "p"}}, msg); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Basic dTpw" {
		t.Errorf("basic auth: %q", gotAuth)
	}
	if err := n.Send(context.Background(), Target{ServerURL: srv.URL, Topic: "t"}, msg); err != nil || gotAuth != "" {
		t.Errorf("no auth: %q, %v", gotAuth, err)
	}

	err = n.Send(context.Background(), Target{ServerURL: srv.URL, Topic: "t", Auth: store.NotifyAuth{Token: "wrong"}}, msg)
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "unauthorized") || strings.Contains(err.Error(), "wrong") {
		t.Errorf("error: %v", err)
	}
}

func TestValidateSettings(t *testing.T) {
	for _, topic := range []string{"backups", "my-host_01"} {
		if err := ValidateTopic(topic); err != nil {
			t.Errorf("topic %q: %v", topic, err)
		}
	}
	for _, topic := range []string{"", "a/b", "with space", strings.Repeat("x", 65)} {
		if ValidateTopic(topic) == nil {
			t.Errorf("topic %q accepted", topic)
		}
	}
	for _, u := range []string{"https://ntfy.sh", "http://ntfy.lan:8080", "https://example.com/ntfy/"} {
		if err := ValidateServerURL(u); err != nil {
			t.Errorf("url %q: %v", u, err)
		}
	}
	for _, u := range []string{"", "ntfy.sh", "ftp://ntfy.sh", "https://user:pw@ntfy.sh", "https://ntfy.sh/?x=1", "https://ntfy.sh/#a"} {
		if ValidateServerURL(u) == nil {
			t.Errorf("url %q accepted", u)
		}
	}
}
