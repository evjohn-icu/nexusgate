package secretstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStorePersistsEncryptedSecretWithoutPlaintext(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("provider/gemini-a", "very-secret-api-key"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.Get("provider/gemini-a")
	if err != nil || !ok || got != "very-secret-api-key" {
		t.Fatalf("Get() = %q, %t, %v", got, ok, err)
	}
	path := filepath.Join(dir, "provider-secrets", "provider-secrets.enc")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "very-secret-api-key" || bytes.Contains(raw, []byte("very-secret-api-key")) {
		t.Fatal("secret store wrote an API key in plaintext")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestStoreRefusesEmptyReference(t *testing.T) {
	store, err := Open(t.TempDir(), "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("", "key"); err == nil {
		t.Fatal("Put accepted an empty reference")
	}
}

func TestStoreCRUDUsesResolveHasAndDelete(t *testing.T) {
	store, err := Open(t.TempDir(), "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}

	const ref = "provider/gemini-a"
	const value = "very-secret-api-key"
	if err := store.Put(ref, value); err != nil {
		t.Fatal(err)
	}
	has, err := store.Has(context.Background(), ref)
	if err != nil || !has {
		t.Fatalf("Has() = %t, %v; want true, nil", has, err)
	}
	got, ok, err := store.Resolve(ref)
	if err != nil || !ok || got != value {
		t.Fatalf("Resolve() = %q, %t, %v", got, ok, err)
	}
	if err := store.Delete(ref); err != nil {
		t.Fatal(err)
	}
	has, err = store.Has(context.Background(), ref)
	if err != nil || has {
		t.Fatalf("Has() after Delete = %t, %v; want false, nil", has, err)
	}
	got, ok, err = store.Resolve(ref)
	if err != nil || ok || got != "" {
		t.Fatalf("Resolve() after Delete = %q, %t, %v; want empty, false, nil", got, ok, err)
	}
}

func TestStoreRejectsUnsafeReferences(t *testing.T) {
	store, err := Open(t.TempDir(), "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		" ",
		"../escape",
		"provider/../escape",
		"/absolute",
		"provider//empty",
		"provider/./dot",
		`provider\\escape`,
		" provider/key",
		"provider/key ",
		"provider/key\n",
	} {
		if err := store.Put(ref, "secret-value"); err == nil {
			t.Errorf("Put accepted unsafe reference %q", ref)
		}
		if _, err := store.Has(context.Background(), ref); err == nil {
			t.Errorf("Has accepted unsafe reference %q", ref)
		}
		if _, _, err := store.Resolve(ref); err == nil {
			t.Errorf("Resolve accepted unsafe reference %q", ref)
		}
		if err := store.Delete(ref); err == nil {
			t.Errorf("Delete accepted unsafe reference %q", ref)
		}
	}
}

func TestStoreCreatesPrivateSecretDirectoryInsideSharedDataDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err != nil {
		t.Fatalf("Open() shared data directory: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "provider-secrets"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("secret directory mode = %o, want 700", info.Mode().Perm())
	}
}

func TestStoreRefusesInsecureExistingFile(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	const value = "very-secret-api-key"
	if err := store.Put("provider/gemini-a", value); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider-secrets", filename)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err == nil {
		t.Fatal("Open accepted a secret file without mode 0600")
	} else if strings.Contains(err.Error(), value) {
		t.Fatalf("Open error leaked secret value: %v", err)
	}
}

func TestStoreFailsClosedWhenOpenedFileBecomesInsecure(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	const value = "very-secret-api-key"
	if err := store.Put("provider/gemini-a", value); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "provider-secrets", filename), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve("provider/gemini-a"); err == nil {
		t.Fatal("Resolve returned a secret after the store file became insecure")
	} else if strings.Contains(err.Error(), value) {
		t.Fatalf("Resolve error leaked secret value: %v", err)
	}
}

func TestStoreRejectsCorruptDecryptedContent(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	const value = "corrupt-content-secret"
	writeEncryptedMap(t, filepath.Join(dir, "provider-secrets", filename), store, map[string]string{
		"../unsafe": value,
	})
	if _, err := Open(dir, "hub-admin-token-for-test"); err == nil {
		t.Fatal("Open accepted decrypted content with an unsafe reference")
	} else if strings.Contains(err.Error(), value) {
		t.Fatalf("Open error leaked secret value: %v", err)
	}
}

func TestStoreRedactsSecretsFromStringAndJSON(t *testing.T) {
	store, err := Open(t.TempDir(), "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	const value = "string-and-json-secret"
	if err := store.Put("provider/gemini-a", value); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(store); strings.Contains(got, value) {
		t.Fatalf("String() leaked secret value: %q", got)
	}
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"provider/gemini-a"`)) || bytes.Contains(raw, []byte(value)) {
		t.Fatalf("JSON listing = %s; want ref but not secret value", raw)
	}
}

func TestStoreConcurrentWritesAndReads(t *testing.T) {
	store, err := Open(t.TempDir(), "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 24
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref := fmt.Sprintf("provider/concurrent-%d", i)
			value := fmt.Sprintf("secret-%d", i)
			if err := store.Put(ref, value); err != nil {
				t.Errorf("Put(%q): %v", ref, err)
				return
			}
			got, ok, err := store.Resolve(ref)
			if err != nil || !ok || got != value {
				t.Errorf("Resolve(%q) = %q, %t, %v", ref, got, ok, err)
			}
		}()
	}
	wg.Wait()
	for i := 0; i < workers; i++ {
		ref := fmt.Sprintf("provider/concurrent-%d", i)
		has, err := store.Has(context.Background(), ref)
		if err != nil || !has {
			t.Fatalf("Has(%q) = %t, %v; want true, nil", ref, has, err)
		}
	}
}

func writeEncryptedMap(t *testing.T, path string, store *Store, secrets map[string]string) {
	t.Helper()
	plain, err := json.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, store.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(encryptedFile{
		Version:    1,
		Nonce:      nonce,
		Ciphertext: store.gcm.Seal(nil, nonce, plain, nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Rotating the Hub administrator token used to make the store permanently
// undecryptable, which bricked every CLI command because Service construction
// opens it. The data-encryption key is now independent of the token.
func TestStoreSurvivesAdminTokenRotation(t *testing.T) {
	dir := t.TempDir()

	original, err := Open(dir, "original-admin-token")
	if err != nil {
		t.Fatalf("open with original token: %v", err)
	}
	if err := original.Put("provider-channel/c1/m1", "sk-live-value"); err != nil {
		t.Fatalf("put: %v", err)
	}

	rotated, err := Open(dir, "rotated-admin-token")
	if err != nil {
		t.Fatalf("open after rotation should succeed: %v", err)
	}
	value, ok, err := rotated.Resolve("provider-channel/c1/m1")
	if err != nil || !ok {
		t.Fatalf("secret should survive rotation: ok=%v err=%v", ok, err)
	}
	if value != "sk-live-value" {
		t.Fatalf("secret value = %q, want %q", value, "sk-live-value")
	}
}

// A store written by the pre-key-file scheme must migrate on first open, keep
// its contents, and leave a backup of the original ciphertext behind.
func TestStoreMigratesLegacyTokenDerivedStore(t *testing.T) {
	dir := t.TempDir()
	secretDir := filepath.Join(dir, "provider-secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatalf("prepare secret dir: %v", err)
	}

	// Write a store the old way: key derived from the admin token, no store.key.
	legacyGCM, err := newGCM(legacyKey("legacy-token"))
	if err != nil {
		t.Fatalf("legacy cipher: %v", err)
	}
	legacy := &Store{path: filepath.Join(secretDir, filename), gcm: legacyGCM, secrets: map[string]string{"ref/one": "legacy-secret"}}
	if err := legacy.persistLocked(); err != nil {
		t.Fatalf("write legacy store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, keyFilename)); !os.IsNotExist(err) {
		t.Fatal("legacy fixture should not have a key file")
	}

	migrated, err := Open(dir, "legacy-token")
	if err != nil {
		t.Fatalf("open should migrate legacy store: %v", err)
	}
	value, ok, err := migrated.Resolve("ref/one")
	if err != nil || !ok || value != "legacy-secret" {
		t.Fatalf("migrated secret = %q ok=%v err=%v", value, ok, err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, keyFilename)); err != nil {
		t.Fatalf("migration should create the key file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(secretDir, filename+backupSuffix)); err != nil {
		t.Fatalf("migration should back up the original ciphertext: %v", err)
	}

	// Rotation is safe from here on.
	if _, err := Open(dir, "some-other-token"); err != nil {
		t.Fatalf("post-migration rotation should succeed: %v", err)
	}
}

// A legacy store that can no longer be decrypted must fail with actionable
// guidance rather than a bare "decrypt payload", and must not be overwritten.
func TestStoreReportsRotatedTokenOnUnmigratableLegacyStore(t *testing.T) {
	dir := t.TempDir()
	secretDir := filepath.Join(dir, "provider-secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatalf("prepare secret dir: %v", err)
	}
	legacyGCM, err := newGCM(legacyKey("the-original-token"))
	if err != nil {
		t.Fatalf("legacy cipher: %v", err)
	}
	path := filepath.Join(secretDir, filename)
	legacy := &Store{path: path, gcm: legacyGCM, secrets: map[string]string{"ref/one": "legacy-secret"}}
	if err := legacy.persistLocked(); err != nil {
		t.Fatalf("write legacy store: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	_, err = Open(dir, "a-different-token")
	if !errors.Is(err, ErrAdminTokenRotated) {
		t.Fatalf("error should identify token rotation, got %v", err)
	}
	if strings.Contains(err.Error(), "a-different-token") || strings.Contains(err.Error(), "legacy-secret") {
		t.Fatal("error must not leak token or secret material")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after failed open: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a failed migration must leave the original ciphertext untouched")
	}
}
