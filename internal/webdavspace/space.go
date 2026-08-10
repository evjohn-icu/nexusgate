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
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evjohn-icu/timingdex/internal/cachecoord"
	"golang.org/x/net/webdav"
)

// Linker resolves a virtual asset reference to a real on-disk path. The Hub
// provides an implementation backed by the repository (original via
// GetPrimaryLocation, proxy via GetArtifact).
type Linker interface {
	// OriginalPath returns the absolute path of an asset's original media
	// file, or "" when the asset has no accessible primary location.
	OriginalPath(ctx context.Context, assetID string) string
	// ProxyPath returns the absolute path of an asset's proxy artifact, or
	// "" when none exists.
	ProxyPath(ctx context.Context, assetID string) string
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
// preserving the real extension so editing software can match it.
func assetVirtualName(realPath string) string {
	ext := filepath.Ext(realPath)
	if ext == "" {
		ext = ".bin"
	}
	return "original" + ext
}

// LinkOriginal adds the asset's original media to the space. It returns the
// virtual path the file will appear at, or an error if the asset cannot be
// resolved (no primary location).
func (s *Space) LinkOriginal(ctx context.Context, assetID string) (string, error) {
	real := s.linker.OriginalPath(ctx, assetID)
	if real == "" {
		return "", fmt.Errorf("asset %s has no accessible original media", assetID)
	}
	vpath := path.Join("/assets", assetID, assetVirtualName(real))
	s.mu.Lock()
	s.entries[vpath] = "original"
	s.mu.Unlock()
	return vpath, nil
}

// LinkProxy adds the asset's proxy artifact to the space.
func (s *Space) LinkProxy(ctx context.Context, assetID string) (string, error) {
	real := s.linker.ProxyPath(ctx, assetID)
	if real == "" {
		return "", fmt.Errorf("asset %s has no proxy artifact", assetID)
	}
	vpath := path.Join("/assets", assetID, "proxy"+filepath.Ext(real))
	s.mu.Lock()
	s.entries[vpath] = "proxy"
	s.mu.Unlock()
	return vpath, nil
}

// Resolve returns the real on-disk path for a virtual path inside the space,
// or ("", false) when the path is not linked.
func (s *Space) Resolve(ctx context.Context, vpath string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	kind, ok := s.entries[vpath]
	if !ok {
		return "", false
	}
	assetID := assetIDFromVirtualPath(vpath)
	if assetID == "" {
		return "", false
	}
	switch kind {
	case "original":
		return s.linker.OriginalPath(ctx, assetID), true
	case "proxy":
		return s.linker.ProxyPath(ctx, assetID), true
	}
	return "", false
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
	// Any intermediate directory that has at least one linked file beneath it
	// exists for listing purposes (/assets, /assets/<id>).
	if isDirPrefix(fs.space, vpath) {
		return &dirInfo{name: path.Base(vpath)}, nil
	}
	real, ok := fs.space.Resolve(ctx, vpath)
	if !ok {
		return nil, os.ErrNotExist
	}
	if real == "" {
		return nil, os.ErrNotExist
	}
	lock, err := fs.sharedLock()
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	return os.Stat(real)
}

// isDirPrefix reports whether vpath is a directory prefix that contains at
// least one linked entry beneath it (so directory listings can traverse it).
func isDirPrefix(s *Space, vpath string) bool {
	prefix := strings.TrimPrefix(vpath, "/")
	if prefix == "" {
		return true
	}
	for _, vp := range s.VirtualPaths() {
		if strings.HasPrefix(strings.TrimPrefix(vp, "/"), prefix+"/") {
			return true
		}
	}
	return false
}

func (fs *spaceFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, &permissionError{"write"}
	}
	vpath := fs.toVirtual(name)
	if vpath == "/" || vpath == "" {
		return &dirFile{name: "/"}, nil
	}
	// Directory listing: any prefix path that contains linked entries.
	if isDirPrefix(fs.space, vpath) {
		return &listingFile{fs: fs, dir: vpath}, nil
	}
	real, ok := fs.space.Resolve(ctx, vpath)
	if !ok {
		return nil, os.ErrNotExist
	}
	if real == "" {
		return nil, os.ErrNotExist
	}
	lock, err := fs.sharedLock()
	if err != nil {
		return nil, err
	}
	file, err := os.Open(real)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	return &lockedFile{File: file, lock: lock}, nil
}

func (fs *spaceFS) sharedLock() (*cachecoord.Lock, error) {
	if fs.space.dataDir == "" {
		return nil, nil
	}
	return cachecoord.AcquireShared(fs.space.dataDir)
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

// dirFile is a read-only directory node.
type dirFile struct{ name string }

func (d *dirFile) Read([]byte) (int, error)           { return 0, fmt.Errorf("cannot read a directory") }
func (d *dirFile) Seek(int64, int) (int64, error)     { return 0, fmt.Errorf("cannot seek a directory") }
func (d *dirFile) Readdir(int) ([]os.FileInfo, error) { return []os.FileInfo{}, nil }
func (d *dirFile) Stat() (os.FileInfo, error)         { return &dirInfo{name: d.name}, nil }
func (d *dirFile) Close() error                       { return nil }
func (d *dirFile) Write([]byte) (int, error)          { return 0, &permissionError{"write"} }

// listingFile serves the synthetic directory listing of an asset folder.
type listingFile struct {
	fs  *spaceFS
	dir string
}

func (l *listingFile) Read([]byte) (int, error) { return 0, fmt.Errorf("cannot read a directory") }
func (l *listingFile) Seek(int64, int) (int64, error) {
	return 0, fmt.Errorf("cannot seek a directory")
}
func (l *listingFile) Close() error               { return nil }
func (l *listingFile) Write([]byte) (int, error)  { return 0, &permissionError{"write"} }
func (l *listingFile) Stat() (os.FileInfo, error) { return &dirInfo{name: l.dir}, nil }
func (l *listingFile) Readdir(int) ([]os.FileInfo, error) {
	prefix := l.dir + "/"
	prefixRel := strings.TrimPrefix(prefix, "/")
	var out []os.FileInfo
	for _, vp := range l.fs.space.VirtualPaths() {
		rel := strings.TrimPrefix(vp, "/")
		if !strings.HasPrefix(rel, prefixRel) {
			continue
		}
		rest := strings.TrimPrefix(rel, prefixRel)
		// Direct child: file for /assets/<id>/, asset dir for /assets/.
		if strings.Contains(rest, "/") {
			continue
		}
		name := path.Base(vp)
		if strings.HasSuffix(l.dir, "/assets") || l.dir == "/assets" {
			// The entry is an asset directory, not a file.
			out = append(out, &dirInfo{name: name})
			continue
		}
		real, _ := l.fs.space.Resolve(context.Background(), vp)
		fi, err := os.Stat(real)
		if err != nil {
			fi = &fileInfoStub{name: name}
		} else {
			fi = &namedFileInfo{fi, name}
		}
		out = append(out, fi)
	}
	return out, nil
}

type fileInfoStub struct{ name string }

func (f *fileInfoStub) Name() string       { return f.name }
func (f *fileInfoStub) Size() int64        { return 0 }
func (f *fileInfoStub) Mode() os.FileMode  { return 0o444 }
func (f *fileInfoStub) ModTime() time.Time { return time.Time{} }
func (f *fileInfoStub) IsDir() bool        { return false }
func (f *fileInfoStub) Sys() any           { return nil }

type namedFileInfo struct {
	os.FileInfo
	name string
}

func (f *namedFileInfo) Name() string { return f.name }
