// Package modelinstall downloads locked model artifacts and verifies them.
//
// The lock file is the only source of URLs, file names and hashes. Nothing here
// accepts a caller-supplied URL, host or path: the catalog id selects an entry,
// the entry's registry decides the host, and every byte is checked against the
// pinned SHA-256 before it is published under the model directory. A download
// that fails verification leaves no file behind, so a partially written
// artifact can never be mistaken for an installed one.
package modelinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// LockSchemaVersion is the only lock schema this installer understands.
const LockSchemaVersion = "inference.models.lock.v1"

// Lock is the parsed models.lock.json.
type Lock struct {
	SchemaVersion string  `json:"schema_version"`
	Models        []Entry `json:"models"`
}

// Entry is one locked model.
type Entry struct {
	CatalogID string `json:"catalog_id"`
	ModelID   string `json:"model_id"`
	Revision  string `json:"revision"`
	Registry  struct {
		Type  string   `json:"type"`
		Repo  string   `json:"repo"`
		Ref   string   `json:"ref"`
		Hosts []string `json:"hosts"`
	} `json:"registry"`
	Files   []File `json:"files"`
	License struct {
		Code    string `json:"code"`
		Weights string `json:"weights"`
		Source  string `json:"source"`
	} `json:"license"`
	ArtifactStatus string `json:"artifact_status"`
}

// File is one locked artifact file.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// LoadLock reads and validates the lock file. It rejects duplicate catalog ids
// and files without a hash: an unverifiable artifact is not installable.
func LoadLock(lockPath string) (*Lock, error) {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read model lock: %w", err)
	}
	var lock Lock
	if err := json.Unmarshal(raw, &lock); err != nil {
		return nil, fmt.Errorf("parse model lock: %w", err)
	}
	if lock.SchemaVersion != LockSchemaVersion {
		return nil, fmt.Errorf("unsupported model lock schema %q", lock.SchemaVersion)
	}
	seen := make(map[string]bool, len(lock.Models))
	for _, entry := range lock.Models {
		if entry.CatalogID == "" {
			return nil, fmt.Errorf("model lock contains an entry without a catalog id")
		}
		if seen[entry.CatalogID] {
			return nil, fmt.Errorf("model lock repeats catalog id %q", entry.CatalogID)
		}
		seen[entry.CatalogID] = true
		for _, file := range entry.Files {
			if strings.TrimSpace(file.SHA256) == "" {
				return nil, fmt.Errorf("model %q file %q has no SHA-256", entry.CatalogID, file.Path)
			}
			if !safeRelativePath(file.Path) {
				return nil, fmt.Errorf("model %q file %q escapes the model directory", entry.CatalogID, file.Path)
			}
		}
	}
	return &lock, nil
}

// Lookup returns the entry for a catalog id.
func (l *Lock) Lookup(catalogID string) (Entry, bool) {
	for _, entry := range l.Models {
		if entry.CatalogID == catalogID {
			return entry, true
		}
	}
	return Entry{}, false
}

func safeRelativePath(p string) bool {
	if p == "" || filepath.IsAbs(p) || strings.Contains(p, "\\") {
		return false
	}
	cleaned := path.Clean(p)
	return !strings.HasPrefix(cleaned, "..") && cleaned != "." && !strings.HasPrefix(cleaned, "/")
}

// Installer downloads and verifies one model at a time.
type Installer struct {
	Lock     *Lock
	ModelDir string
	// Client is the download transport. Nil uses a client with a 30s header
	// timeout and no overall timeout (a large weights file legitimately takes
	// minutes; per-request contexts carry the deadline).
	Client *http.Client
	// URLFor overrides URL construction in tests. It must still be called with
	// an allowlisted entry.
	URLFor func(entry Entry, file File) (string, error)
	// OnProgress receives (file, bytesWritten, totalBytes).
	OnProgress func(file string, written, total int64)
}

// Install downloads every missing file for catalogID and publishes them
// atomically. It returns the digest of the verified file list.
func (i *Installer) Install(ctx context.Context, catalogID string) (string, error) {
	if i == nil || i.Lock == nil {
		return "", fmt.Errorf("installer has no lock")
	}
	if strings.TrimSpace(i.ModelDir) == "" {
		return "", fmt.Errorf("installer has no model directory")
	}
	entry, ok := i.Lock.Lookup(catalogID)
	if !ok {
		return "", fmt.Errorf("catalog id %q is not locked", catalogID)
	}
	if entry.ArtifactStatus == "blocked_gated" {
		return "", fmt.Errorf("model %q is gated and no access acceptance exists", catalogID)
	}
	if len(entry.Files) == 0 {
		return "", fmt.Errorf("model %q has no locked files", catalogID)
	}
	client := i.Client
	if client == nil {
		client = &http.Client{Timeout: 0, Transport: &http.Transport{ResponseHeaderTimeout: 30 * time.Second}}
	}
	digestParts := make([]string, 0, len(entry.Files))
	for _, file := range entry.Files {
		target := filepath.Join(i.ModelDir, catalogID, filepath.FromSlash(file.Path))
		if ok, err := fileMatches(target, file); err != nil {
			return "", err
		} else if ok {
			digestParts = append(digestParts, file.Path+":"+file.SHA256)
			continue
		}
		source, err := i.urlFor(entry, file)
		if err != nil {
			return "", err
		}
		if err := i.download(ctx, client, entry, source, target, file); err != nil {
			return "", err
		}
		digestParts = append(digestParts, file.Path+":"+file.SHA256)
	}
	manifest := map[string]any{
		"catalog_id":   entry.CatalogID,
		"model_id":     entry.ModelID,
		"revision":     entry.Revision,
		"files":        digestParts,
		"installed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.MarshalIndent(manifest, "", " ")
	if err != nil {
		return "", err
	}
	if err := writeAtomic(filepath.Join(i.ModelDir, catalogID, "install.json"), encoded); err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (i *Installer) urlFor(entry Entry, file File) (string, error) {
	if i.URLFor != nil {
		raw, err := i.URLFor(entry, file)
		if err != nil {
			return "", err
		}
		return i.validateURL(entry, raw)
	}
	var raw string
	switch entry.Registry.Type {
	case "huggingface", "huggingface_gated":
		raw = fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", entry.Registry.Repo, entry.Revision, file.Path)
	case "modelscope":
		raw = fmt.Sprintf("https://modelscope.cn/models/%s/resolve/%s/%s", entry.Registry.Repo, entry.Revision, file.Path)
	case "kaggle_models":
		return "", fmt.Errorf("model %q is fetched by the Kaggle tooling, not the HTTP installer", entry.CatalogID)
	default:
		return "", fmt.Errorf("model %q has unknown registry type %q", entry.CatalogID, entry.Registry.Type)
	}
	return i.validateURL(entry, raw)
}

// validateURL enforces the registry's own host allowlist. A redirect to any
// other host is rejected by the same check in the redirect hook.
func (i *Installer) validateURL(entry Entry, raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("model %q has an invalid download URL: %w", entry.CatalogID, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("model %q download must use https", entry.CatalogID)
	}
	allowed := false
	for _, host := range entry.Registry.Hosts {
		if strings.EqualFold(host, parsed.Hostname()) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("model %q download host %q is not in the locked allowlist", entry.CatalogID, parsed.Hostname())
	}
	return raw, nil
}

func (i *Installer) download(ctx context.Context, client *http.Client, entry Entry, rawURL, target string, file File) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	// The client's redirect policy must re-check the allowlist; a signed CDN
	// URL is fine only when the lock named that host.
	if client.CheckRedirect == nil {
		clone := *client
		clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			_, err := i.validateURL(entry, req.URL.String())
			return err
		}
		client = &clone
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", file.Path, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", file.Path, response.StatusCode)
	}
	if file.Size > 0 && response.ContentLength >= 0 && response.ContentLength != file.Size {
		return fmt.Errorf("download %s: content length %d does not match the locked %d", file.Path, response.ContentLength, file.Size)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".download-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	published := false
	defer func() {
		if !published {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	hasher := sha256.New()
	writer := io.MultiWriter(tmp, hasher)
	written, err := copyWithProgress(writer, response.Body, file.Size, func(n int64) {
		if i.OnProgress != nil {
			i.OnProgress(file.Path, n, file.Size)
		}
	})
	if err != nil {
		return fmt.Errorf("download %s: %w", file.Path, err)
	}
	if file.Size > 0 && written != file.Size {
		return fmt.Errorf("download %s: wrote %d bytes, want %d", file.Path, written, file.Size)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, file.SHA256) {
		return fmt.Errorf("download %s: sha256 %s does not match the locked %s", file.Path, got, file.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	published = true
	return nil
}

// copyWithProgress reads exactly limit bytes when limit is positive and reports
// cumulative progress; it never buffers the artifact in memory.
func copyWithProgress(dst io.Writer, src io.Reader, limit int64, progress func(int64)) (int64, error) {
	buf := make([]byte, 1<<20)
	var total int64
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			if limit > 0 && total+int64(n) > limit {
				return total, fmt.Errorf("response is larger than the locked %d bytes", limit)
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return total, err
			}
			total += int64(n)
			if progress != nil {
				progress(total)
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func fileMatches(target string, file File) (bool, error) {
	info, err := os.Stat(target)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || (file.Size > 0 && info.Size() != file.Size) {
		return false, nil
	}
	handle, err := os.Open(target)
	if err != nil {
		return false, err
	}
	defer handle.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, handle); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), file.SHA256), nil
}

func writeAtomic(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".manifest-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, target)
}

// VerifyInstalled re-checks every file of an installed model against the lock
// without touching the network. Probe uses it before asking the runtime to load
// the model, so "installed" is a fact about bytes rather than a memory of a
// successful download.
func (i *Installer) VerifyInstalled(catalogID string) error {
	entry, ok := i.Lock.Lookup(catalogID)
	if !ok {
		return fmt.Errorf("catalog id %q is not locked", catalogID)
	}
	for _, file := range entry.Files {
		target := filepath.Join(i.ModelDir, catalogID, filepath.FromSlash(file.Path))
		ok, err := fileMatches(target, file)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("model %q file %q is missing or does not match the lock", catalogID, file.Path)
		}
	}
	return nil
}
