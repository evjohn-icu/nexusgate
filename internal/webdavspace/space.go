// Package webdavspace implements on-demand, read-only WebDAV virtual spaces
// for delivering footage to editing agents. A space starts empty; an agent
// request (via the MCP server) links a specific asset in, and the file then
// appears as an ordinary file under /spaces/{id}/assets/{asset}/... while the
// bytes are streamed from the NAS location — nothing is copied and no real
// path is ever revealed to the client.
package webdavspace

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/cachecoord"
	"golang.org/x/net/webdav"
)

// Linker resolves a virtual asset reference to a trusted on-disk location: a
// directory root plus the file's path relative to that root. The Hub provides
// an implementation backed by the repository (originals via the asset's
// primary location, proxies via GetArtifact). Handing out a root/relative
// pair instead of an absolute path is what lets serve time confine every
// open to the root with os.OpenRoot: a stale or wrong link can only fail,
// never escape the configured tree.
type Linker interface {
	// OriginalLocation returns the trusted root directory and the asset's
	// original media path relative to that root (native separators), or
	// ("", "") when the asset has no accessible primary location.
	OriginalLocation(ctx context.Context, assetID string) (root, relative string)
	// ProxyLocation returns the trusted root directory and the asset's proxy
	// artifact path relative to that root (native separators), or ("", "")
	// when none exists.
	ProxyLocation(ctx context.Context, assetID string) (root, relative string)
}

// Space is one on-demand virtual directory. It is created empty; Link adds
// asset entries that then resolve through the Linker at read time.
type Space struct {
	ID        string
	CreatedAt time.Time

	mu      sync.RWMutex
	entries map[string]string // virtual path ("/assets/<id>/original.mov") -> kind
	linker  Linker
	dataDir string
}

// NewSpace creates an empty space whose entries resolve via linker.
func NewSpace(id string, linker Linker, dataDir ...string) *Space {
	dir := ""
	if len(dataDir) > 0 {
		dir = dataDir[0]
	}
	return &Space{ID: id, CreatedAt: time.Now().UTC(), entries: map[string]string{}, linker: linker, dataDir: dir}
}

// assetVirtualName returns the virtual filename for an original asset,
// preserving the real extension (taken from the linker's relative path) so
// editing software can match it.
func assetVirtualName(relative string) string {
	ext := filepath.Ext(relative)
	if ext == "" {
		ext = ".bin"
	}
	return "original" + ext
}

// LinkOriginal adds the asset's original media to the space. It returns the
// virtual path the file will appear at, or an error if the asset cannot be
// resolved (no primary location).
func (s *Space) LinkOriginal(ctx context.Context, assetID string) (string, error) {
	root, relative := s.linker.OriginalLocation(ctx, assetID)
	if root == "" || relative == "" {
		return "", fmt.Errorf("asset %s has no accessible original media", assetID)
	}
	vpath := path.Join("/assets", assetID, assetVirtualName(relative))
	s.mu.Lock()
	s.entries[vpath] = "original"
	s.mu.Unlock()
	return vpath, nil
}

// LinkProxy adds the asset's proxy artifact to the space.
func (s *Space) LinkProxy(ctx context.Context, assetID string) (string, error) {
	root, relative := s.linker.ProxyLocation(ctx, assetID)
	if root == "" || relative == "" {
		return "", fmt.Errorf("asset %s has no proxy artifact", assetID)
	}
	vpath := path.Join("/assets", assetID, "proxy"+filepath.Ext(relative))
	s.mu.Lock()
	s.entries[vpath] = "proxy"
	s.mu.Unlock()
	return vpath, nil
}

// Resolve keeps the virtual path lookup and returns the trusted root/relative
// pair backing it, or ("", "", false) when the path is not linked to an
// available location. The pair never contains an absolute media path.
func (s *Space) Resolve(ctx context.Context, vpath string) (root, relative string, ok bool) {
	s.mu.RLock()
	kind, linked := s.entries[vpath]
	s.mu.RUnlock()
	if !linked {
		return "", "", false
	}
	assetID := assetIDFromVirtualPath(vpath)
	if assetID == "" {
		return "", "", false
	}
	switch kind {
	case "original":
		root, relative = s.linker.OriginalLocation(ctx, assetID)
	case "proxy":
		root, relative = s.linker.ProxyLocation(ctx, assetID)
	default:
		return "", "", false
	}
	if root == "" || relative == "" {
		return "", "", false
	}
	return root, relative, true
}

func assetIDFromVirtualPath(vpath string) string {
	parts := strings.Split(strings.TrimPrefix(vpath, "/"), "/")
	// /assets/<id>/<file>
	if len(parts) != 3 || parts[0] != "assets" {
		return ""
	}
	return parts[1]
}

// VirtualPaths returns all linked virtual paths (sorted) for listing.
func (s *Space) VirtualPaths() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.entries))
	for p := range s.entries {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// spaceChild is one direct child entry of a space directory.
type spaceChild struct {
	name  string
	isDir bool
	// info carries the opened file's metadata for file children; it is nil
	// for synthetic directory children.
	info os.FileInfo
}

func (c spaceChild) fileInfo() os.FileInfo {
	if c.isDir {
		return &dirInfo{name: c.name}
	}
	return &backedFileInfo{FileInfo: c.info, name: c.name}
}

// childrenOf derives the direct children of the virtual directory dir from
// the space's linked entries: every linked entry below dir contributes its
// first remaining path component, deduplicated and sorted by name. Only
// existing children are reported — a file child must open as a regular file
// through its trusted root/relative pair, and a directory child must still
// contain at least one existing entry of its own, so a vanished NAS or a
// stale link appears as nothing rather than as an empty directory.
func (s *Space) childrenOf(ctx context.Context, dir string) []spaceChild {
	prefix := strings.Trim(strings.TrimPrefix(dir, "/"), "/")
	firstComponents := map[string]bool{} // child name -> has linked entries beneath it
	for _, vp := range s.VirtualPaths() {
		rel := strings.TrimPrefix(vp, "/")
		if rel == "" {
			continue
		}
		if prefix != "" {
			if !strings.HasPrefix(rel, prefix+"/") {
				continue
			}
			rel = strings.TrimPrefix(rel, prefix+"/")
		}
		isDir := false
		if i := strings.Index(rel, "/"); i >= 0 {
			rel, isDir = rel[:i], true
		}
		firstComponents[rel] = firstComponents[rel] || isDir
	}
	children := make([]spaceChild, 0, len(firstComponents))
	for name, isDir := range firstComponents {
		if isDir {
			if len(s.childrenOf(ctx, path.Join(dir, name))) == 0 {
				continue
			}
			children = append(children, spaceChild{name: name, isDir: true})
			continue
		}
		root, relative, ok := s.Resolve(ctx, path.Join(dir, name))
		if !ok {
			continue
		}
		info, err := statRooted(root, relative)
		if err != nil {
			continue // missing or unsupported: appears unavailable
		}
		children = append(children, spaceChild{name: name, info: info})
	}
	sort.Slice(children, func(i, j int) bool { return children[i].name < children[j].name })
	return children
}

// spaceFS adapts a Space to webdav.FileSystem. Only read operations are
// supported; every write returns a PermissionError so the space is
// source-media-readonly by construction.
type spaceFS struct {
	space *Space
	root  string // virtual root prefix this FS serves (e.g. /spaces/<id>)
}

// VirtualRoot returns the root path prefix served by this filesystem.
func (s *Space) NewHandlerFS(root string) *spaceFS {
	return &spaceFS{space: s, root: root}
}

type permissionError struct{ op string }

func (e *permissionError) Error() string {
	return fmt.Sprintf("%s not allowed: space is read-only", e.op)
}

func (fs *spaceFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return &permissionError{"mkdir"}
}

func (fs *spaceFS) RemoveAll(ctx context.Context, name string) error {
	return &permissionError{"remove"}
}

func (fs *spaceFS) Rename(ctx context.Context, oldName, newName string) error {
	return &permissionError{"rename"}
}

func (fs *spaceFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	vpath := fs.toVirtual(name)
	if vpath == "/" || vpath == "" {
		return &dirInfo{name: "/"}, nil
	}
	// The same directory predicate OpenFile applies: a virtual prefix with
	// at least one existing linked entry beneath it.
	if len(fs.space.childrenOf(ctx, vpath)) > 0 {
		return &dirInfo{name: path.Base(vpath)}, nil
	}
	file, lock, err := fs.openSpaceFile(ctx, vpath)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
		_ = lock.Release()
	}()
	fi, err := file.Stat()
	if err != nil {
		return nil, err
	}
	return &backedFileInfo{FileInfo: fi, name: path.Base(vpath)}, nil
}

func (fs *spaceFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, &permissionError{"write"}
	}
	vpath := fs.toVirtual(name)
	// The space root always exists, so an empty space can still be listed.
	if vpath == "/" || vpath == "" {
		return &spaceDirFile{dir: "/", entries: fs.space.childrenOf(ctx, "/")}, nil
	}
	// A virtual prefix with at least one existing linked entry beneath it is
	// a synthetic directory — the same predicate Stat applies.
	if children := fs.space.childrenOf(ctx, vpath); len(children) > 0 {
		return &spaceDirFile{dir: vpath, entries: children}, nil
	}
	file, lock, err := fs.openSpaceFile(ctx, vpath)
	if err != nil {
		return nil, err
	}
	return &lockedFile{File: file, lock: lock}, nil
}

// openSpaceFile resolves a virtual file path through the linker to its
// trusted root/relative pair, opens the file confined to that root, and
// takes the shared cache lock that keeps cache maintenance from deleting the
// file while it is being served. On success the caller owns file and lock
// and must close the file before releasing the lock; on every error both
// handles are already released.
func (fs *spaceFS) openSpaceFile(ctx context.Context, vpath string) (*os.File, *cachecoord.Lock, error) {
	root, relative, ok := fs.space.Resolve(ctx, vpath)
	if !ok {
		return nil, nil, os.ErrNotExist
	}
	lock, err := fs.sharedLock()
	if err != nil {
		return nil, nil, err
	}
	file, err := openRooted(root, relative)
	if err != nil {
		_ = lock.Release()
		return nil, nil, err
	}
	return file, lock, nil
}

func (fs *spaceFS) sharedLock() (*cachecoord.Lock, error) {
	if fs.space.dataDir == "" {
		return nil, nil
	}
	return cachecoord.AcquireShared(fs.space.dataDir)
}

// openRooted opens the file at relative under root, confined to the root
// directory by os.Root. Links that escape the configured root, missing files
// and non-regular entries all appear uniformly unavailable (os.ErrNotExist);
// the underlying cause stays wrapped for server-side logging. The root
// directory handle is always released; the returned file stays open.
func openRooted(root, relative string) (*os.File, error) {
	if !containedRelative(relative) {
		return nil, &os.PathError{Op: "open", Path: relative, Err: os.ErrNotExist}
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("%w: open media root: %w", os.ErrNotExist, err)
	}
	defer dir.Close()
	// openProbeFlag keeps a FIFO (or other special file) in the root from
	// blocking the open; the regular-file check rejects it right after.
	file, err := dir.OpenFile(relative, os.O_RDONLY|openProbeFlag, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open media: %w", os.ErrNotExist, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("%w: stat media: %w", os.ErrNotExist, err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, &os.PathError{Op: "open", Path: relative, Err: os.ErrNotExist}
	}
	return file, nil
}

// statRooted reports the metadata of the file at relative under root through
// a confined, transient open; only regular files report anything.
func statRooted(root, relative string) (os.FileInfo, error) {
	file, err := openRooted(root, relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

// containedRelative reports whether relative stays inside its root after
// cleaning: always relative, never backing out of the root. os.Root still
// enforces the real containment; this cheap check just makes escape attempts
// report like any other unavailable link.
func containedRelative(relative string) bool {
	if relative == "" {
		return false
	}
	clean := filepath.Clean(relative)
	return !filepath.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

type lockedFile struct {
	*os.File
	lock *cachecoord.Lock
}

func (f *lockedFile) Close() error {
	err := f.File.Close()
	if lockErr := f.lock.Release(); err == nil {
		err = lockErr
	}
	return err
}

func (fs *spaceFS) toVirtual(name string) string {
	clean := path.Clean(name)
	if clean == "." || clean == "/" || clean == fs.root {
		return "/"
	}
	return strings.TrimPrefix(clean, fs.root)
}

// dirInfo reports a synthetic directory entry.
type dirInfo struct{ name string }

func (d *dirInfo) Name() string       { return d.name }
func (d *dirInfo) Size() int64        { return 0 }
func (d *dirInfo) Mode() os.FileMode  { return os.ModeDir | 0o555 }
func (d *dirInfo) ModTime() time.Time { return time.Time{} }
func (d *dirInfo) IsDir() bool        { return true }
func (d *dirInfo) Sys() any           { return nil }

// backedFileInfo presents an opened media file under its virtual name; the
// real filename on disk never leaves the server.
type backedFileInfo struct {
	os.FileInfo
	name string
}

func (f *backedFileInfo) Name() string { return f.name }

// spaceDirFile is the single synthetic read-only directory node shared by
// /, /assets and /assets/<id>. Its children are derived once at open time
// from the space's linked entries; Readdir pages them with the standard
// offset/EOF contract.
type spaceDirFile struct {
	dir     string
	entries []spaceChild
	offset  int
}

func (d *spaceDirFile) Read([]byte) (int, error) { return 0, fmt.Errorf("cannot read a directory") }
func (d *spaceDirFile) Seek(int64, int) (int64, error) {
	return 0, fmt.Errorf("cannot seek a directory")
}
func (d *spaceDirFile) Close() error               { return nil }
func (d *spaceDirFile) Write([]byte) (int, error)  { return 0, &permissionError{"write"} }
func (d *spaceDirFile) Stat() (os.FileInfo, error) { return &dirInfo{name: path.Base(d.dir)}, nil }

// Readdir serves the directory's children. A positive count pages from the
// current offset and reports io.EOF once the end is reached, like
// os.File.Readdir; a nonpositive count returns every remaining entry in one
// slice with a nil error.
func (d *spaceDirFile) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		out := make([]os.FileInfo, 0, max(0, len(d.entries)-d.offset))
		for _, child := range d.entries[d.offset:] {
			out = append(out, child.fileInfo())
		}
		d.offset = len(d.entries)
		return out, nil
	}
	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}
	end := min(d.offset+count, len(d.entries))
	out := make([]os.FileInfo, 0, end-d.offset)
	for _, child := range d.entries[d.offset:end] {
		out = append(out, child.fileInfo())
	}
	d.offset = end
	if d.offset >= len(d.entries) {
		return out, io.EOF
	}
	return out, nil
}
