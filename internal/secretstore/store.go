// Package secretstore keeps Hub-only provider keys out of SQLite, configuration
// files, browser state, and Worker configuration. Its encrypted file is a
// local-at-rest protection boundary, not a substitute for OS account security.
package secretstore

import (
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
}

type fileOps struct {
	createTemp    func(string) (*os.File, error)
	rename        func(string, string) error
	syncDirectory func(string) error
	remove        func(string) error
}

type rekeyJournalData struct {
	State              string `json:"state"`
	CiphertextPath     string `json:"ciphertext_path"`
	CiphertextExisted  bool   `json:"ciphertext_existed"`
	KeyPath            string `json:"key_path"`
	SnapshotCiphertext string `json:"snapshot_ciphertext"`
	SnapshotKey        string `json:"snapshot_key"`
	StagedCiphertext   string `json:"staged_ciphertext"`
	StagedKey          string `json:"staged_key"`
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

	if err := recoverRekey(secretDir); err != nil {
		return nil, fmt.Errorf("recover secret store rekey: %w", err)
	}
	if err := migrateLegacyStore(path, keyPath, hubAdminToken); err != nil {
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

	s := &Store{path: path, gcm: gcm, secrets: map[string]string{}, ops: defaultFileOps()}
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
	key := sha256.Sum256([]byte("timingdex/provider-secrets/v1\x00" + hubAdminToken))
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

// migrateLegacyStore upgrades a store written before store.key existed. It is a
// no-op once the key file is present, or when there is no ciphertext to carry
// over. Nothing is written until the legacy payload has decrypted successfully,
// so a failed migration leaves the original file untouched.
func migrateLegacyStore(path, keyPath, hubAdminToken string) error {
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

	if err := copyPrivateFile(path, path+backupSuffix); err != nil {
		return fmt.Errorf("back up secret store before migration: %w", err)
	}
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	migrated := &Store{path: path, gcm: gcm, secrets: legacy.secrets}
	if err := migrated.persistLocked(); err != nil {
		return fmt.Errorf("rewrite secret store during migration: %w", err)
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

func copyPrivateFile(source, destination string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return writePrivateFile(destination, raw, defaultFileOps())
}

// Rekey generates a new data-encryption key, re-encrypts every stored secret
// with it, atomically persists the new ciphertext and key file, and backs up
// the previous key to store.key.pre-rekey. On failure the original files and
// in-memory state are restored.
//
// Rekey is exclusive: it holds the write lock for its entire duration, so no
// other call can observe an inconsistent state. A store opened in process-local
// mode (dataDir empty) has no persistent key to rotate and returns an error.
func (s *Store) Rekey() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return fmt.Errorf("secret store has no persistent key to rotate")
	}

	if err := s.reloadLocked(); err != nil {
		return err
	}

	keyPath := s.keyPath()
	preRekeyPath := keyPath + ".pre-rekey"
	journalPath := filepath.Join(filepath.Dir(s.path), rekeyJournal)
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
	j := rekeyJournalData{State: "prepared", CiphertextPath: s.path, CiphertextExisted: ciphertextExisted, KeyPath: keyPath, SnapshotCiphertext: oldCipherSnapshot, SnapshotKey: oldKeySnapshot, StagedCiphertext: cipherTemp, StagedKey: keyTemp}
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
func (s *Store) Resolve(ref string) (string, bool, error) {
	if err := validateRef(ref); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return "", false, err
	}
	value, ok := s.secrets[ref]
	return value, ok, nil
}

// Has reports whether ref is present without exposing its value.
func (s *Store) Has(_ context.Context, ref string) (bool, error) {
	if err := validateRef(ref); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.reloadLocked(); err != nil {
		return false, err
	}
	_, ok := s.secrets[ref]
	return ok, nil
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
}

// Delete removes ref. Deleting an absent reference is idempotent.
func (s *Store) Delete(ref string) error {
	if err := validateRef(ref); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

func recoverRekey(dir string) error {
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
		for _, p := range []string{journal.SnapshotCiphertext, journal.SnapshotKey, journal.StagedCiphertext, journal.StagedKey} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clean committed rekey transient: %w", err)
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
	for _, item := range []struct{ snapshot, canonical string }{{journal.SnapshotCiphertext, journal.CiphertextPath}, {journal.SnapshotKey, journal.KeyPath}} {
		if item.canonical == journal.CiphertextPath && !journal.CiphertextExisted {
			if err := os.Remove(item.canonical); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove rekey rollback ciphertext: %w", err)
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
	for _, p := range []string{journal.SnapshotCiphertext, journal.SnapshotKey, journal.StagedCiphertext, journal.StagedKey, path} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clean rekey rollback transient: %w", err)
		}
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
