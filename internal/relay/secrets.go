package relay

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"meshra/internal/model"
)

// The vault is write-only through the public surface. Operator input is
// accepted, encrypted at rest and handed back only to the process that needs
// the value. No summary, event, log line or tool text contains a decrypted
// value, and the key never leaves the private data directory.
const (
	secretKeyFile       = "automation.key"
	secretKeyBytes      = 32
	secretMaskToken     = "[REDACTED]"
	secretMaskMinLength = 8
	secretKindAPIKey    = "endpoint.apiKey"
	secretKindACPEnv    = "acp.env"
	secretKindGeneric   = "generic"
)

// Registered secret values are masked in persisted events, logs and tool text.
// Registration is additive and process-local: the values are never written to
// the database outside the encrypted record.
var secretRegistry = struct {
	mu     sync.RWMutex
	values []string
}{}

// registerSecretValue remembers a plaintext value and the common URL-escaped
// forms a provider or connector may echo back. Short values are ignored: masking
// them would corrupt unrelated text without protecting a meaningful credential.
func registerSecretValue(value string) {
	if len(value) < secretMaskMinLength {
		return
	}
	forms := []string{value}
	if escaped := url.QueryEscape(value); escaped != value {
		forms = append(forms, escaped)
	}
	if escaped := url.PathEscape(value); escaped != value {
		forms = append(forms, escaped)
	}
	secretRegistry.mu.Lock()
	defer secretRegistry.mu.Unlock()
	for _, form := range forms {
		known := false
		for _, existing := range secretRegistry.values {
			if existing == form {
				known = true
				break
			}
		}
		if !known {
			secretRegistry.values = append(secretRegistry.values, form)
		}
	}
	// Longest first: an overlapping shorter value must not mask part of a
	// longer credential and leave a readable remainder.
	sort.SliceStable(secretRegistry.values, func(i, j int) bool { return len(secretRegistry.values[i]) > len(secretRegistry.values[j]) })
}

// maskSecretValues replaces every registered secret value with a fixed token.
// It is deliberately not a detector: unregistered credentials remain possible,
// which is why access control and origin pinning stay in place.
func maskSecretValues(s string) string {
	if s == "" {
		return s
	}
	secretRegistry.mu.RLock()
	defer secretRegistry.mu.RUnlock()
	for _, value := range secretRegistry.values {
		if strings.Contains(s, value) {
			s = strings.ReplaceAll(s, value, secretMaskToken)
		}
	}
	return s
}

// redactSecrets applies the relay redactor, which already masks registered
// vault values. Callers that persist untrusted text use this instead of redact.
func redactSecrets(s string) string { return redact(s) }

// secretKeyPath is the vault key inside the existing private data directory.
func (r *Relay) secretKeyPath() string { return filepath.Join(filepath.Dir(r.lock.Name()), secretKeyFile) }

// readSecretKey loads the 32 byte vault key. The file must be a regular file
// with no group or world permission; a symlink or a loosened mode is a failure,
// not something to repair silently.
func (r *Relay) readSecretKey() ([]byte, error) {
	fd, err := unix.Open(r.secretKeyPath(), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, model.Err(503, "secret vault key is unavailable")
	}
	file := os.NewFile(uintptr(fd), r.secretKeyPath())
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, model.Err(503, "secret vault key is unavailable")
	}
	if !info.Mode().IsRegular() || info.Size() != secretKeyBytes || info.Mode().Perm()&0o077 != 0 {
		return nil, model.Err(503, "secret vault key is unavailable")
	}
	key := make([]byte, secretKeyBytes)
	if _, err = io.ReadFull(file, key); err != nil {
		return nil, model.Err(503, "secret vault key is unavailable")
	}
	return key, nil
}

// createSecretKey writes a fresh key with exclusive creation and 0600 mode.
// A concurrent creator wins the race and its key is used.
func (r *Relay) createSecretKey() ([]byte, error) {
	key := make([]byte, secretKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, model.Err(503, "secret vault key could not be generated")
	}
	fd, err := unix.Open(r.secretKeyPath(), unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return r.readSecretKey()
		}
		return nil, model.Err(503, "secret vault key could not be created")
	}
	file := os.NewFile(uintptr(fd), r.secretKeyPath())
	if _, err = file.Write(key); err != nil {
		file.Close()
		os.Remove(r.secretKeyPath())
		return nil, model.Err(503, "secret vault key could not be written")
	}
	if err = file.Sync(); err != nil {
		file.Close()
		os.Remove(r.secretKeyPath())
		return nil, model.Err(503, "secret vault key could not be written")
	}
	if err = file.Close(); err != nil {
		os.Remove(r.secretKeyPath())
		return nil, model.Err(503, "secret vault key could not be written")
	}
	if err = os.Chmod(r.secretKeyPath(), 0o600); err != nil {
		return nil, model.Err(503, "secret vault key permissions are unsafe")
	}
	return key, nil
}

// ensureSecretKey returns the vault key. A missing key is created only while
// the vault holds no ciphertext: regenerating a key over existing records would
// destroy every stored secret, so that case fails closed with 503.
func (r *Relay) ensureSecretKey(create bool) ([]byte, error) {
	key, err := r.readSecretKey()
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var count int
	if err = r.db.QueryRow("SELECT count(*) FROM automation_secrets").Scan(&count); err != nil {
		return nil, model.Err(503, "secret vault is unavailable")
	}
	if count > 0 {
		return nil, model.Err(503, "secret vault key is unavailable")
	}
	if !create {
		return nil, model.Err(503, "secret vault key is unavailable")
	}
	return r.createSecretKey()
}

// secretAssociatedData binds a ciphertext to its identifier and kind, so a
// record cannot be replayed under a different name or purpose.
func secretAssociatedData(id, kind string) []byte { return []byte("meshra.automation.secret\x00" + id + "\x00" + kind) }

func (r *Relay) encryptSecretValue(key []byte, id, kind, value string) ([]byte, []byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, model.Err(503, "secret vault is unavailable")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, model.Err(503, "secret vault is unavailable")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, model.Err(503, "secret vault is unavailable")
	}
	return nonce, gcm.Seal(nil, nonce, []byte(value), secretAssociatedData(id, kind)), nil
}

func (r *Relay) decryptSecretValue(key []byte, id, kind string, nonce, ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", model.Err(503, "secret cannot be decrypted")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", model.Err(503, "secret cannot be decrypted")
	}
	if len(nonce) != gcm.NonceSize() || len(ciphertext) == 0 {
		return "", model.Err(503, "secret cannot be decrypted")
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, secretAssociatedData(id, kind))
	if err != nil {
		return "", model.Err(503, "secret cannot be decrypted")
	}
	return string(plain), nil
}

// prepareSecret validates the operator input and returns the material for one
// encrypted record. The plaintext is registered with the redactor immediately.
func (r *Relay) prepareSecret(kind, value string) (string, []byte, []byte, error) {
	if kind == "" || len(kind) > 64 || !safeRegistryText(kind) {
		return "", nil, nil, model.Err(400, "invalid secret kind")
	}
	if value == "" {
		return "", nil, nil, model.Err(400, "secret value must not be empty")
	}
	if len(value) > 64<<10 {
		return "", nil, nil, model.Err(413, "secret value is too large")
	}
	key, err := r.ensureSecretKey(true)
	if err != nil {
		return "", nil, nil, err
	}
	id := randomID()
	nonce, ciphertext, err := r.encryptSecretValue(key, id, kind, value)
	if err != nil {
		return "", nil, nil, err
	}
	registerSecretValue(value)
	return id, nonce, ciphertext, nil
}

func (r *Relay) insertSecret(ctx context.Context, tx *sql.Tx, id, kind string, nonce, ciphertext []byte) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO automation_secrets(id,kind,nonce,ciphertext,updated_at) VALUES(?,?,?,?,?)", id, kind, nonce, ciphertext, now().Format(time.RFC3339Nano))
	return err
}

// storeSecret encrypts and persists one operator supplied value and returns
// its opaque reference. The value itself is never returned.
func (r *Relay) storeSecret(ctx context.Context, kind, value string) (string, error) {
	id, nonce, ciphertext, err := r.prepareSecret(kind, value)
	if err != nil {
		return "", err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = r.insertSecret(ctx, tx, id, kind, nonce, ciphertext); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// readSecret returns the plaintext for process-internal use. Callers must not
// persist, log or return the value; the vault itself never exposes it.
func (r *Relay) readSecret(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", model.Err(404, "secret not found")
	}
	var kind string
	var nonce, ciphertext []byte
	err := r.db.QueryRowContext(ctx, "SELECT kind,nonce,ciphertext FROM automation_secrets WHERE id=?", id).Scan(&kind, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", model.Err(404, "secret not found")
	}
	if err != nil {
		return "", model.Err(503, "secret vault is unavailable")
	}
	key, err := r.ensureSecretKey(false)
	if err != nil {
		return "", err
	}
	value, err := r.decryptSecretValue(key, id, kind, nonce, ciphertext)
	if err != nil {
		return "", err
	}
	registerSecretValue(value)
	return value, nil
}

// secretStatus reports missing, saved or unreadable without revealing any
// part of the value.
func (r *Relay) secretStatus(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "missing", nil
	}
	var count int
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM automation_secrets WHERE id=?", id).Scan(&count); err != nil {
		return "", model.Err(503, "secret vault is unavailable")
	}
	if count == 0 {
		return "missing", nil
	}
	if _, err := r.readSecret(ctx, id); err != nil {
		return "unreadable", nil
	}
	return "saved", nil
}

// secretExistsLocked verifies that a reference points at a stored secret.
func (r *Relay) secretExistsLocked(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var count int
	if err := r.db.QueryRowContext(ctx, "SELECT count(*) FROM automation_secrets WHERE id=?", id).Scan(&count); err != nil {
		return false, model.Err(503, "secret vault is unavailable")
	}
	return count > 0, nil
}

// deleteSecretLocked removes an encrypted record. Callers hold relay
// ownership and must not hold an open transaction: the relay owns a single
// database connection.
func (r *Relay) deleteSecretLocked(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, err := r.db.ExecContext(ctx, "DELETE FROM automation_secrets WHERE id=?", id)
	return err
}
