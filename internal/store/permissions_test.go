package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/restic"
)

func TestPermissions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db, other := stackID(t, s, "web"), stackID(t, s, "db"), stackID(t, s, "other")
	alice, err := s.CreateUser(ctx, User{Username: "alice", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, User{Username: "bob", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}

	// No grant: zero permission, no error (deny by default).
	p, err := s.Permission(ctx, alice, web)
	if err != nil || !p.None() || p.UserID != alice || p.StackID != web {
		t.Fatalf("no grant: %+v, %v", p, err)
	}

	for _, p := range []Permission{
		{UserID: alice, StackID: web, CanBackup: true},
		{UserID: alice, StackID: db, CanBackup: true, CanRestore: true},
		{UserID: bob, StackID: web, CanRestore: true},
		{UserID: bob, StackID: db, CanLogs: true}, // logs alone is a grant
		{UserID: alice, StackID: other, CanUpdate: true},
		{UserID: bob, StackID: other, CanOperate: true}, // operate alone is a grant
	} {
		if err := s.SetPermission(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Update an existing grant.
	if err := s.SetPermission(ctx, Permission{UserID: alice, StackID: web, CanRestore: true}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Permission(ctx, alice, web); p.CanBackup || !p.CanRestore {
		t.Fatalf("after update: %+v", p)
	}

	byStack, err := s.PermissionsForUser(ctx, alice)
	if err != nil || len(byStack) != 3 || !byStack[db].CanBackup || !byStack[db].CanRestore {
		t.Fatalf("PermissionsForUser: %+v, %v", byStack, err)
	}
	byUser, err := s.PermissionsForStack(ctx, web)
	if err != nil || len(byUser) != 2 || !byUser[alice].CanRestore || !byUser[bob].CanRestore {
		t.Fatalf("PermissionsForStack: %+v, %v", byUser, err)
	}

	// Granting nothing removes the row.
	if err := s.SetPermission(ctx, Permission{UserID: bob, StackID: web}); err != nil {
		t.Fatal(err)
	}
	if byUser, _ := s.PermissionsForStack(ctx, web); len(byUser) != 1 {
		t.Fatalf("after revoke: %+v", byUser)
	}
	if p, _ := s.Permission(ctx, bob, db); !p.CanLogs || p.CanBackup || p.CanRestore || p.None() {
		t.Fatalf("logs-only grant: %+v", p)
	}
	if p, _ := s.Permission(ctx, alice, other); !p.CanUpdate || p.CanLogs || p.CanOperate || p.None() {
		t.Fatalf("update-only grant: %+v", p)
	}
	if p, _ := s.Permission(ctx, bob, other); !p.CanOperate || p.CanUpdate || p.None() {
		t.Fatalf("operate-only grant: %+v", p)
	}
	if byUser, _ := s.PermissionsForStack(ctx, other); !byUser[bob].CanOperate || byUser[alice].CanOperate {
		t.Fatalf("PermissionsForStack with operate: %+v", byUser)
	}

	if err := s.SetPermission(ctx, Permission{UserID: 999, StackID: web, CanBackup: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: want ErrNotFound, got %v", err)
	}
	if err := s.SetPermission(ctx, Permission{UserID: alice, StackID: 999, CanBackup: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown stack: want ErrNotFound, got %v", err)
	}
}

func TestListUsersAndFlags(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	admin, _ := s.CreateUser(ctx, User{Username: "admin", PasswordHash: "h", IsAdmin: true})
	carol, _ := s.CreateUser(ctx, User{Username: "carol", PasswordHash: "h"})
	if err := s.CreateSession(ctx, Session{IDHash: "c1", UserID: carol, CSRFToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "admin" || users[1].Username != "carol" {
		t.Fatalf("ListUsers: %+v, %v", users, err)
	}

	// The only active admin cannot be demoted or disabled.
	for _, tc := range []struct{ admin, disabled bool }{{false, false}, {true, true}} {
		if err := s.SetUserFlags(ctx, admin, tc.admin, tc.disabled); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("flags %+v on last admin: want ErrLastAdmin, got %v", tc, err)
		}
	}
	if u, _ := s.UserByID(ctx, admin); !u.IsAdmin || u.Disabled {
		t.Fatalf("last admin changed: %+v", u)
	}

	// With a second admin it works.
	if err := s.SetUserFlags(ctx, carol, true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserFlags(ctx, admin, false, false); err != nil {
		t.Fatal(err)
	}

	// Disabling ends the user's sessions.
	if err := s.SetUserFlags(ctx, admin, true, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserFlags(ctx, carol, false, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SessionWithUser(ctx, "c1", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session of disabled user: want ErrNotFound, got %v", err)
	}
	if err := s.SetUserFlags(ctx, 999, false, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: want ErrNotFound, got %v", err)
	}
}

func TestStackSummariesAndRepositoryInfo(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, _ := stackID(t, s, "web"), stackID(t, s, "db")

	if _, err := s.RepositoryInfoForStack(ctx, web); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no repository: want ErrNotFound, got %v", err)
	}
	repo, err := s.SetRepository(ctx, web, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	t2 := t1.Add(24 * time.Hour)
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{snap("aaaaaaaa11111111", t1), snap("bbbbbbbb22222222", t2)}, t2); err != nil {
		t.Fatal(err)
	}
	// "bb" vanished from the repository: no longer counted.
	if err := s.ReplaceSnapshots(ctx, repo, []restic.Snapshot{snap("aaaaaaaa11111111", t1)}, t2.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	sums, err := s.StackSummaries(ctx, "")
	if err != nil || len(sums) != 2 {
		t.Fatalf("StackSummaries: %+v, %v", sums, err)
	}
	if d := sums[0]; d.Name != "db" || d.HasRepository || d.Snapshots != 0 || !d.LastSnapshot.IsZero() {
		t.Fatalf("db summary: %+v", d)
	}
	if w := sums[1]; w.Name != "web" || !w.HasRepository || w.Snapshots != 1 || !w.LastSnapshot.Equal(t1) {
		t.Fatalf("web summary: %+v", w)
	}

	ri, err := s.RepositoryInfoForStack(ctx, web)
	if err != nil || ri.ID != repo || ri.Location != "/srv/restic/web" || !ri.SnapshotsRefreshedAt.Equal(t2.Add(time.Hour)) {
		t.Fatalf("RepositoryInfoForStack: %+v, %v", ri, err)
	}
}
