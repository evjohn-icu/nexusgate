package secretstore

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// lockStoreDirectory prepares a private store directory without opening a
// store, for journal- and migration-level tests.
func lockStoreDirectory(t *testing.T) (dir, secretDir, path, keyPath string) {
	t.Helper()
	dir = t.TempDir()
	secretDir = filepath.Join(dir, "provider-secrets")
	if err := ensurePrivateDirectory(secretDir); err != nil {
		t.Fatalf("prepare secret dir: %v", err)
	}
	return dir, secretDir, filepath.Join(secretDir, filename), filepath.Join(secretDir, keyFilename)
}

// writeLegacyStore encrypts secrets under the pre-migration token derivation.
func writeLegacyStore(t *testing.T, path string, secrets map[string]string) []byte {
	t.Helper()
	legacyGCM, err := newGCM(legacyKey("legacy-token"))
	if err != nil {
		t.Fatalf("legacy cipher: %v", err)
	}
	legacy := &Store{path: path, gcm: legacyGCM, secrets: secrets}
	if err := legacy.persistLocked(); err != nil {
		t.Fatalf("write legacy store: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read legacy store: %v", err)
	}
	return raw
}

func migrationOps(t *testing.T, failRename func(destination string) bool) fileOps {
	t.Helper()
	ops := defaultFileOps()
	ops.rename = func(source, destination string) error {
		if err := os.Rename(source, destination); err != nil {
			return err
		}
		if failRename(destination) {
			return errors.New("injected rename failure")
		}
		return nil
	}
	return ops
}

func renameCounter(t *testing.T, target string, failOn int) (func(destination string) bool, func() int) {
	t.Helper()
	hits := 0
	fail := func(destination string) bool {
		if filepath.Base(destination) != target {
			return false
		}
		hits++
		return hits == failOn
	}
	return fail, func() int { return hits }
}

// A fault at every publication boundary of the journaled legacy migration must
// leave the next Open with a complete state — either the untouched legacy
// store, which migration then completes normally, or the fully published new
// store — with the legacy backup always preserved.
func TestLegacyMigrationFaultsRecoverCompleteState(t *testing.T) {
	tests := []struct {
		name       string
		failTarget string
		failOn     int
	}{
		{"prepared journal write fails", rekeyJournal, 1},
		{"ciphertext commit fails", filename, 1},
		{"key commit fails", keyFilename, 1},
		{"commit marker write fails", rekeyJournal, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, path, keyPath := lockStoreDirectory(t)
			legacy := writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})

			failRename, hits := renameCounter(t, tc.failTarget, tc.failOn)
			if err := migrateLegacyStore(path, keyPath, "legacy-token", migrationOps(t, failRename)); err == nil {
				t.Fatalf("injected failure at %s did not fail the migration", tc.failTarget)
			}
			if hits() == 0 {
				t.Fatalf("failure hook never fired for %s", tc.failTarget)
			}

			reopened, err := Open(dir, "legacy-token")
			if err != nil {
				t.Fatalf("Open after failed migration: %v", err)
			}
			value, ok, err := reopened.Resolve("ref/one")
			if err != nil || !ok || value != "legacy-secret" {
				t.Fatalf("recovered secret = %q, %t, %v", value, ok, err)
			}
			backup, err := os.ReadFile(path + backupSuffix)
			if err != nil {
				t.Fatalf("legacy backup missing after recovery: %v", err)
			}
			if string(backup) != string(legacy) {
				t.Fatal("legacy backup changed during recovery")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), rekeyJournal)); !os.IsNotExist(err) {
				t.Fatalf("journal remains after recovery: %v", err)
			}
			if _, err := os.Stat(keyPath); err != nil {
				t.Fatalf("key file missing after recovery: %v", err)
			}
		})
	}
}

func TestLegacyMigrationSuccessKeepsBackupAndCompletes(t *testing.T) {
	dir, _, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})

	if err := migrateLegacyStore(path, keyPath, "legacy-token", defaultFileOps()); err != nil {
		t.Fatalf("migrateLegacyStore: %v", err)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatalf("legacy backup must survive a successful migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), rekeyJournal)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after success: %v", err)
	}
	reopened, err := Open(dir, "legacy-token")
	if err != nil {
		t.Fatal(err)
	}
	if value, ok, err := reopened.Resolve("ref/one"); err != nil || !ok || value != "legacy-secret" {
		t.Fatalf("migrated secret = %q, %t, %v", value, ok, err)
	}
}

func TestMigrationRejectionLeavesLegacyStoreUntouched(t *testing.T) {
	_, _, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrateLegacyStore(path, keyPath, "a-different-token", defaultFileOps()); !errors.Is(err, ErrAdminTokenRotated) {
		t.Fatalf("error should identify token rotation, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed migration must leave the legacy ciphertext untouched")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("failed migration must not create a key file: %v", err)
	}
	if _, err := os.Stat(path + backupSuffix); !os.IsNotExist(err) {
		t.Fatalf("failed migration must not leave a backup: %v", err)
	}
}

// Old migration stranded a key file next to a canonical ciphertext that still
// matches its legacy backup. Recovery removes the stray key only when the
// backup decrypts under the supplied admin token.
func TestStrandedLegacyMigrationRecoversOnOpen(t *testing.T) {
	dir, _, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+backupSuffix, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	strayKey := make([]byte, keyBytes)
	if _, err := rand.Read(strayKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(strayKey)), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, "legacy-token")
	if err != nil {
		t.Fatalf("Open stranded store: %v", err)
	}
	if value, ok, err := reopened.Resolve("ref/one"); err != nil || !ok || value != "legacy-secret" {
		t.Fatalf("recovered secret = %q, %t, %v", value, ok, err)
	}
	if _, err := os.Stat(path + backupSuffix); err != nil {
		t.Fatalf("backup must be preserved during recovery: %v", err)
	}
}

func TestStrandedLegacyMigrationRefusesWrongToken(t *testing.T) {
	dir, _, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+backupSuffix, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	strayKey := make([]byte, keyBytes)
	if _, err := rand.Read(strayKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(strayKey)), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Open(dir, "a-different-token")
	if !errors.Is(err, ErrAdminTokenRotated) {
		t.Fatalf("error should identify token rotation, got %v", err)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("refused recovery must not remove the stranded key file: %v", statErr)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, backup) {
		t.Fatal("refused recovery must not touch the canonical ciphertext")
	}
}

// A completed migration (canonical ciphertext differs from the backup) is not
// a stranded state and must open normally without deleting anything.
func TestStrandedLegacyMigrationSkipsCompletedState(t *testing.T) {
	dir, _, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})
	legacy, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+backupSuffix, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	strayKey := make([]byte, keyBytes)
	if _, err := rand.Read(strayKey); err != nil {
		t.Fatal(err)
	}
	completed := &Store{path: path, gcm: nil, secrets: map[string]string{}}
	gcm, err := newGCM(strayKey)
	if err != nil {
		t.Fatal(err)
	}
	completed.gcm = gcm
	writeEncryptedMap(t, path, completed, map[string]string{"ref/one": "legacy-secret"})
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(strayKey)), 0o600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, "legacy-token")
	if err != nil {
		t.Fatalf("Open completed store: %v", err)
	}
	if value, ok, err := reopened.Resolve("ref/one"); err != nil || !ok || value != "legacy-secret" {
		t.Fatalf("secret = %q, %t, %v", value, ok, err)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("completed migration key file must stay: %v", statErr)
	}
}

// A prepared legacy-migration journal rolls back only for the token that can
// decrypt the snapshot; with the wrong token the new state stays untouched and
// the journal waits.
func TestMigrationRollbackWaitsForOriginalToken(t *testing.T) {
	dir, _, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})

	// Fail the key commit: the ciphertext is published, the key file is not,
	// and a prepared legacy-migration journal is on disk.
	failRename, _ := renameCounter(t, keyFilename, 1)
	if err := migrateLegacyStore(path, keyPath, "legacy-token", migrationOps(t, failRename)); err == nil {
		t.Fatal("injected failure did not fail the migration")
	}
	published, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(published, mustRead(t, path+backupSuffix)) {
		t.Fatal("fixture expected the new ciphertext to be published")
	}

	if _, err := Open(dir, "a-different-token"); !errors.Is(err, ErrAdminTokenRotated) {
		t.Fatalf("wrong token must wait instead of rolling back, got %v", err)
	}
	afterRefusal, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRefusal, published) {
		t.Fatal("refused recovery must not overwrite the published ciphertext")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(path), rekeyJournal)); statErr != nil {
		t.Fatalf("journal must wait for the original token: %v", statErr)
	}

	reopened, err := Open(dir, "legacy-token")
	if err != nil {
		t.Fatalf("Open with the original token: %v", err)
	}
	if value, ok, err := reopened.Resolve("ref/one"); err != nil || !ok || value != "legacy-secret" {
		t.Fatalf("recovered secret = %q, %t, %v", value, ok, err)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(path), rekeyJournal)); !os.IsNotExist(statErr) {
		t.Fatalf("journal remains after recovery: %v", statErr)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Journals written before key_existed existed must keep restoring the key from
// its snapshot — the absent field defaults to true.
func TestRekeyJournalWithoutKeyExistedFieldRestoresSnapshot(t *testing.T) {
	_, secretDir, path, keyPath := lockStoreDirectory(t)
	oldCiphertext := []byte("old-ciphertext-bytes")
	oldKey := []byte(base64.StdEncoding.EncodeToString(make([]byte, keyBytes)))
	if err := os.WriteFile(path, oldCiphertext, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("corrupted-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshotCt := filepath.Join(secretDir, "ct-snapshot")
	snapshotKey := filepath.Join(secretDir, "key-snapshot")
	if err := os.WriteFile(snapshotCt, oldCiphertext, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshotKey, oldKey, 0o600); err != nil {
		t.Fatal(err)
	}
	// Deliberately written without key_existed: the old rekey format.
	journal, err := json.Marshal(map[string]any{
		"state": "prepared", "ciphertext_path": path, "ciphertext_existed": true,
		"key_path": keyPath, "snapshot_ciphertext": snapshotCt, "snapshot_key": snapshotKey,
		"staged_ciphertext": "", "staged_key": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, rekeyJournal), journal, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := recoverRekey(secretDir, "any-token"); err != nil {
		t.Fatalf("recoverRekey: %v", err)
	}
	if got := mustRead(t, keyPath); !bytes.Equal(got, oldKey) {
		t.Fatal("absent key_existed must default to restoring the key snapshot")
	}
	if got := mustRead(t, path); !bytes.Equal(got, oldCiphertext) {
		t.Fatal("ciphertext snapshot was not restored")
	}
	if _, err := os.Stat(snapshotCt); !os.IsNotExist(err) {
		t.Fatal("rekey ciphertext snapshot must be cleaned after recovery")
	}
	if _, err := os.Stat(snapshotKey); !os.IsNotExist(err) {
		t.Fatal("rekey key snapshot must be cleaned after recovery")
	}
}

// An explicit key_existed=false rolls the canonical key file back to absence —
// the legacy-migration shape.
func TestMigrationJournalRollbackRemovesKeyFile(t *testing.T) {
	_, secretDir, path, keyPath := lockStoreDirectory(t)
	writeLegacyStore(t, path, map[string]string{"ref/one": "legacy-secret"})
	legacy := mustRead(t, path)
	if err := os.WriteFile(path+backupSuffix, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	newKey := make([]byte, keyBytes)
	if _, err := rand.Read(newKey); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(newKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	keyExisted := false
	j := rekeyJournalData{
		State: "prepared", Kind: journalKindMigration,
		CiphertextPath: path, CiphertextExisted: true, KeyPath: keyPath, KeyExisted: &keyExisted,
		SnapshotCiphertext: path + backupSuffix, SnapshotKey: "",
		StagedCiphertext: filepath.Join(secretDir, "staged-ct"), StagedKey: filepath.Join(secretDir, "staged-key"),
	}
	if err := writeJournal(filepath.Join(secretDir, rekeyJournal), j, defaultFileOps()); err != nil {
		t.Fatal(err)
	}

	if err := recoverRekey(secretDir, "a-different-token"); !errors.Is(err, ErrAdminTokenRotated) {
		t.Fatalf("wrong token must refuse the rollback, got %v", err)
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatalf("refused rollback must keep the key file: %v", statErr)
	}

	if err := recoverRekey(secretDir, "legacy-token"); err != nil {
		t.Fatalf("recoverRekey: %v", err)
	}
	if _, statErr := os.Stat(keyPath); !os.IsNotExist(statErr) {
		t.Fatalf("rollback must remove the key file that did not exist before: %v", statErr)
	}
	if got := mustRead(t, path); !bytes.Equal(got, legacy) {
		t.Fatal("rollback must restore the legacy ciphertext")
	}
	if _, statErr := os.Stat(path + backupSuffix); statErr != nil {
		t.Fatalf("backup must be preserved during rollback: %v", statErr)
	}
}

func TestStoreTwoHandlesSharePersistenceAndSurviveRekey(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}

	if err := first.Put("provider/one", "value-one"); err != nil {
		t.Fatal(err)
	}
	if err := second.Put("provider/two", "value-two"); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := first.Resolve("provider/two"); err != nil || !ok || value != "value-two" {
		t.Fatalf("first handle after second Put: %q, %t, %v", value, ok, err)
	}
	if value, ok, err := second.Resolve("provider/one"); err != nil || !ok || value != "value-one" {
		t.Fatalf("second handle after first Put: %q, %t, %v", value, ok, err)
	}

	if err := first.Rekey(); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := second.Resolve("provider/one"); err != nil || !ok || value != "value-one" {
		t.Fatalf("second handle after rekey: %q, %t, %v", value, ok, err)
	}
	if err := second.Put("provider/three", "value-three"); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := first.Resolve("provider/three"); err != nil || !ok || value != "value-three" {
		t.Fatalf("first handle after second-handle Put: %q, %t, %v", value, ok, err)
	}
}

func TestStoreConcurrentDistinctPutsRetainBothRecords(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}

	const perHandle = 16
	var wg sync.WaitGroup
	for i := range perHandle {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := first.Put("provider/first-"+itoa(i), "first-value-"+itoa(i)); err != nil {
				t.Errorf("first handle Put %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := second.Put("provider/second-"+itoa(i), "second-value-"+itoa(i)); err != nil {
				t.Errorf("second handle Put %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	for i := range perHandle {
		want := "first-value-" + itoa(i)
		if got, ok, err := first.Resolve("provider/first-" + itoa(i)); err != nil || !ok || got != want {
			t.Fatalf("first handle lost its own record %d: %q, %t, %v", i, got, ok, err)
		}
		if got, ok, err := second.Resolve("provider/first-" + itoa(i)); err != nil || !ok || got != want {
			t.Fatalf("second handle lost the first handle's record %d: %q, %t, %v", i, got, ok, err)
		}
		want = "second-value-" + itoa(i)
		if got, ok, err := second.Resolve("provider/second-" + itoa(i)); err != nil || !ok || got != want {
			t.Fatalf("second handle lost its own record %d: %q, %t, %v", i, got, ok, err)
		}
		if got, ok, err := first.Resolve("provider/second-" + itoa(i)); err != nil || !ok || got != want {
			t.Fatalf("first handle lost the second handle's record %d: %q, %t, %v", i, got, ok, err)
		}
	}
}

// The cross-handle lock file lives inside the already-private store directory
// and inherits its protection; the directory permissions never widen.
func TestStoreLockFileStaysPrivateInsideStoreDirectory(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("provider/one", "value-one"); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(dir, "provider-secrets")
	lockPath := filepath.Join(secretPath, lockFilename)
	info, err := os.Lstat(lockPath)
	if err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
	if err := validateSecretFileInfo(lockPath, info); err != nil {
		t.Fatal(err)
	}
	secretDir, err := os.Lstat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSecretDirectory(secretPath, secretDir); err != nil {
		t.Fatal(err)
	}
}

// Process-local stores have no lock path and no persistent behavior.
func TestStoreProcessLocalHasNoLockPath(t *testing.T) {
	store, err := Open("", "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("provider/local", "value-local"); err != nil {
		t.Fatal(err)
	}
	if value, ok, err := store.Resolve("provider/local"); err != nil || !ok || value != "value-local" {
		t.Fatalf("process-local Resolve: %q, %t, %v", value, ok, err)
	}
	if err := store.withPathLock(func() error { return nil }); err != nil {
		t.Fatalf("process-local withPathLock: %v", err)
	}
	if err := store.Rekey(); err == nil || !strings.Contains(err.Error(), "no persistent key") {
		t.Fatalf("process-local Rekey = %v, want the no-persistent-key error", err)
	}
}

// A failed rekey leaves a prepared journal and a half-published canonical
// state. The next persistent operation on any handle — not only a fresh Open —
// recovers the journal under the path lock before touching the files.
func TestRuntimeOperationRecoversInterruptedRekey(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Put("provider/one", "value-one"); err != nil {
		t.Fatal(err)
	}

	first.ops = failingRekeyOps(t, func(_, destination string) bool { return filepath.Base(destination) == filename }, func(string) bool { return false })
	if err := first.Rekey(); err == nil {
		t.Fatal("injected failure did not fail the rekey")
	}
	if _, err := os.Stat(filepath.Join(dir, "provider-secrets", rekeyJournal)); err != nil {
		t.Fatalf("journal missing after failed rekey: %v", err)
	}

	// The second handle's Put runs recovery first: the rolled-back state is
	// consistent, the put succeeds and both records survive.
	if err := second.Put("provider/two", "value-two"); err != nil {
		t.Fatalf("Put after interrupted rekey: %v", err)
	}
	for ref, want := range map[string]string{"provider/one": "value-one", "provider/two": "value-two"} {
		if got, ok, err := second.Resolve(ref); err != nil || !ok || got != want {
			t.Fatalf("Resolve(%q) after runtime recovery = %q, %t, %v", ref, got, ok, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "provider-secrets", rekeyJournal)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after runtime recovery: %v", err)
	}
}
