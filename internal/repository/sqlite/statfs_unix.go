//go:build !windows

package sqlite

import "syscall"

// unixFreeBytes reports the free bytes on the filesystem holding path, using
// the blocks available to unprivileged users (Bavail, not Bfree, so a root
// reserve is not counted as usable space).
func unixFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func init() {
	statfsFreeBytes = unixFreeBytes
}
