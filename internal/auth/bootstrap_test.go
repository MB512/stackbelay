package auth

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/secrets"
	"github.com/MB512/stackvault/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	box, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

var bannerPassword = regexp.MustCompile(`INITIAL ADMIN PASSWORD \(user "admin"\): (\S+)`)

// bootstrap runs EnsureInitialAdmin and returns the password from the
// banner ("" if no banner was printed).
func bootstrap(t *testing.T, st *store.Store) string {
	t.Helper()
	var banner bytes.Buffer
	if err := EnsureInitialAdmin(context.Background(), st, &banner, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	m := bannerPassword.FindStringSubmatch(banner.String())
	if m == nil {
		if banner.Len() > 0 {
			t.Fatalf("unexpected banner %q", banner.String())
		}
		return ""
	}
	return m[1]
}

func TestInitialAdminLifecycle(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	// First start: admin created, password shown.
	pw1 := bootstrap(t, st)
	if pw1 == "" {
		t.Fatal("no password on first start")
	}
	u, err := st.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !u.IsAdmin || !u.MustChangePassword || CheckPassword(u.PasswordHash, pw1) != nil {
		t.Fatalf("unexpected admin %+v", u)
	}
	if err := st.CreateSession(ctx, store.Session{IDHash: "s", UserID: u.ID, CSRFToken: "c", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// Restart without change: new password, old one invalid, sessions gone.
	pw2 := bootstrap(t, st)
	if pw2 == "" || pw2 == pw1 {
		t.Fatalf("second start: pw1=%q pw2=%q", pw1, pw2)
	}
	u, _ = st.UserByUsername(ctx, "admin")
	if CheckPassword(u.PasswordHash, pw1) == nil || CheckPassword(u.PasswordHash, pw2) != nil {
		t.Fatal("password not replaced")
	}
	if _, _, err := st.SessionWithUser(ctx, "s", time.Now()); err == nil {
		t.Fatal("session with old generated password survived")
	}

	// Admin sets their own password: nothing printed anymore.
	hash, _ := HashPassword("my own long password")
	if err := st.SetPassword(ctx, u.ID, hash, false); err != nil {
		t.Fatal(err)
	}
	if pw := bootstrap(t, st); pw != "" {
		t.Fatalf("password printed after it was changed: %q", pw)
	}
	u, _ = st.UserByUsername(ctx, "admin")
	if CheckPassword(u.PasswordHash, "my own long password") != nil {
		t.Fatal("own password was overwritten")
	}

	audit, _ := st.ListAudit(ctx, 10)
	if len(audit) != 2 || audit[1].Action != "admin.bootstrap" || audit[0].Action != "admin.password_regenerated" {
		t.Fatalf("unexpected audit log %+v", audit)
	}
}

func TestInitialAdminNotCreatedWhenOtherAdminExists(t *testing.T) {
	st := openStore(t)
	if _, err := st.CreateUser(context.Background(), store.User{Username: "alice", PasswordHash: "h", IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if pw := bootstrap(t, st); pw != "" {
		t.Fatal("admin created although an admin exists")
	}
	if _, err := st.UserByUsername(context.Background(), "admin"); err == nil {
		t.Fatal("admin user exists")
	}
}

func TestInitialAdminRecoveryWhenNoActiveAdmin(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	hash, _ := HashPassword("old password 123")
	id, err := st.CreateUser(ctx, store.User{Username: "admin", PasswordHash: hash, IsAdmin: false})
	if err != nil {
		t.Fatal(err)
	}
	pw := bootstrap(t, st)
	u, _ := st.UserByID(ctx, id)
	if pw == "" || !u.IsAdmin || !u.MustChangePassword {
		t.Fatalf("not recovered: pw=%q user=%+v", pw, u)
	}
}
