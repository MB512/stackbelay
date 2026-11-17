package auth

import (
	"testing"

	"github.com/MB512/stackvault/internal/store"
)

func TestAllowed(t *testing.T) {
	user := store.User{ID: 7}
	admin := store.User{ID: 1, IsAdmin: true}
	perm := func(backup, restore bool) store.Permission {
		return store.Permission{UserID: 7, StackID: 3, CanBackup: backup, CanRestore: restore}
	}
	tests := []struct {
		name   string
		user   store.User
		perm   store.Permission
		action Action
		want   bool
	}{
		{"no grant: view", user, store.Permission{}, ActionView, false},
		{"no grant: backup", user, store.Permission{}, ActionBackup, false},
		{"backup only: view", user, perm(true, false), ActionView, true},
		{"backup only: backup", user, perm(true, false), ActionBackup, true},
		{"backup only: restore", user, perm(true, false), ActionRestore, false},
		{"restore only: view", user, perm(false, true), ActionView, true},
		{"restore only: backup", user, perm(false, true), ActionBackup, false},
		{"restore only: restore", user, perm(false, true), ActionRestore, true},
		{"other user's grant", store.User{ID: 8}, perm(true, true), ActionView, false},
		{"disabled user", store.User{ID: 7, Disabled: true}, perm(true, true), ActionBackup, false},
		{"backup only: logs", user, perm(true, false), ActionLogs, false},
		{"logs only: view", user, store.Permission{UserID: 7, StackID: 3, CanLogs: true}, ActionView, true},
		{"logs only: logs", user, store.Permission{UserID: 7, StackID: 3, CanLogs: true}, ActionLogs, true},
		{"logs only: backup", user, store.Permission{UserID: 7, StackID: 3, CanLogs: true}, ActionBackup, false},
		{"admin: logs", admin, store.Permission{}, ActionLogs, true},
		{"admin: update", admin, store.Permission{}, ActionUpdate, true},
		{"update only: view", user, store.Permission{UserID: 7, StackID: 3, CanUpdate: true}, ActionView, true},
		{"update only: update", user, store.Permission{UserID: 7, StackID: 3, CanUpdate: true}, ActionUpdate, true},
		{"update only: backup", user, store.Permission{UserID: 7, StackID: 3, CanUpdate: true}, ActionBackup, false},
		{"backup only: update", user, perm(true, false), ActionUpdate, false},
		{"admin: operate", admin, store.Permission{}, ActionOperate, true},
		{"operate only: view", user, store.Permission{UserID: 7, StackID: 3, CanOperate: true}, ActionView, true},
		{"operate only: operate", user, store.Permission{UserID: 7, StackID: 3, CanOperate: true}, ActionOperate, true},
		{"operate only: backup", user, store.Permission{UserID: 7, StackID: 3, CanOperate: true}, ActionBackup, false},
		{"update only: operate", user, store.Permission{UserID: 7, StackID: 3, CanUpdate: true}, ActionOperate, false},
		{"unknown action", user, perm(true, true), Action(99), false},
		{"admin without grant: restore", admin, store.Permission{}, ActionRestore, true},
		{"disabled admin", store.User{ID: 1, IsAdmin: true, Disabled: true}, store.Permission{}, ActionView, false},
		{"admin: unknown action", admin, store.Permission{}, Action(-1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Allowed(tc.user, tc.perm, tc.action); got != tc.want {
				t.Fatalf("Allowed(%s) = %v, want %v", tc.action, got, tc.want)
			}
		})
	}
}
