package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/MB512/stackvault/internal/store"
)

// InitialAdmin is the username of the bootstrap admin account.
const InitialAdmin = "admin"

// EnsureInitialAdmin makes sure the installation can be administered:
//
//   - No active admin and no "admin" user: create "admin" with a generated
//     password.
//   - "admin" still has a generated password (never changed), or no active
//     admin exists at all: generate a new password for "admin" (and make it
//     an active admin).
//
// In both cases the password is printed as a plain-text banner to banner
// (the container log) and the user must change it at the next login. Once
// it has been changed, nothing is printed. Generating a new password on
// every start means the plaintext is never stored anywhere.
func EnsureInitialAdmin(ctx context.Context, st *store.Store, banner io.Writer, log *slog.Logger) error {
	admins, err := st.CountActiveAdmins(ctx)
	if err != nil {
		return err
	}
	u, err := st.UserByUsername(ctx, InitialAdmin)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	var action string
	switch {
	case !exists && admins == 0:
		action = "admin.bootstrap"
	case exists && u.MustChangePassword:
		action = "admin.password_regenerated"
	case exists && admins == 0:
		action = "admin.recovered" // all admins disabled or demoted
	default:
		return nil
	}

	password, err := GeneratePassword()
	if err != nil {
		return err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if exists {
		if err := st.SetPassword(ctx, u.ID, hash, true); err != nil {
			return err
		}
		if err := st.SetUserAdmin(ctx, u.ID, true); err != nil {
			return err
		}
		// Sessions opened with the previous generated password end.
		if err := st.DeleteUserSessions(ctx, u.ID, ""); err != nil {
			return err
		}
	} else {
		id, err := st.CreateUser(ctx, store.User{Username: InitialAdmin, PasswordHash: hash, IsAdmin: true, MustChangePassword: true})
		if err != nil {
			return err
		}
		u.ID = id
	}
	if err := st.AddAudit(ctx, store.AuditEntry{UserID: u.ID, Action: action}); err != nil {
		return err
	}

	log.Warn("generated a new password for the initial admin account; it is shown in the banner on stderr",
		"username", InitialAdmin, "reason", action)
	_, err = fmt.Fprintf(banner, `
=====================================================================
  INITIAL ADMIN PASSWORD (user %q): %s
  CHANGE IT NOW: log in and set a new password.
  Until then, a new password is generated and shown on every start.
=====================================================================

`, InitialAdmin, password)
	return err
}
