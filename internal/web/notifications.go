package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/notify"
	"github.com/MB512/stackbelay/internal/store"
)

// NotifySender sends a test notification (*notify.NTFY).
type NotifySender interface {
	Send(ctx context.Context, t notify.Target, m notify.Message) error
}

// Authentication types of the notification form.
const (
	notifyAuthNone  = "none"
	notifyAuthToken = "token"
	notifyAuthBasic = "basic"
)

// notifyForm is the notification settings form. It never holds the stored
// token or password, only whether one is stored.
type notifyForm struct {
	ServerURL, Topic string
	AuthType         string // notifyAuthNone, notifyAuthToken or notifyAuthBasic
	TokenStored      bool
	User             string
	PasswordStored   bool
	Rows             []notifyRow   // the job type x outcome matrix
	Single           []notifyEvent // rollback, left down
}

// notifyRow is one job type of the matrix.
type notifyRow struct {
	Label             string
	Failed, Succeeded notifyEvent
}

// notifyEvent is one checkbox.
type notifyEvent struct {
	Key    notify.Key
	Label  string
	On     bool
	Urgent bool
}

// notifyMatrix lists the rows of the matrix: label, failed key, succeeded key.
var notifyMatrix = []struct {
	label             string
	failed, succeeded notify.Key
}{
	{"Backup", notify.KeyBackupFailed, notify.KeyBackupSucceeded},
	{"Update", notify.KeyUpdateFailed, notify.KeyUpdateSucceeded},
	{"Compose change", notify.KeyComposeFailed, notify.KeyComposeSucceeded},
	{"Restore", notify.KeyRestoreFailed, notify.KeyRestoreSucceeded},
	{"Stack operation (stop, start, restart, up, down)", notify.KeyOperationFailed, notify.KeyOperationSucceeded},
	{"Host cleanup", notify.KeyCleanupFailed, notify.KeyCleanupSucceeded},
}

// notifyPage is the data of admin_notifications.html.
type notifyPage struct {
	Form        notifyForm
	Host        string   // as messages name it
	MutedStacks []string // names, sorted
	CanTest     bool
}

func newNotifyForm(ns store.NotifySettings) notifyForm {
	f := notifyForm{ServerURL: ns.ServerURL, Topic: ns.Topic, AuthType: notifyAuthNone,
		TokenStored: ns.Auth.Token != "", User: ns.Auth.User, PasswordStored: ns.Auth.Password != ""}
	switch {
	case ns.Auth.Token != "":
		f.AuthType = notifyAuthToken
	case ns.Auth.User != "":
		f.AuthType = notifyAuthBasic
	}
	event := func(key notify.Key) notifyEvent {
		info, _ := notify.Info(key)
		return notifyEvent{Key: key, Label: info.Label, On: notify.Enabled(ns.Events, key),
			Urgent: info.Priority == notify.PriorityUrgent}
	}
	for _, m := range notifyMatrix {
		f.Rows = append(f.Rows, notifyRow{Label: m.label, Failed: event(m.failed), Succeeded: event(m.succeeded)})
	}
	f.Single = []notifyEvent{event(notify.KeyRollback), event(notify.KeyLeftDown)}
	return f
}

// adminNotifications shows the notification settings (admin only).
func (s *Server) adminNotifications(w http.ResponseWriter, r *http.Request) {
	ns, err := s.Store.NotifySettings(r.Context())
	if err != nil {
		s.internalError(w, r, "loading notification settings", err)
		return
	}
	s.renderNotifications(w, r, http.StatusOK, newNotifyForm(ns), "", "")
}

func (s *Server) renderNotifications(w http.ResponseWriter, r *http.Request, status int, form notifyForm, info, errMsg string) {
	stacks, err := s.Store.ListStacks(r.Context())
	if err != nil {
		s.internalError(w, r, "listing stacks", err)
		return
	}
	muted, err := s.Store.MutedStacks(r.Context())
	if err != nil {
		s.internalError(w, r, "listing muted stacks", err)
		return
	}
	page := notifyPage{Form: form, Host: notify.ShortHost(s.Hostname), CanTest: s.Notify != nil}
	for _, st := range stacks { // sorted by name
		if muted[st.ID] {
			page.MutedStacks = append(page.MutedStacks, st.Name)
		}
	}
	s.render(w, r, status, "admin_notifications.html",
		pageData{Title: "Notifications", Info: info, Error: errMsg, Data: page})
}

// saveNotifications stores the notification settings (admin only).
func (s *Server) saveNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	old, err := s.Store.NotifySettings(ctx)
	if err != nil {
		s.internalError(w, r, "loading notification settings", err)
		return
	}
	ns, echo, msg := parseNotifyInput(r, old)
	if msg != "" {
		s.renderNotifications(w, r, http.StatusBadRequest, echo, "", msg)
		return
	}
	if err := s.Store.SaveNotifySettings(ctx, ns); err != nil {
		s.internalError(w, r, "saving notification settings", err)
		return
	}
	var on []string
	for _, e := range notify.Events {
		if ns.Events[string(e.Key)] {
			on = append(on, string(e.Key))
		}
	}
	// The topic is left out: on a public server it is the only secret.
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "notify.update",
		Detail: fmt.Sprintf("server %q, topic set: %t, auth %s, credentials changed: %t, events on: %s",
			truncate(ns.ServerURL, 200), ns.Topic != "", authType(ns.Auth), ns.Auth != old.Auth, strings.Join(on, ", "))})
	info := "Saved."
	if ns.Topic == "" {
		info = "Saved. Notifications are off until a topic is set."
	}
	s.renderNotifications(w, r, http.StatusOK, newNotifyForm(ns), info, "")
}

// parseNotifyInput reads the form. An empty token or password field keeps
// the stored one. On an error it returns the form to show again (without
// secrets) and a message for the user.
func parseNotifyInput(r *http.Request, old store.NotifySettings) (store.NotifySettings, notifyForm, string) {
	ns := store.NotifySettings{
		ServerURL: strings.TrimSpace(r.PostFormValue("server_url")),
		Topic:     strings.TrimSpace(r.PostFormValue("topic")),
		Events:    map[string]bool{},
	}
	for _, e := range notify.Events {
		ns.Events[string(e.Key)] = false
	}
	for _, key := range r.PostForm["event"] {
		if _, ok := notify.Info(notify.Key(key)); ok {
			ns.Events[key] = true
		}
	}
	authKind := r.PostFormValue("auth")
	switch authKind {
	case notifyAuthToken:
		ns.Auth.Token = r.PostFormValue("token")
		if ns.Auth.Token == "" {
			ns.Auth.Token = old.Auth.Token
		}
	case notifyAuthBasic:
		ns.Auth.User = strings.TrimSpace(r.PostFormValue("user"))
		ns.Auth.Password = r.PostFormValue("password")
		if ns.Auth.Password == "" && ns.Auth.User == old.Auth.User {
			ns.Auth.Password = old.Auth.Password
		}
	default:
		authKind = notifyAuthNone
	}

	echo := newNotifyForm(ns)
	echo.AuthType = authKind
	echo.TokenStored, echo.PasswordStored = old.Auth.Token != "", old.Auth.Password != ""
	if ns.ServerURL == "" {
		ns.ServerURL = "https://ntfy.sh"
	}
	switch {
	case notify.ValidateServerURL(ns.ServerURL) != nil:
		return ns, echo, capitalize(notify.ValidateServerURL(ns.ServerURL).Error()) + "."
	case ns.Topic != "" && notify.ValidateTopic(ns.Topic) != nil:
		return ns, echo, capitalize(notify.ValidateTopic(ns.Topic).Error()) + "."
	case authKind == notifyAuthToken && ns.Auth.Token == "":
		return ns, echo, "Enter the access token."
	case authKind == notifyAuthBasic && (ns.Auth.User == "" || ns.Auth.Password == ""):
		return ns, echo, "Enter user and password."
	case strings.ContainsAny(ns.Auth.Token+ns.Auth.User, "\r\n"):
		return ns, echo, "Token and user must be a single line."
	}
	return ns, echo, ""
}

func authType(a store.NotifyAuth) string {
	switch {
	case a.Token != "":
		return notifyAuthToken
	case a.User != "":
		return notifyAuthBasic
	}
	return notifyAuthNone
}

// testNotification sends a test message with the stored settings (admin
// only).
func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ns, err := s.Store.NotifySettings(ctx)
	if err != nil {
		s.internalError(w, r, "loading notification settings", err)
		return
	}
	form := newNotifyForm(ns)
	target, on := notify.TargetOf(ns)
	switch {
	case s.Notify == nil:
		s.renderNotifications(w, r, http.StatusServiceUnavailable, form, "", "Sending is not available in this installation.")
		return
	case !on:
		s.renderNotifications(w, r, http.StatusBadRequest, form, "", "Set and save a topic first.")
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "notify.test"})
	host := notify.ShortHost(s.Hostname)
	msg := notify.Message{Title: "🔔 StackBelay test · " + host, Priority: notify.PriorityDefault,
		Body: "Notifications from " + host + " work."}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := s.Notify.Send(sctx, target, msg); err != nil {
		s.Logger.Warn("sending test notification", "error", err)
		s.renderNotifications(w, r, http.StatusBadGateway, form, "", "Sending failed: "+truncate(err.Error(), 300))
		return
	}
	s.renderNotifications(w, r, http.StatusOK, form, "Test notification sent.", "")
}

// setStackNotifyMuted mutes or unmutes a stack's notifications (admin
// only, Settings tab).
func (s *Server) setStackNotifyMuted(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	muted := r.PostFormValue("muted") == "true"
	if err := s.Store.SetStackNotifyMuted(ctx, st.ID, muted); err != nil {
		s.internalError(w, r, "muting notifications", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "stack.notify_muted", StackID: st.ID,
		Detail: fmt.Sprintf("notifications muted: %t", muted)})
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}
