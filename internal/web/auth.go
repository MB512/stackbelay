package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/store"
)

// Session cookie names. With HTTPS the "__Host-" prefix makes browsers
// enforce Secure, Path=/ and no Domain attribute (the cookie cannot be set
// or overwritten by other hosts or subdomains).
const (
	cookieSecure   = "__Host-stackvault_session"
	cookieInsecure = "stackvault_session"
)

// maxFormBytes limits request bodies of form posts. The compose editor
// posts a whole compose file (up to jobs.MaxComposeBytes), which URL
// encoding can triple (every line break becomes "%0A"), so its route gets
// maxComposeFormBytes.
const (
	maxFormBytes        = 64 << 10
	maxComposeFormBytes = 1 << 20
)

// formLimit returns the body limit for a state-changing request.
func formLimit(r *http.Request) int64 {
	if strings.HasPrefix(r.URL.Path, "/stacks/") && strings.HasSuffix(r.URL.Path, "/"+tabCompose) {
		return maxComposeFormBytes
	}
	return maxFormBytes
}

// authInfo is attached to the request context for logged-in requests.
type authInfo struct {
	Session store.Session
	User    store.User
}

// ctxKey is a private type for context keys, so keys from other packages
// cannot collide with ours.
type ctxKey int

const authKey ctxKey = 0

func authFrom(ctx context.Context) *authInfo {
	a, _ := ctx.Value(authKey).(*authInfo)
	return a
}

func (s *Server) cookieName() string {
	if s.SecureCookies {
		return cookieSecure
	}
	return cookieInsecure
}

// isHTTPS reports whether the client connection is HTTPS, directly or via a
// TLS-terminating reverse proxy (X-Forwarded-Proto). The header can be
// forged by a client talking plain HTTP, but then the Secure cookie is not
// sent back over plain HTTP, so no usable session results; the check is a
// guard against misconfiguration.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// loadSession attaches the session and user to the request context if the
// request carries a valid session cookie. It also extends the session
// (sliding expiration) once half of its lifetime has passed.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.cookieName())
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		now := s.now()
		sess, user, err := s.Store.SessionWithUser(r.Context(), auth.HashToken(c.Value), now)
		switch {
		case errors.Is(err, store.ErrNotFound):
			next.ServeHTTP(w, r) // expired or logged out: anonymous
			return
		case err != nil:
			s.Logger.Error("loading session", "error", err)
			s.renderError(w, r, http.StatusInternalServerError, "Internal error.")
			return
		case user.Disabled:
			next.ServeHTTP(w, r)
			return
		}
		// The Status tab's periodic refresh (pollHeader) does not count as
		// activity, so an open tab alone does not keep the login alive.
		if sess.ExpiresAt.Sub(now) < s.SessionTTL/2 && r.Header.Get(pollHeader) == "" {
			sess.ExpiresAt = now.Add(s.SessionTTL)
			if err := s.Store.ExtendSession(r.Context(), sess.IDHash, sess.ExpiresAt); err != nil {
				s.Logger.Warn("extending session", "error", err)
			}
		}
		ctx := context.WithValue(r.Context(), authKey, &authInfo{Session: sess, User: user})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireUser redirects anonymous requests to the login page.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authFrom(r.Context()) == nil {
			if isHTMX(r) {
				// HTMX would swap the login page into the fragment; this
				// header makes it load the login page as a whole instead.
				w.Header().Set("HX-Redirect", "/login")
				http.Error(w, "please log in", http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			s.renderError(w, r, http.StatusUnauthorized, "Please log in.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requirePasswordChanged keeps users with a generated password on the
// change-password page.
func (s *Server) requirePasswordChanged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := authFrom(r.Context())
		if a != nil && a.User.MustChangePassword && r.URL.Path != "/account/password" && r.URL.Path != "/logout" {
			http.Redirect(w, r, "/account/password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkCSRF requires the session's CSRF token on every state-changing
// request, as form field csrf_token or header X-CSRF-Token (HTMX).
func (s *Server) checkCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, formLimit(r))
		if err := r.ParseForm(); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				s.renderError(w, r, http.StatusRequestEntityTooLarge, "The form is too large.")
				return
			}
			s.renderError(w, r, http.StatusBadRequest, "Invalid form.")
			return
		}
		a := authFrom(r.Context())
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.PostFormValue("csrf_token")
		}
		if a == nil || token == "" || !auth.TokensEqual(token, a.Session.CSRFToken) {
			s.Logger.Warn("CSRF token missing or invalid", "path", r.URL.Path)
			s.renderError(w, r, http.StatusForbidden, "The form has expired. Please reload the page and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if authFrom(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login.html", pageData{Title: "Log in"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	fail := func(status int, msg string) {
		s.render(w, r, status, "login.html", pageData{Title: "Log in", Error: msg, Data: username})
	}

	if s.SecureCookies && !isHTTPS(r) {
		fail(http.StatusBadRequest, "Login requires HTTPS. Run StackVault behind a TLS reverse proxy "+
			"(or set STACKVAULT_ALLOW_INSECURE_HTTP=true for testing).")
		return
	}
	if username == "" || password == "" || len(username) > 128 || len(password) > 1024 {
		fail(http.StatusBadRequest, "Invalid username or password.")
		return
	}

	key := strings.ToLower(username)
	if ok, wait := s.Limiter.Allow(key); !ok {
		s.Logger.Warn("login blocked by rate limit", "username", username)
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many failed attempts. Try again in %s.", wait.Round(time.Second)))
		return
	}

	user, err := s.Store.UserByUsername(r.Context(), username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.CheckDummy(password) // same timing as for an existing user
	case err != nil:
		s.Logger.Error("login: loading user", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Internal error.")
		return
	default:
		err = auth.CheckPassword(user.PasswordHash, password)
		if err != nil && !errors.Is(err, auth.ErrMismatch) {
			s.Logger.Error("login: checking password", "user", username, "error", err)
		}
		if err == nil && user.Disabled {
			err = errors.New("account disabled")
		}
	}
	if err != nil {
		s.Limiter.Failure(key)
		s.audit(r, store.AuditEntry{UserID: user.ID, Action: "login.failure", Detail: fmt.Sprintf("username %q", truncate(username, 64))})
		fail(http.StatusUnauthorized, "Invalid username or password.")
		return
	}
	s.Limiter.Success(key)

	// A new session id on every login (no session fixation). An existing
	// session from this browser is ended.
	if old, err := r.Cookie(s.cookieName()); err == nil && old.Value != "" {
		_ = s.Store.DeleteSession(r.Context(), auth.HashToken(old.Value))
	}
	if err := s.startSession(w, r, user.ID); err != nil {
		s.Logger.Error("login: creating session", "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Internal error.")
		return
	}
	s.audit(r, store.AuditEntry{UserID: user.ID, Action: "login.success"})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	csrf, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err := s.Store.CreateSession(r.Context(), store.Session{
		IDHash: auth.HashToken(token), UserID: userID, CSRFToken: csrf, ExpiresAt: s.now().Add(s.SessionTTL),
	}); err != nil {
		return err
	}
	// No Expires/MaxAge: a browser-session cookie. The server enforces the
	// session lifetime. Secure is only false with the explicit
	// STACKVAULT_ALLOW_INSECURE_HTTP opt-in.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above
		Name: s.cookieName(), Value: token, Path: "/",
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	if err := s.Store.DeleteSession(r.Context(), a.Session.IDHash); err != nil {
		s.Logger.Error("logout", "error", err)
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure as in startSession
		Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "logout"})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) passwordForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "password.html", pageData{Title: "Change password"})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	current, newPw, confirm := r.PostFormValue("current"), r.PostFormValue("new"), r.PostFormValue("confirm")
	fail := func(status int, msg string) {
		s.render(w, r, status, "password.html", pageData{Title: "Change password", Error: msg})
	}

	key := strings.ToLower(a.User.Username)
	if ok, wait := s.Limiter.Allow(key); !ok {
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many failed attempts. Try again in %s.", wait.Round(time.Second)))
		return
	}
	if err := auth.CheckPassword(a.User.PasswordHash, current); err != nil {
		s.Limiter.Failure(key)
		s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "password.change_failed"})
		fail(http.StatusUnauthorized, "The current password is wrong.")
		return
	}
	switch {
	case newPw != confirm:
		fail(http.StatusBadRequest, "The new passwords do not match.")
		return
	case newPw == current:
		fail(http.StatusBadRequest, "The new password must be different from the current one.")
		return
	}
	if err := auth.ValidateNewPassword(newPw); err != nil {
		fail(http.StatusBadRequest, capitalize(err.Error())+".")
		return
	}
	hash, err := auth.HashPassword(newPw)
	if err == nil {
		err = s.Store.SetPassword(r.Context(), a.User.ID, hash, false)
	}
	if err == nil {
		// Other sessions (e.g. a forgotten browser) end; this one stays.
		err = s.Store.DeleteUserSessions(r.Context(), a.User.ID, a.Session.IDHash)
	}
	if err != nil {
		s.Logger.Error("changing password", "user", a.User.Username, "error", err)
		s.renderError(w, r, http.StatusInternalServerError, "Internal error.")
		return
	}
	s.Limiter.Success(key)
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "password.change"})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// audit writes an audit entry; failures are logged, not shown to the user.
func (s *Server) audit(r *http.Request, e store.AuditEntry) {
	e.At = s.now()
	if err := s.Store.AddAudit(r.Context(), e); err != nil {
		s.Logger.Error("writing audit log", "action", e.Action, "error", err)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
