package relay

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"meshra/internal/model"
	"regexp"
	"unicode/utf8"
)

const passwordIterations = 600000

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,63}$`)

func (r *Relay) ConsoleAccount(p model.Principal) (model.ConsoleAccount, error) {
	if err := requireAdmin(p); err != nil {
		return model.ConsoleAccount{}, err
	}
	var a model.ConsoleAccount
	err := r.db.QueryRow("SELECT username FROM console_account WHERE id=1").Scan(&a.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	a.Configured = err == nil
	return a, err
}

func (r *Relay) AuthenticateConsole(username, password string) (model.Principal, error) {
	var stored string
	var salt, hash []byte
	var iterations int
	err := r.db.QueryRow("SELECT username,salt,password_hash,iterations FROM console_account WHERE id=1").Scan(&stored, &salt, &hash, &iterations)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.Principal{}, err
	}
	if len(password) > 1024 || len(username) > 64 {
		return model.Principal{}, model.Err(401, "invalid username or password")
	}
	// Keep unknown-user and incorrect-password checks on the same expensive path.
	if errors.Is(err, sql.ErrNoRows) {
		salt = make([]byte, 32)
		hash = make([]byte, 32)
		iterations = passwordIterations
	}
	if iterations != passwordIterations || len(salt) != 32 || len(hash) != 32 {
		return model.Principal{}, model.Err(500, "invalid console credential configuration")
	}
	derived, e := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if e != nil {
		return model.Principal{}, e
	}
	passwordOK := subtle.ConstantTimeCompare(hash, derived)
	userOK := subtle.ConstantTimeCompare([]byte(stored), []byte(username))
	if err != nil || passwordOK&userOK != 1 {
		return model.Principal{}, model.Err(401, "invalid username or password")
	}
	return model.Principal{Admin: true}, nil
}

func (r *Relay) SaveConsoleAccount(p model.Principal, username, password, current string) (model.ConsoleAccount, error) {
	if err := requireAdmin(p); err != nil {
		return model.ConsoleAccount{}, err
	}
	if !usernamePattern.MatchString(username) {
		return model.ConsoleAccount{}, model.Err(400, "username must be 3–64 letters, numbers, dots, underscores or hyphens, starting with a letter or number")
	}
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 15 || len(password) > 1024 {
		return model.ConsoleAccount{}, model.Err(400, "password must contain at least 15 characters and at most 1024 bytes")
	}
	// Serialize credential changes, but do not hold the relay's runtime lock during hashing.
	r.accountMu.Lock()
	defer r.accountMu.Unlock()
	account, err := r.ConsoleAccount(p)
	if err != nil {
		return model.ConsoleAccount{}, err
	}
	if account.Configured {
		_, err = r.AuthenticateConsole(account.Username, current)
		recoveryOK := current != "" && subtle.ConstantTimeCompare([]byte(current), []byte(r.admin)) == 1
		if err != nil && !recoveryOK {
			return model.ConsoleAccount{}, model.Err(403, "current password or administrator recovery token is incorrect")
		}
	}
	salt := make([]byte, 32)
	if _, err = rand.Read(salt); err != nil {
		return model.ConsoleAccount{}, err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return model.ConsoleAccount{}, err
	}
	_, err = r.db.Exec(`INSERT INTO console_account(id,username,salt,password_hash,iterations) VALUES(1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,salt=excluded.salt,password_hash=excluded.password_hash,iterations=excluded.iterations`, username, salt, hash, passwordIterations)
	return model.ConsoleAccount{Configured: true, Username: username}, err
}
