package secretstore

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func rekeyFixture(t *testing.T) (*Store, string, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("provider-channel/c1/m1", "sk-rekey"); err != nil {
		t.Fatal(err)
	}
	secretDir := filepath.Join(dir, "provider-secrets")
	ciphertext, err := os.ReadFile(filepath.Join(secretDir, filename))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(secretDir, keyFilename))
	if err != nil {
		t.Fatal(err)
	}
	return store, dir, ciphertext, key
}

func failingRekeyOps(t *testing.T, fail func(string, string) bool, syncFail func(string) bool) fileOps {
	t.Helper()
	ops := defaultFileOps()
	ops.rename = func(source, destination string) error {
		if err := os.Rename(source, destination); err != nil {
			return err
		}
		if fail(source, destination) {
			return errors.New("injected rename failure")
		}
		return nil
	}
	ops.syncDirectory = func(dir string) error {
		if err := syncDirectory(dir); err != nil {
			return err
		}
		if syncFail(dir) {
			return errors.New("injected directory sync failure")
		}
		return nil
	}
	return ops
}

func assertCanonicalBytes(t *testing.T, dir string, wantCiphertext, wantKey []byte) {
	t.Helper()
	secretDir := filepath.Join(dir, "provider-secrets")
	got, err := os.ReadFile(filepath.Join(secretDir, filename))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(wantCiphertext) {
		t.Fatal("canonical ciphertext changed")
	}
	got, err = os.ReadFile(filepath.Join(secretDir, keyFilename))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(wantKey) {
		t.Fatal("canonical key changed")
	}
}

func TestRekeyFailureAfterCiphertextRenameRecoversOnOpen(t *testing.T) {
	store, dir, oldCiphertext, oldKey := rekeyFixture(t)
	store.ops = failingRekeyOps(t, func(_, destination string) bool { return filepath.Base(destination) == filename }, func(string) bool { return false })
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "provider-secrets", rekeyJournal)); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	reopened, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, dir, oldCiphertext, oldKey)
	if _, ok, err := reopened.Resolve("provider-channel/c1/m1"); err != nil || !ok {
		t.Fatalf("recovered secret: %v, %t", err, ok)
	}
}

func TestRekeyFailureAfterCiphertextDirectorySyncRecoversOnOpen(t *testing.T) {
	store, dir, oldCiphertext, oldKey := rekeyFixture(t)
	var syncs int
	store.ops = failingRekeyOps(t, func(_, _ string) bool { return false }, func(_ string) bool {
		syncs++
		return syncs == 5
	})
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, dir, oldCiphertext, oldKey)
}

func TestRekeyFailureAfterKeyRenameRecoversOnOpen(t *testing.T) {
	store, dir, oldCiphertext, oldKey := rekeyFixture(t)
	store.ops = failingRekeyOps(t, func(_, destination string) bool { return filepath.Base(destination) == keyFilename }, func(string) bool { return false })
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, dir, oldCiphertext, oldKey)
}

func TestRekeyFailureAfterKeyDirectorySyncRecoversOnOpen(t *testing.T) {
	store, dir, oldCiphertext, oldKey := rekeyFixture(t)
	var syncs int
	store.ops = failingRekeyOps(t, func(_, _ string) bool { return false }, func(_ string) bool {
		syncs++
		return syncs == 6
	})
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, dir, oldCiphertext, oldKey)
}

func TestRekeyCommitMarkerFailureRecoversOnOpen(t *testing.T) {
	store, dir, _, _ := rekeyFixture(t)
	var journalWrites int
	store.ops = failingRekeyOps(t, func(_, destination string) bool {
		if filepath.Base(destination) == rekeyJournal {
			journalWrites++
			return journalWrites == 2
		}
		return false
	}, func(string) bool { return false })
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "provider-secrets", rekeyJournal)); !os.IsNotExist(err) {
		t.Fatal("journal was not cleaned after rollback")
	}
}

func TestRekeyRollbackFailureLeavesJournalForReopen(t *testing.T) {
	store, dir, oldCiphertext, oldKey := rekeyFixture(t)
	store.ops = failingRekeyOps(t, func(_, destination string) bool { return filepath.Base(destination) == keyFilename }, func(string) bool { return false })
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	// The failed transaction leaves the prepared journal; Open must restore it
	// using production operations, independently of the failed Store hooks.
	reopened, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, dir, oldCiphertext, oldKey)
	if _, ok, err := reopened.Resolve("provider-channel/c1/m1"); err != nil || !ok {
		t.Fatalf("recovered secret: %v, %t", err, ok)
	}
}

func TestRekeyBackupWriteFailureLeavesCanonicalFilesUntouched(t *testing.T) {
	store, dir, oldCiphertext, oldKey := rekeyFixture(t)
	store.ops = failingRekeyOps(t, func(_, destination string) bool { return filepath.Base(destination) == "store.key.pre-rekey" }, func(string) bool { return false })
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	assertCanonicalBytes(t, dir, oldCiphertext, oldKey)
}

func TestRekeySuccessCleansJournalAndPreservesPermissions(t *testing.T) {
	store, dir, _, _ := rekeyFixture(t)
	if err := store.Rekey(); err != nil {
		t.Fatal(err)
	}
	secretDir := filepath.Join(dir, "provider-secrets")
	if _, err := os.Stat(filepath.Join(secretDir, rekeyJournal)); !os.IsNotExist(err) {
		t.Fatal("journal remains")
	}
	for _, name := range []string{filename, keyFilename, keyFilename + ".pre-rekey"} {
		info, err := os.Stat(filepath.Join(secretDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != secretFileMode {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
}

func TestRekeyEmptyStoreRollbackDoesNotCreateCiphertext(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(dir, "provider-secrets", filename)
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("empty store ciphertext stat = %v, want not exist", err)
	}
	store.ops = failingRekeyOps(t, func(_, destination string) bool { return filepath.Base(destination) == filename }, func(string) bool { return false })
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey succeeded")
	}
	if _, err := Open(dir, "hub-admin-token-for-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secretPath); !os.IsNotExist(err) {
		t.Fatalf("recovered empty store ciphertext stat = %v, want not exist", err)
	}
}

func TestRekeyRotatesKeyAndPreservesSecrets(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"provider-channel/c1/m1": "sk-one",
		"provider-channel/c2/m1": "sk-two",
		"provider-channel/c3/m1": "sk-three",
	}
	for ref, val := range secrets {
		if err := store.Put(ref, val); err != nil {
			t.Fatalf("Put(%q): %v", ref, err)
		}
	}

	// Read old key before rekey.
	keyPath := filepath.Join(dir, "provider-secrets", "store.key")
	oldKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Rekey(); err != nil {
		t.Fatalf("Rekey() = %v", err)
	}

	// All secrets still readable through the same store.
	for ref, want := range secrets {
		got, ok, err := store.Resolve(ref)
		if err != nil || !ok || got != want {
			t.Fatalf("Resolve(%q) after Rekey = %q, %t, %v; want %q", ref, got, ok, err, want)
		}
	}

	// Key file content changed.
	newKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(oldKey) == string(newKey) {
		t.Fatal("store.key content did not change after Rekey")
	}

	// Pre-rekey backup exists and contains the old key.
	preRekeyPath := filepath.Join(dir, "provider-secrets", "store.key.pre-rekey")
	backupKey, err := os.ReadFile(preRekeyPath)
	if err != nil {
		t.Fatalf("read store.key.pre-rekey: %v", err)
	}
	if string(backupKey) != string(oldKey) {
		t.Fatal("store.key.pre-rekey does not contain the old key")
	}
}

func TestRekeySurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	// First session: put secrets, rekey.
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{
		"provider-channel/c1/m1": "sk-reopen-1",
		"provider-channel/c2/m1": "sk-reopen-2",
	}
	for ref, val := range secrets {
		if err := store.Put(ref, val); err != nil {
			t.Fatalf("Put(%q): %v", ref, err)
		}
	}
	if err := store.Rekey(); err != nil {
		t.Fatalf("Rekey() = %v", err)
	}

	// Second session: reopen and verify all secrets.
	reopened, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatalf("Open after Rekey: %v", err)
	}
	for ref, want := range secrets {
		got, ok, err := reopened.Resolve(ref)
		if err != nil || !ok || got != want {
			t.Fatalf("Resolve(%q) after reopen = %q, %t, %v; want %q", ref, got, ok, err, want)
		}
	}
}

func TestRekeyConcurrent(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	// Pre-populate.
	for i := 0; i < 20; i++ {
		ref := secretRef(t, "concurrent", i)
		if err := store.Put(ref, secretValue(t, i)); err != nil {
			t.Fatalf("Put(%q): %v", ref, err)
		}
	}

	const readers = 12
	var wg sync.WaitGroup

	// Start concurrent readers.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				ref := secretRef(t, "concurrent", i)
				want := secretValue(t, i)
				got, ok, err := store.Resolve(ref)
				if err != nil {
					t.Errorf("Resolve(%q) error during concurrent Rekey: %v", ref, err)
					return
				}
				if !ok {
					t.Errorf("Resolve(%q) not found during concurrent Rekey", ref)
					return
				}
				if got != want {
					t.Errorf("Resolve(%q) = %q during concurrent Rekey, want %q", ref, got, want)
					return
				}
			}
		}()
	}

	// Rekey in this goroutine while readers are running.
	if err := store.Rekey(); err != nil {
		t.Fatalf("concurrent Rekey() = %v", err)
	}

	wg.Wait()

	// All secrets still intact after the storm.
	for i := 0; i < 20; i++ {
		ref := secretRef(t, "concurrent", i)
		want := secretValue(t, i)
		got, ok, err := store.Resolve(ref)
		if err != nil || !ok || got != want {
			t.Fatalf("post-concurrent Resolve(%q) = %q, %t, %v; want %q", ref, got, ok, err, want)
		}
	}
}

func TestRekeyEmptyStore(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	// No secrets — Rekey should still succeed.
	if err := store.Rekey(); err != nil {
		t.Fatalf("Rekey() on empty store: %v", err)
	}
	// Verify we can still write after rekey.
	if err := store.Put("provider-channel/c1/m1", "post-rekey"); err != nil {
		t.Fatalf("Put after empty Rekey: %v", err)
	}
	got, ok, err := store.Resolve("provider-channel/c1/m1")
	if err != nil || !ok || got != "post-rekey" {
		t.Fatalf("Resolve after empty Rekey: %q, %t, %v", got, ok, err)
	}
}

func TestRekeyRefusedForProcessLocal(t *testing.T) {
	store, err := Open("", "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rekey(); err == nil {
		t.Fatal("Rekey() should fail for process-local store")
	}
}

func TestRekeyPreservesFilePermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, "hub-admin-token-for-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("provider-channel/c1/m1", "sk-perm"); err != nil {
		t.Fatal(err)
	}
	if err := store.Rekey(); err != nil {
		t.Fatalf("Rekey() = %v", err)
	}

	secretDir := filepath.Join(dir, "provider-secrets")
	dirInfo, err := os.Stat(secretDir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("provider-secrets/ mode = %o, want 700", dirInfo.Mode().Perm())
	}

	for _, name := range []string{"store.key", "store.key.pre-rekey", "provider-secrets.enc"} {
		info, err := os.Stat(filepath.Join(secretDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
}

func secretRef(t *testing.T, prefix string, i int) string {
	t.Helper()
	return "provider-channel/" + prefix + "/m" + itoa(i)
}

func secretValue(t *testing.T, i int) string {
	t.Helper()
	return "sk-concurrent-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := make([]byte, 0, 10)
	for n := i; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}
