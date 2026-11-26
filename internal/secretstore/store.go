// Package secretstore keeps Hub-only provider keys out of SQLite, configuration
// files, browser state, and Worker configuration. Its encrypted file is a
// local-at-rest protection boundary, not a substitute for OS account security.
package secretstore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
)

const (
	filename       = "provider-secrets.enc"
	keyFilename    = "store.key"
	backupSuffix   = ".pre-key-migration"
	dataDirMode    = os.FileMode(0o700)
	secretFileMode = os.FileMode(0o600)
	maxRefLength   = 512
	keyBytes       = 32
	rekeyJournal   = "rekey.journal"
	// lockFilename is the cross-handle lock inside the private store
	// directory. Every persistent store operation — including reads, so
	// recovery is always serialized — holds it exclusively after the
	// per-instance mutex; process-local stores have no lock path at all.
	lockFilename = ".secret-store.lock"
)

// Journal kinds. An absent Kind means the original rekey protocol.
const (
	journalKindRekey     = "rekey"
	journalKindMigration = "legacy-migration"
)

// ErrAdminTokenRotated reports a store that predates the independent key file
// and can no longer be opened with the administrator token currently in use.
var ErrAdminTokenRotated = errors.New("provider secret store cannot be decrypted with the current Hub administrator token")

type encryptedFile struct {
	Version    int    `json:"version"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

type Store struct {
	mu      sync.RWMutex
	path    string
	gcm     cipher.AEAD
	secrets map[string]string
	ops     fileOps
	// adminToken is held only in memory and only so a persistent operation
	// can validate a legacy-migration rollback before replaying it. It is
	// never serialized, logged or included in errors.
	adminToken string
}

type fileOps struct {
	createTemp    func(string) (*os.File, error)
	rename        func(string, string) error
	syncDirectory func(string) error
	remove        func(string) error
}

type rekeyJournalData struct {
	State string `json:"state"`
	// Kind distinguishes the original rekey protocol from the legacy
	// token-migration protocol that reuses it. Empty means rekey.
	Kind string `json:"kind,omitempty"`
	// CiphertextPath and KeyPath are the canonical filenames the journal
	// protects.
	CiphertextPath    string `json:"ciphertext_path"`
	CiphertextExisted bool   `json:"ciphertext_existed"`
	KeyPath           string `json:"key_path"`
	// KeyExisted records whether the canonical key file existed before the
	// operation. The field is optional: a journal written before it existed
	// has no field, and the rekey protocol — whose only caller of the time
	// always had a key snapshot — must then behave as before, i.e. as if the
	// key existed and must be restored from its snapshot.
	KeyExisted         *bool  `json:"key_existed,omitempty"`
	SnapshotCiphertext string `json:"snapshot_ciphertext"`
	SnapshotKey        string `json:"snapshot_key"`
	StagedCiphertext   string `json:"staged_ciphertext"`
	StagedKey          string `json:"staged_key"`
}

func (j *rekeyJournalData) isLegacyMigration() bool {
	return j.Kind == journalKindMigration
}

// keyExistedDefaults applies the legacy default for journals that predate the
// field: the key existed and its snapshot restores it.
func (j *rekeyJournalData) keyExistedDefaults() bool {
	return j.KeyExisted == nil || *j.KeyExisted
}

func defaultFileOps() fileOps {
	return fileOps{createTemp: func(dir string) (*os.File, error) { return os.CreateTemp(dir, ".provider-secrets-*") }, rename: os.Rename, syncDirectory: syncDirectory, remove: os.Remove}
}

// Open opens the Hub-only provider secret store. When dataDir is empty, the
// store remains process-local for compatibility with in-memory callers.
//
// The data-encryption key lives in its own file rather than being derived from
// the Hub administrator token. Deriving it from the token made every token
// rotation — the standard response to a suspected disclosure — unrecoverably
// lock the store, and because Service construction opens it, that bricked every
// CLI command including doctor. hubAdminToken is now used only to migrate a
// pre-existing store written by the old scheme.
//
// Being honest about what this buys: store.key sits next to the ciphertext with
// mode 0600, exactly like admin-token does. It is not a key-management system,
// and it does not defend against an attacker who can already read the Hub data
// directory. The protection boundary is the OS account, as it was before; what
// changed is that routine key rotation no longer destroys the store.
func Open(dataDir, hubAdminToken string) (*Store, error) {
	if strings.TrimSpace(hubAdminToken) == "" {
		return nil, fmt.Errorf("secret store requires Hub administrator token")
	}

	// Process-local mode keeps the legacy token-derived key: there is no file
	// to persist an independent key into, and nothing survives the process.
	if strings.TrimSpace(dataDir) == "" {
		gcm, err := newGCM(legacyKey(hubAdminToken))
		if err != nil {
			return nil, err
		}
		return &Store{path: "", gcm: gcm, secrets: map[string]string{}, ops: defaultFileOps()}, nil
	}

	// The Hub data directory also contains SQLite, derived media and other
	// NAS-owned files. It is deliberately allowed to be group-readable in
	// common deployments. Only the encrypted secret-store subdirectory is
	// a private OS boundary.
	secretDir := filepath.Join(dataDir, "provider-secrets")
	if err := ensurePrivateDirectory(secretDir); err != nil {
		return nil, fmt.Errorf("open secret store directory: %w", err)
	}
	path := filepath.Join(secretDir, filename)
	keyPath := filepath.Join(secretDir, keyFilename)

	// Cross-handle coordination: recovery, migration and the initial load all
	// read and possibly rewrite the same on-disk files, so they run under the
	// store path lock no matter which process or handle wins it.
	lock, err := lockStorePath(secretDir)
	if err != nil {
		return nil, err
	}
	defer lock.Release()

	if err := recoverRekey(secretDir, hubAdminToken); err != nil {
		return nil, fmt.Errorf("recover secret store rekey: %w", err)
	}
	if err := migrateLegacyStore(path, keyPath, hubAdminToken, defaultFileOps()); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	s := &Store{path: path, gcm: gcm, secrets: map[string]string{}, ops: defaultFileOps(), adminToken: hubAdminToken}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// legacyKey reproduces the pre-migration derivation so an existing store can
// still be read once, at migration time.
func legacyKey(hubAdminToken string) []byte {
	key := sha256.Sum256([]byte("nexusgate/provider-secrets/v1\x00" + hubAdminToken))
	return key[:]
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create secret cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create secret cipher mode: %w", err)
	}
	return gcm, nil
}

// lockStorePath acquires the exclusive cross-handle lock for one store
// directory. The lock file lives inside the already-private store directory
// and is created with 0600, so it never widens the directory's exposure.
func lockStorePath(secretDir string) (*cachecoord.Lock, error) {
	lock, err := cachecoord.AcquirePath(filepath.Join(secretDir, lockFilename), true)
	if err != nil {
		return nil, fmt.Errorf("acquire secret store lock: %w", err)
	}
	return lock, nil
}

// withPathLock runs fn while holding the store path lock, after the caller
// already holds the per-instance mutex. Before fn runs, any journal left by an
// interrupted persistent operation is recovered and the current on-disk key is
// reloaded with the cipher rebuilt, so a handle never decrypts with a key
// another handle has already rotated or migrated. Process-local stores have no
// lock path and simply run fn. Persistent methods must call this exactly once
// per operation — the internal *Locked helpers never re-acquire.
func (s *Store) withPathLock(fn func() error) error {
	if s.path == "" {
		return fn()
	}
	lock, err := lockStorePath(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := recoverRekey(filepath.Dir(s.path), s.adminToken); err != nil {
		return fmt.Errorf("recover secret store journal: %w", err)
	}
	if err := s.refreshKeyLocked(); err != nil {
		return err
	}
	return fn()
}

// refreshKeyLocked re-reads the on-disk data-encryption key and rebuilds the
// cipher. Callers hold the store mutex and the path lock.
func (s *Store) refreshKeyLocked() error {
	if s.path == "" {
		return nil
	}
	key, err := loadOrCreateKey(s.keyPath())
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	s.gcm = gcm
	return nil
}

// migrateLegacyStore upgrades a store written before store.key existed. It is
// a no-op once the key file is present, or when there is no ciphertext to carry
// over. Nothing is published until the legacy payload has decrypted
// successfully, so a failed migration leaves the original file untouched.
//
// The migration reuses the prepared/committed rekey journal: the legacy
// ciphertext and key presence are snapshotted, both replacement files are
// staged, a prepared record is written, the new ciphertext and key file are
// published in order, and a committed record closes the transaction. Recovery
// rolls both filenames back to the snapshot, preserving the legacy backup —
// which stays on disk as the operator's copy of the pre-migration store either
// way.
//
// Stores stranded by the old unjournaled migration are recognized first: a key
// file next to a canonical ciphertext that still matches the legacy backup.
// That state is recoverable only when the backup decrypts under the supplied
// admin token, which proves the store belongs to this deployment; the stray
// key file is then removed and migration reruns normally.
func migrateLegacyStore(path, keyPath, hubAdminToken string, ops fileOps) error {
	if ops.createTemp == nil {
		ops = defaultFileOps()
	}
	backupPath := path + backupSuffix
	secretDir := filepath.Dir(path)

	if err := recoverStrandedMigration(path, keyPath, backupPath, hubAdminToken); err != nil {
		return err
	}
	if _, err := os.Lstat(keyPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat secret store key: %w", err)
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat secret store: %w", err)
	}

	legacyGCM, err := newGCM(legacyKey(hubAdminToken))
	if err != nil {
		return err
	}
	legacy := &Store{path: path, gcm: legacyGCM, secrets: map[string]string{}}
	if err := legacy.reloadLocked(); err != nil {
		// The operator has a real choice here, so name both halves of it. The
		// error deliberately carries no token or secret material.
		return fmt.Errorf("%w: start the Hub once with the original token to migrate the store automatically, or delete %s to discard the stored Provider keys and configure them again",
			ErrAdminTokenRotated, filepath.Dir(path))
	}

	// Snapshot the legacy ciphertext as the operator-facing backup. An existing
	// backup from an earlier attempt is left untouched: it already holds the
	// same legacy payload and recovery must be able to trust it.
	if _, err := os.Lstat(backupPath); os.IsNotExist(err) {
		if err := copyPrivateFile(path, backupPath, ops); err != nil {
			return fmt.Errorf("back up secret store before migration: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat secret store backup: %w", err)
	}

	// Stage both replacement files before any canonical filename changes.
	newKeyBytes := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, newKeyBytes); err != nil {
		return fmt.Errorf("generate migration key: %w", err)
	}
	migratedGCM, err := newGCM(newKeyBytes)
	if err != nil {
		return err
	}
	newCiphertext, err := encryptedBytes(legacy.secrets, migratedGCM)
	if err != nil {
		return fmt.Errorf("migration: encrypt new ciphertext: %w", err)
	}
	cipherTemp, err := writePrivateTemp(ops, path, newCiphertext)
	if err != nil {
		return fmt.Errorf("migration: stage new ciphertext: %w", err)
	}
	keyTemp, err := writePrivateTemp(ops, keyPath, []byte(base64.StdEncoding.EncodeToString(newKeyBytes)))
	if err != nil {
		_ = ops.remove(cipherTemp)
		return fmt.Errorf("migration: stage new key: %w", err)
	}

	// The legacy store predates store.key, so the canonical key file never
	// existed and rollback deletes it; the ciphertext existed and its snapshot
	// is the operator backup.
	keyExisted := false
	j := rekeyJournalData{
		State: "prepared", Kind: journalKindMigration,
		CiphertextPath: path, CiphertextExisted: true, KeyPath: keyPath, KeyExisted: &keyExisted,
		SnapshotCiphertext: backupPath, SnapshotKey: "",
		StagedCiphertext: cipherTemp, StagedKey: keyTemp,
	}
	journalPath := filepath.Join(secretDir, rekeyJournal)
	if err := writeJournal(journalPath, j, ops); err != nil {
		// No canonical file has been renamed yet. Remove the journal before the
		// staged files so a post-rename journal-write failure cannot leave Open
		// with an unrecoverable prepared record.
		if removeErr := ops.remove(journalPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("migration: write journal: %w; preserve recovery journal: %v", err, removeErr)
		}
		_ = ops.remove(cipherTemp)
		_ = ops.remove(keyTemp)
		return fmt.Errorf("migration: write journal: %w", err)
	}

	if err := renameAndSync(ops, cipherTemp, path); err != nil {
		return fmt.Errorf("migration: commit ciphertext: %w", err)
	}
	if err := renameAndSync(ops, keyTemp, keyPath); err != nil {
		return fmt.Errorf("migration: commit key: %w", err)
	}
	j.State = "committed"
	if err := writeJournal(journalPath, j, ops); err != nil {
		return fmt.Errorf("migration: write commit marker: %w", err)
	}
	// The backup is deliberately preserved after a successful migration.
	_ = ops.remove(journalPath)
	return nil
}

// recoverStrandedMigration removes a key file stranded by the old unjournaled
// migration. The old code created the new key file before publishing the new
// ciphertext; a crash between the two left a store whose canonical ciphertext
// is still the legacy bytes and whose key file belongs to the aborted attempt.
// That state is identified by a canonical ciphertext byte-identical to the
// legacy backup, and is recoverable only when the backup decrypts under the
// supplied admin token — anything else is left exactly as found.
func recoverStrandedMigration(path, keyPath, backupPath, hubAdminToken string) error {
	if _, err := os.Lstat(keyPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat secret store key: %w", err)
	}
	backup, err := os.ReadFile(backupPath)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read secret store backup: %w", err)
	}
	canonical, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read secret store: %w", err)
	}
	if !bytes.Equal(canonical, backup) {
		// The canonical ciphertext moved on from the legacy payload — a
		// completed migration or an unrelated configuration. Nothing to undo.
		return nil
	}
	legacyGCM, err := newGCM(legacyKey(hubAdminToken))
	if err != nil {
		return err
	}
	if err := legacyDecrypts(backup, legacyGCM); err != nil {
		return fmt.Errorf("%w: start the Hub once with the original token to migrate the store automatically, or delete %s to discard the stored Provider keys and configure them again",
			ErrAdminTokenRotated, filepath.Dir(path))
	}
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stranded secret store key: %w", err)
	}
	return nil
}

// legacyDecrypts reports whether one encrypted ciphertext file decrypts under
// the legacy derivation.
func legacyDecrypts(raw []byte, gcm cipher.AEAD) error {
	var file encryptedFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	if file.Version != 1 || len(file.Nonce) != gcm.NonceSize() || len(file.Ciphertext) <= gcm.Overhead() {
		return errors.New("invalid encrypted payload")
	}
	if _, err := gcm.Open(nil, file.Nonce, file.Ciphertext, nil); err != nil {
		return errors.New("decrypt payload")
	}
	return nil
}

// loadOrCreateKey returns the store's data-encryption key, creating it on first
// use. The key file is held to the same 0600/regular-file rules as the
// ciphertext it protects.
func loadOrCreateKey(keyPath string) ([]byte, error) {
	info, err := os.Lstat(keyPath)
	if err == nil {
		if err := validateSecretFileInfo(info); err != nil {
			return nil, fmt.Errorf("read secret store key: %w", err)
		}
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("read secret store key: %w", err)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != keyBytes {
			return nil, fmt.Errorf("read secret store key: key file is malformed")
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat secret store key: %w", err)
	}

	key := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate secret store key: %w", err)
	}
	if err := writePrivateFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)), defaultFileOps()); err != nil {
		return nil, fmt.Errorf("write secret store key: %w", err)
	}
	return key, nil
}

func copyPrivateFile(source, destination string, ops fileOps) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writePrivateFile(destination, raw, ops)
}

// Rekey generates a new data-encryption key, re-encrypts every stored secret
// with it, atomically persists the new ciphertext and key file, and backs up
// the previous key to store.key.pre-rekey. On failure the original files and
// in-memory state are restored.
//
// Rekey is exclusive: it reloads from disk and rewrites the store while holding
// the write lock and the cross-handle path lock for its entire duration, so no
// other handle can observe an inconsistent state. A store opened in
// process-local mode (dataDir empty) has no persistent key to rotate and
// returns an error.
func (s *Store) Rekey() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return fmt.Errorf("secret store has no persistent key to rotate")
	}
	return s.withPathLock(func() error {
		if err := s.reloadLocked(); err != nil {
			return err
		}
		return s.rekeyLocked()
	})
}

func (s *Store) rekeyLocked() error {
	keyPath := s.keyPath()
	secretDir := filepath.Dir(s.path)
	preRekeyPath := keyPath + ".pre-rekey"
	journalPath := filepath.Join(secretDir, rekeyJournal)
	oldCiphertext, err := os.ReadFile(s.path)
	ciphertextExisted := err == nil
	if os.IsNotExist(err) {
		oldCiphertext = nil
	} else if err != nil {
		return fmt.Errorf("read current ciphertext for rekey: %w", err)
	}
	oldKey, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read current key for rekey: %w", err)
	}
	oldKeySnapshot := journalPath + ".old-key"
	oldCipherSnapshot := journalPath + ".old-ciphertext"
	cleanup := func() { _ = s.ops.remove(oldKeySnapshot); _ = s.ops.remove(oldCipherSnapshot) }
	if err := writePrivateFile(oldKeySnapshot, oldKey, s.ops); err != nil {
		cleanup()
		return fmt.Errorf("back up current key for rekey: %w", err)
	}
	if err := writePrivateFile(oldCipherSnapshot, oldCiphertext, s.ops); err != nil {
		cleanup()
		return fmt.Errorf("back up current ciphertext for rekey: %w", err)
	}
	if err := writePrivateFile(preRekeyPath, oldKey, s.ops); err != nil {
		cleanup()
		return fmt.Errorf("back up key before rekey: %w", err)
	}

	// Generate new key material.
	newKeyBytes := make([]byte, keyBytes)
	if _, err := io.ReadFull(rand.Reader, newKeyBytes); err != nil {
		return fmt.Errorf("generate rekey key: %w", err)
	}
	newGCM, err := newGCM(newKeyBytes)
	if err != nil {
		return err
	}

	newCiphertext, err := encryptedBytes(s.secrets, newGCM)
	if err != nil {
		cleanup()
		return fmt.Errorf("rekey: encrypt new ciphertext: %w", err)
	}
	cipherTemp, err := writePrivateTemp(s.ops, s.path, newCiphertext)
	if err != nil {
		cleanup()
		return fmt.Errorf("rekey: stage new ciphertext: %w", err)
	}
	keyTemp, err := writePrivateTemp(s.ops, keyPath, []byte(base64.StdEncoding.EncodeToString(newKeyBytes)))
	if err != nil {
		_ = s.ops.remove(cipherTemp)
		cleanup()
		return fmt.Errorf("rekey: stage new key: %w", err)
	}
	keyExisted := true
	j := rekeyJournalData{State: "prepared", Kind: journalKindRekey, CiphertextPath: s.path, CiphertextExisted: ciphertextExisted, KeyPath: keyPath, KeyExisted: &keyExisted, SnapshotCiphertext: oldCipherSnapshot, SnapshotKey: oldKeySnapshot, StagedCiphertext: cipherTemp, StagedKey: keyTemp}
	if err := writeJournal(journalPath, j, s.ops); err != nil {
		// No canonical file has been renamed yet. Remove the journal before its
		// snapshots so a post-rename journal-write failure cannot leave Open
		// with an unrecoverable prepared record.
		if removeErr := s.ops.remove(journalPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("rekey: write journal: %w; preserve recovery journal: %v", err, removeErr)
		}
		_ = s.ops.remove(cipherTemp)
		_ = s.ops.remove(keyTemp)
		cleanup()
		return fmt.Errorf("rekey: write journal: %w", err)
	}
	if err := renameAndSync(s.ops, cipherTemp, s.path); err != nil {
		return fmt.Errorf("rekey: commit ciphertext: %w", err)
	}
	if err := renameAndSync(s.ops, keyTemp, keyPath); err != nil {
		return fmt.Errorf("rekey: commit key: %w", err)
	}
	j.State = "committed"
	if err := writeJournal(journalPath, j, s.ops); err != nil {
		return fmt.Errorf("rekey: write commit marker: %w", err)
	}
	_ = s.ops.remove(oldKeySnapshot)
	_ = s.ops.remove(oldCipherSnapshot)
	_ = s.ops.remove(journalPath)
	s.gcm = newGCM
	return nil
}

// keyPath returns the path to the data-encryption key file derived from the
// ciphertext path set at Open time.
func (s *Store) keyPath() string {
	return filepath.Join(filepath.Dir(s.path), keyFilename)
}

// Get resolves a reference. It is retained for the service-facing API used by
// app.Service; new callers may use Resolve for the same behavior.
func (s *Store) Get(ref string) (string, bool, error) {
	return s.Resolve(ref)
}

// Resolve returns the value for ref, whether it exists, and any store error.
// Persistent stores reload from disk under the path lock, so a value is never
// served while another handle is mid-write or mid-recovery on the same files.
func (s *Store) Resolve(ref string) (string, bool, error) {
	if err := validateRef(ref); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var value string
	var ok bool
	err := s.withPathLock(func() error {
		if err := s.reloadLocked(); err != nil {
			return err
		}
		value, ok = s.secrets[ref]
		return nil
	})
	return value, ok, err
}

// Has reports whether ref is present without exposing its value.
func (s *Store) Has(_ context.Context, ref string) (bool, error) {
	if err := validateRef(ref); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ok bool
	err := s.withPathLock(func() error {
		if err := s.reloadLocked(); err != nil {
			return err
		}
		_, ok = s.secrets[ref]
		return nil
	})
	return ok, err
}

// Put stores value under ref. Values are held only by the Hub process and the
// encrypted file; they are never included in returned errors or listings.
func (s *Store) Put(ref, value string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("secret value is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withPathLock(func() error {
		if err := s.reloadLocked(); err != nil {
			return err
		}
		previous, hadPrevious := s.secrets[ref]
		s.secrets[ref] = value
		if err := s.persistLocked(); err != nil {
			if hadPrevious {
				s.secrets[ref] = previous
			} else {
				delete(s.secrets, ref)
			}
			return err
		}
		return nil
	})
}

// Delete removes ref. Deleting an absent reference is idempotent.
func (s *Store) Delete(ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.withPathLock(func() error {
		if err := s.reloadLocked(); err != nil {
			return err
		}
		previous, ok := s.secrets[ref]
		if !ok {
			return nil
		}
		delete(s.secrets, ref)
		if err := s.persistLocked(); err != nil {
			s.secrets[ref] = previous
			return err
		}
		return nil
	})
}

// String deliberately reports only metadata. In particular, it never formats
// the in-memory map because that would expose provider values in diagnostics.
func (s *Store) String() string {
	if s == nil {
		return "secretstore.Store<nil>"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("secretstore.Store{refs:%d}", len(s.secrets))
}

// MarshalJSON provides a safe diagnostic listing of references without values.
func (s *Store) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	refs := make([]string, 0, len(s.secrets))
	for ref := range s.secrets {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return json.Marshal(struct {
		Refs []string `json:"refs"`
	}{Refs: refs})
}

func (s *Store) reloadLocked() error {
	if s.path == "" {
		return nil
	}
	if err := ensurePrivateDirectory(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("read secret store directory: %w", err)
	}

	info, err := os.Lstat(s.path)
	if os.IsNotExist(err) {
		s.secrets = map[string]string{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat secret store: %w", err)
	}
	if err := validateSecretFileInfo(info); err != nil {
		return fmt.Errorf("read secret store: %w", err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read secret store: %w", err)
	}
	after, err := os.Lstat(s.path)
	if err != nil {
		return fmt.Errorf("stat secret store after read: %w", err)
	}
	if err := validateSecretFileInfo(after); err != nil {
		return fmt.Errorf("read secret store: %w", err)
	}
	if !os.SameFile(info, after) {
		return fmt.Errorf("read secret store: file changed during read")
	}

	var file encryptedFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("read secret store: invalid encrypted payload")
	}
	if file.Version != 1 || len(file.Nonce) != s.gcm.NonceSize() || len(file.Ciphertext) <= s.gcm.Overhead() {
		return fmt.Errorf("read secret store: invalid encrypted payload")
	}
	plain, err := s.gcm.Open(nil, file.Nonce, file.Ciphertext, nil)
	if err != nil {
		return fmt.Errorf("read secret store: decrypt payload")
	}
	var decoded map[string]string
	if err := json.Unmarshal(plain, &decoded); err != nil || decoded == nil {
		return fmt.Errorf("read secret store: invalid secret mapping")
	}
	for ref, value := range decoded {
		if err := validateRef(ref); err != nil || strings.TrimSpace(value) == "" {
			return fmt.Errorf("read secret store: invalid secret mapping")
		}
	}
	s.secrets = decoded
	return nil
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := ensurePrivateDirectory(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("write secret store directory: %w", err)
	}
	if info, err := os.Lstat(s.path); err == nil {
		if err := validateSecretFileInfo(info); err != nil {
			return fmt.Errorf("write secret store: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat secret store before write: %w", err)
	}

	plain, err := json.Marshal(s.secrets)
	if err != nil {
		return fmt.Errorf("encode secret store")
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate secret nonce: %w", err)
	}
	raw, err := json.Marshal(encryptedFile{
		Version:    1,
		Nonce:      nonce,
		Ciphertext: s.gcm.Seal(nil, nonce, plain, nil),
	})
	if err != nil {
		return fmt.Errorf("encode encrypted secret store")
	}

	ops := s.ops
	if ops.createTemp == nil {
		ops = defaultFileOps()
	}
	return writePrivateFile(s.path, raw, ops)
}

func writePrivateTemp(ops fileOps, path string, raw []byte) (string, error) {
	tmp, err := ops.createTemp(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("create secret store temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
	}()
	if err := tmp.Chmod(secretFileMode); err != nil {
		_ = ops.remove(tmpPath)
		return "", fmt.Errorf("protect secret store temporary file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = ops.remove(tmpPath)
		return "", fmt.Errorf("write secret store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = ops.remove(tmpPath)
		return "", fmt.Errorf("sync secret store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = ops.remove(tmpPath)
		return "", fmt.Errorf("close secret store: %w", err)
	}
	return tmpPath, nil
}

func renameAndSync(ops fileOps, source, destination string) error {
	if err := ops.rename(source, destination); err != nil {
		return fmt.Errorf("commit secret store: %w", err)
	}
	if info, err := os.Lstat(destination); err != nil {
		return fmt.Errorf("stat committed secret store: %w", err)
	} else if err := validateSecretFileInfo(info); err != nil {
		return fmt.Errorf("commit secret store: %w", err)
	}
	if err := ops.syncDirectory(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("sync secret store directory: %w", err)
	}
	return nil
}

func writePrivateFile(path string, raw []byte, ops fileOps) error {
	tmpPath, err := writePrivateTemp(ops, path, raw)
	if err != nil {
		return err
	}
	if err := renameAndSync(ops, tmpPath, path); err != nil {
		_ = ops.remove(tmpPath)
		return err
	}
	return nil
}

func encryptedBytes(secrets map[string]string, gcm cipher.AEAD) ([]byte, error) {
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, fmt.Errorf("encode secret store")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate secret nonce: %w", err)
	}
	return json.Marshal(encryptedFile{Version: 1, Nonce: nonce, Ciphertext: gcm.Seal(nil, nonce, plain, nil)})
}

func writeJournal(path string, journal rekeyJournalData, ops fileOps) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writePrivateFile(path, raw, ops)
}

// recoverRekey replays the rekey journal — the same protocol the legacy
// migration reuses — left behind by an interrupted persistent operation.
// hubAdminToken is required to validate a legacy-migration rollback: the
// snapshot it would restore is only trusted when it decrypts under the legacy
// derivation of the supplied token, so a journal written for a store this
// token cannot own is left untouched rather than used to overwrite files.
func recoverRekey(dir, hubAdminToken string) error {
	path := filepath.Join(dir, rekeyJournal)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal rekeyJournalData
	if err := json.Unmarshal(raw, &journal); err != nil {
		return fmt.Errorf("read rekey journal: %w", err)
	}
	if journal.State == "committed" {
		for _, p := range []string{journal.StagedCiphertext, journal.StagedKey} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clean committed rekey transient: %w", err)
			}
		}
		// A rekey run owns its snapshots; a legacy-migration run must preserve
		// its operator backup, which survives the completed migration on
		// purpose.
		if !journal.isLegacyMigration() {
			for _, p := range []string{journal.SnapshotCiphertext, journal.SnapshotKey} {
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("clean committed rekey transient: %w", err)
				}
			}
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove committed rekey journal: %w", err)
		}
		return nil
	}
	if journal.State != "prepared" {
		return fmt.Errorf("invalid rekey journal state")
	}
	if journal.isLegacyMigration() {
		if err := validateMigrationRollback(journal, hubAdminToken); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		snapshot, canonical string
		restorable          bool
	}{
		{journal.SnapshotCiphertext, journal.CiphertextPath, journal.CiphertextExisted},
		{journal.SnapshotKey, journal.KeyPath, journal.keyExistedDefaults()},
	} {
		if !item.restorable {
			// The canonical file did not exist before the operation; recovery
			// restores that absence.
			if err := os.Remove(item.canonical); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove rekey rollback file: %w", err)
			}
			continue
		}
		old, err := os.ReadFile(item.snapshot)
		if err != nil {
			return fmt.Errorf("read rekey rollback snapshot: %w", err)
		}
		if err := writePrivateFile(item.canonical, old, defaultFileOps()); err != nil {
			return fmt.Errorf("restore rekey rollback snapshot: %w", err)
		}
	}
	if journal.isLegacyMigration() {
		// Keep the legacy backup: it is the operator's pre-migration store and
		// recovery does not consume it.
		for _, p := range []string{journal.StagedCiphertext, journal.StagedKey, path} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clean rekey rollback transient: %w", err)
			}
		}
		return nil
	}
	for _, p := range []string{journal.SnapshotCiphertext, journal.SnapshotKey, journal.StagedCiphertext, journal.StagedKey, path} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clean rekey rollback transient: %w", err)
		}
	}
	return nil
}

// validateMigrationRollback refuses to roll a legacy migration back onto
// ciphertext the supplied token cannot actually own. The snapshot must decrypt
// under the legacy derivation — proving the store belongs to this
// deployment — and the staged/new state it would replace is left untouched
// when it does not, so the journal waits for the original token instead.
func validateMigrationRollback(journal rekeyJournalData, hubAdminToken string) error {
	snapshot, err := os.ReadFile(journal.SnapshotCiphertext)
	if err != nil {
		return fmt.Errorf("read legacy migration rollback snapshot: %w", err)
	}
	legacyGCM, err := newGCM(legacyKey(hubAdminToken))
	if err != nil {
		return err
	}
	if err := legacyDecrypts(snapshot, legacyGCM); err != nil {
		return fmt.Errorf("%w: start the Hub once with the original token to finish recovering the secret store migration", ErrAdminTokenRotated)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, dataDirMode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("data directory is not a private directory")
	}
	if info.Mode().Perm() != dataDirMode {
		return fmt.Errorf("data directory must have mode 0700")
	}
	return nil
}

func validateSecretFileInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("secret file is not a regular file")
	}
	if info.Mode().Perm() != secretFileMode {
		return fmt.Errorf("secret file must have mode 0600")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func validateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("secret reference is required")
	}
	if len(ref) > maxRefLength || !utf8.ValidString(ref) || strings.TrimSpace(ref) != ref {
		return fmt.Errorf("secret reference is unsafe")
	}
	segments := strings.Split(ref, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("secret reference is unsafe")
		}
		for _, char := range segment {
			if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
				continue
			}
			return fmt.Errorf("secret reference is unsafe")
		}
	}
	return nil
}
