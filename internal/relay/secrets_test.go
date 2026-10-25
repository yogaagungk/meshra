package relay

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secretTestValue() string { return "vault-only-value-9876543210" }

func TestSecretRoundTripAndWrongKeyFailure(t *testing.T) {
	r, dir := openTest(t, nil)
	ctx := context.Background()
	id, err := r.storeSecret(ctx, secretKindGeneric, secretTestValue())
	if err != nil {
		t.Fatal(err)
	}
	value, err := r.readSecret(ctx, id)
	if err != nil || value != secretTestValue() {
		t.Fatalf("secret round trip: %q %v", value, err)
	}
	status, err := r.secretStatus(ctx, id)
	if err != nil || status != "saved" {
		t.Fatalf("secret status: %q %v", status, err)
	}
	// A different key must fail closed instead of returning garbage.
	other := make([]byte, secretKeyBytes)
	other[0] = 0x5a
	if err = os.WriteFile(filepath.Join(dir, secretKeyFile), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = r.readSecret(ctx, id); err == nil {
		t.Fatal("a wrong key decrypted the secret")
	} else {
		requireStatus(t, err, 503)
	}
	status, err = r.secretStatus(ctx, id)
	if err != nil || status != "unreadable" {
		t.Fatalf("wrong key status: %q %v", status, err)
	}
}

func TestSecretKeyIsExclusiveAndPrivate(t *testing.T) {
	r, dir := openTest(t, nil)
	ctx := context.Background()
	if _, err := r.storeSecret(ctx, secretKindGeneric, secretTestValue()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, secretKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("vault key permissions: %v", info.Mode().Perm())
	}
	if info.Size() != secretKeyBytes {
		t.Fatalf("vault key size: %d", info.Size())
	}
}

func TestSecretMissingKeyWithCiphertextFailsClosed(t *testing.T) {
	r, dir := openTest(t, nil)
	ctx := context.Background()
	id, err := r.storeSecret(ctx, secretKindGeneric, secretTestValue())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, secretKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err = r.readSecret(ctx, id); err == nil {
		t.Fatal("a missing key returned a secret")
	} else {
		requireStatus(t, err, 503)
	}
	if _, err = os.Stat(filepath.Join(dir, secretKeyFile)); !os.IsNotExist(err) {
		t.Fatal("the vault key was regenerated over existing ciphertext")
	}
	if _, err = r.storeSecret(ctx, secretKindGeneric, "another-vault-value-123456"); err == nil {
		t.Fatal("a new secret was stored without the original key")
	} else {
		requireStatus(t, err, 503)
	}
}

func TestSecretValueNeverAppearsInDTOsOrEvents(t *testing.T) {
	r, _ := openTest(t, nil)
	ctx := context.Background()
	value := "vault-endpoint-value-1122334455"
	payload, err := json.Marshal(map[string]any{
		"endpoint": map[string]any{
			"name":            "Local model",
			"baseUrl":         "http://127.0.0.1:11434/v1",
			"allowInsecureHttp": true,
			"enabled":         true,
		},
		"apiKey": value,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.connectionsOperation(ctx, admin, "connections.saveEndpoint", payload)
	if err != nil {
		t.Fatal(err)
	}
	view, ok := result.(ProviderEndpointView)
	if !ok {
		t.Fatalf("unexpected view type %T", result)
	}
	if view.SecretStatus != "saved" {
		t.Fatalf("secret status: %q", view.SecretStatus)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), value) {
		t.Fatal("the endpoint summary exposed the secret value")
	}
	var body string
	if err = r.db.QueryRow("SELECT body FROM provider_endpoints WHERE id=?", view.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, value) {
		t.Fatal("the stored endpoint exposed the secret value")
	}
	rows, err := r.db.Query("SELECT data FROM automation_events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(data, value) {
			t.Fatal("an automation event exposed the secret value")
		}
	}
	var ciphertext string
	var nonce []byte
	if err = r.db.QueryRow("SELECT ciphertext,nonce FROM automation_secrets WHERE id=?", view.SecretRef).Scan(&ciphertext, &nonce); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ciphertext, value) {
		t.Fatal("the secret was stored in plaintext")
	}
}

func TestRegisteredSecretValuesAreMasked(t *testing.T) {
	value := "vault secret/with+special=chars1234567890"
	registerSecretValue(value)
	masked := maskSecretValues("bearer " + value)
	if strings.Contains(masked, value) {
		t.Fatalf("the raw value survived masking: %q", masked)
	}
	if !strings.Contains(masked, secretMaskToken) {
		t.Fatalf("masking produced no token: %q", masked)
	}
	escaped := url.QueryEscape(value)
	masked = maskSecretValues("token=" + escaped)
	if strings.Contains(masked, escaped) {
		t.Fatalf("the escaped value survived masking: %q", masked)
	}
	path := url.PathEscape(value)
	masked = maskSecretValues("path/" + path)
	if strings.Contains(masked, path) {
		t.Fatalf("the path escaped value survived masking: %q", masked)
	}
}

func TestSecretStoreRejectsEmptyAndOversizedValues(t *testing.T) {
	r, _ := openTest(t, nil)
	ctx := context.Background()
	if _, err := r.storeSecret(ctx, secretKindGeneric, ""); err == nil {
		t.Fatal("an empty secret was stored")
	} else {
		requireStatus(t, err, 400)
	}
	if _, err := r.storeSecret(ctx, secretKindGeneric, strings.Repeat("x", (64<<10)+1)); err == nil {
		t.Fatal("an oversized secret was stored")
	} else {
		requireStatus(t, err, 413)
	}
}
