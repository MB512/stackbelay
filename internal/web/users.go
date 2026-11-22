package web

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/store"
)

// usernamePattern: lower case only, so "Alice" and "alice" cannot be two
// different accounts.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// usersPage is the data of the user management page.
type usersPage struct {
	Users []store.User
	// NewPassword is a generated password to show once (after creating a
	// user or resetting a password), for the user NewPasswordFor.
	NewPassword    string
	NewPasswordFor string
	Username       string // form value to keep after an error
}

func (s *Server) userList(w http.ResponseWriter, r *http.Request) {
	s.renderUsers(w, r, http.StatusOK, usersPage{}, "")
}

// renderUsers shows the user page with an optional error message.
func (s *Server) renderUsers(w http.ResponseWriter, r *http.Request, status int, page usersPage, errMsg string) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.internalError(w, r, "listing users", err)
		return
	}
	page.Users = users
	s.render(w, r, status, "users.html", pageData{Title: "Users", Error: errMsg, Data: page})
}

// createUser creates a user with a generated password that must be changed
// at the first login. The password is shown exactly once.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	username := strings.TrimSpace(r.PostFormValue("username"))
	isAdmin := r.PostFormValue("is_admin") == "on"
	if !usernamePattern.MatchString(username) {
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{Username: username},
			"Usernames are 1 to 64 characters: lower-case letters, digits, '.', '_' and '-'.")
		return
	}
	password, hash, err := newGeneratedPassword()
	if err != nil {
		s.internalError(w, r, "creating user", err)
		return
	}
	id, err := s.Store.CreateUser(r.Context(), store.User{
		Username: username, PasswordHash: hash, IsAdmin: isAdmin, MustChangePassword: true,
	})
	if errors.Is(err, store.ErrConflict) {
		s.renderUsers(w, r, http.StatusConflict, usersPage{Username: username}, "This username is already taken.")
		return
	}
	if err != nil {
		s.internalError(w, r, "creating user", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "user.create",
		Detail: fmt.Sprintf("user %q (id %d), admin=%t", username, id, isAdmin)})
	s.renderUsers(w, r, http.StatusOK, usersPage{NewPassword: password, NewPasswordFor: username}, "")
}

// targetUser loads the user from the URL. Admins cannot change their own
// flags or reset their own password here (to not lock themselves out by
// accident); they use the normal password change.
func (s *Server) targetUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "User not found.")
		return store.User{}, false
	}
	u, err := s.Store.UserByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "User not found.")
		return store.User{}, false
	}
	if err != nil {
		s.internalError(w, r, "loading user", err)
		return store.User{}, false
	}
	if u.ID == authFrom(r.Context()).User.ID {
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{}, "You cannot change your own account here.")
		return store.User{}, false
	}
	return u, true
}

// updateUser sets the admin and disabled flags of a user.
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	isAdmin := r.PostFormValue("is_admin") == "on"
	disabled := r.PostFormValue("disabled") == "on"
	err := s.Store.SetUserFlags(r.Context(), u.ID, isAdmin, disabled)
	if errors.Is(err, store.ErrLastAdmin) {
		s.renderUsers(w, r, http.StatusBadRequest, usersPage{}, "At least one active administrator is required.")
		return
	}
	if err != nil {
		s.internalError(w, r, "updating user", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: "user.update",
		Detail: fmt.Sprintf("user %q: admin=%t disabled=%t", u.Username, isAdmin, disabled)})
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// resetPassword gives a user a new generated password (shown once), which
// must be changed at the next login, and ends the user's sessions.
func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	password, hash, err := newGeneratedPassword()
	if err == nil {
		err = s.Store.SetPassword(r.Context(), u.ID, hash, true)
	}
	if err == nil {
		err = s.Store.DeleteUserSessions(r.Context(), u.ID, "")
	}
	if err != nil {
		s.internalError(w, r, "resetting password", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: "user.password_reset",
		Detail: fmt.Sprintf("user %q", u.Username)})
	s.renderUsers(w, r, http.StatusOK, usersPage{NewPassword: password, NewPasswordFor: u.Username}, "")
}

func newGeneratedPassword() (password, hash string, err error) {
	if password, err = auth.GeneratePassword(); err != nil {
		return "", "", err
	}
	if hash, err = auth.HashPassword(password); err != nil {
		return "", "", err
	}
	return password, hash, nil
}
