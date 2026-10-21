package relay

import (
	"bytes"
	"meshra/internal/model"
	"os"
	"path/filepath"
	"testing"
)

func TestConsoleAccountPersistenceRotationAndRecovery(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { r.Close() }()
	a, err := r.ConsoleAccount(admin)
	if err != nil || a.Configured {
		t.Fatal(a, err)
	}
	if _, err = r.SaveConsoleAccount(model.Principal{}, "operator", "a long test passphrase", " "); err == nil {
		t.Fatal("non-admin set credentials")
	}
	for _, v := range []struct{ user, pass string }{{"ab", "a long test passphrase"}, {"has space", "a long test passphrase"}, {"operator", "short"}} {
		if _, err = r.SaveConsoleAccount(admin, v.user, v.pass, ""); err == nil {
			t.Fatal("invalid credentials accepted")
		}
	}
	password := "a long test passphrase 🔑"
	a, err = r.SaveConsoleAccount(admin, "operator", password, "")
	if err != nil || !a.Configured {
		t.Fatal(a, err)
	}
	var salt, hash []byte
	if err = r.db.QueryRow("SELECT salt,password_hash FROM console_account WHERE id=1").Scan(&salt, &hash); err != nil {
		t.Fatal(err)
	}
	if len(salt) != 32 || len(hash) != 32 || bytes.Contains(hash, []byte(password)) {
		t.Fatal("password storage")
	}
	for _, v := range []struct{ user, pass string }{{"unknown", password}, {"operator", "incorrect"}, {"operator", ""}} {
		if _, err = r.AuthenticateConsole(v.user, v.pass); err == nil {
			t.Fatal("invalid login accepted")
		}
	}
	if p, err := r.AuthenticateConsole("operator", password); err != nil || !p.Admin {
		t.Fatal(p, err)
	}
	if _, err = r.SaveConsoleAccount(admin, "operator", "a replacement password", "incorrect"); err == nil {
		t.Fatal("password replaced without reauthentication")
	}
	if _, err = r.SaveConsoleAccount(admin, "operator", "a replacement password", password); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateConsole("operator", password); err == nil {
		t.Fatal("old password valid")
	}
	token, err := os.ReadFile(filepath.Join(dir, "admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.SaveConsoleAccount(admin, "recovered", "a recovered passphrase", string(token)); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := r.AuthenticateConsole("recovered", "a recovered passphrase"); err != nil || !p.Admin {
		t.Fatal(p, err)
	}
	if p, err := r.Authenticate(string(token)); err != nil || !p.Admin {
		t.Fatal("CLI recovery token changed", err)
	}
}
