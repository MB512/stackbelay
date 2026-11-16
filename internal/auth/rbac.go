package auth

import "github.com/MB512/stackvault/internal/store"

// Action is something a user can do on a stack.
type Action int

// The actions on a stack. Managing users, repositories, permissions and
// schedules is admin-only and not an Action.
const (
	ActionView    Action = iota // see status, snapshots and jobs
	ActionBackup                // start a backup
	ActionRestore               // start a restore
	ActionLogs                  // view the live container logs
	ActionUpdate                // update the stack (pull, back up, recreate)
)

func (a Action) String() string {
	switch a {
	case ActionView:
		return "view"
	case ActionBackup:
		return "backup"
	case ActionRestore:
		return "restore"
	case ActionLogs:
		return "logs"
	case ActionUpdate:
		return "update"
	}
	return "unknown"
}

// Allowed reports whether user may perform action on the stack that perm
// belongs to (see docs/data-model.md, "Authorization rules"). perm is the
// user's permission on that stack; a zero Permission means no grant.
// Unknown actions are denied.
func Allowed(user store.User, perm store.Permission, action Action) bool {
	if user.Disabled {
		return false
	}
	if user.IsAdmin {
		return action >= ActionView && action <= ActionUpdate
	}
	if perm.UserID != user.ID {
		return false // a permission of someone else
	}
	switch action {
	case ActionView:
		return !perm.None() // any grant includes viewing the stack
	case ActionBackup:
		return perm.CanBackup
	case ActionRestore:
		return perm.CanRestore
	case ActionLogs:
		return perm.CanLogs
	case ActionUpdate:
		return perm.CanUpdate
	}
	return false
}
