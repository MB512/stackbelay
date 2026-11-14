package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUsers(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.CreateUser(ctx, User{Username: "admin", PasswordHash: "h1", IsAdmin: true, MustChangePassword: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, User{Username: "admin", PasswordHash: "x"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username: want ErrConflict, got %v", err)
	}
	u, err := s.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != id || !u.IsAdmin || !u.MustChangePassword || u.Disabled || u.PasswordHash != "h1" || u.CreatedAt.IsZero() {
		t.Fatalf("unexpected user %+v", u)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 1 {
		t.Fatalf("admins = %d", n)
	}

	if err := s.SetPassword(ctx, id, "h2", false); err != nil {
		t.Fatal(err)
	}
	u, _ = s.UserByID(ctx, id)
	if u.PasswordHash != "h2" || u.MustChangePassword {
		t.Fatalf("after SetPassword: %+v", u)
	}
	if err := s.SetPassword(ctx, 999, "x", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.UserByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSessions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, User{Username: "u", PasswordHash: "h"})
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

	for _, id := range []string{"s1", "s2", "s3"} {
		if err := s.CreateSession(ctx, Session{IDHash: id, UserID: uid, CSRFToken: "csrf-" + id, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	sess, u, err := s.SessionWithUser(ctx, "s1", now)
	if err != nil {
		t.Fatal(err)
	}
	if sess.CSRFToken != "csrf-s1" || u.Username != "u" || !sess.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("got %+v %+v", sess, u)
	}

	// Expired at now+1h.
	if _, _, err := s.SessionWithUser(ctx, "s1", now.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: want ErrNotFound, got %v", err)
	}
	if err := s.ExtendSession(ctx, "s1", now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionWithUser(ctx, "s1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("extended session: %v", err)
	}

	// Password change: all other sessions go.
	if err := s.DeleteUserSessions(ctx, uid, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionWithUser(ctx, "s2", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("s2 survived DeleteUserSessions")
	}
	if _, _, err := s.SessionWithUser(ctx, "s1", now); err != nil {
		t.Fatal("kept session was deleted")
	}

	if err := s.DeleteSession(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionWithUser(ctx, "s1", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("logout did not delete session")
	}

	if err := s.CreateSession(ctx, Session{IDHash: "old", UserID: uid, CSRFToken: "c", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteExpiredSessions(ctx, now); err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d, %v", n, err)
	}
}

func TestAudit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	uid, _ := s.CreateUser(ctx, User{Username: "u", PasswordHash: "h"})
	entries := []AuditEntry{
		{Action: "login.failure", Detail: `username "nobody"`},
		{UserID: uid, Action: "login.success"},
	}
	for _, e := range entries {
		if err := s.AddAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAudit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Action != "login.success" || got[0].UserID != uid ||
		got[1].UserID != 0 || got[1].Detail != `username "nobody"` || got[1].At.IsZero() {
		t.Fatalf("unexpected audit log %+v", got)
	}
}
