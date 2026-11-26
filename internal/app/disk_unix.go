//go:build !windows

package app

import "syscall"

// freeBytes reports the space available to the calling user on the filesystem
// holding path. A full cache volume can interrupt encoding and source staging.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
