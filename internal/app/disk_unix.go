//go:build !windows

package app

import "syscall"

// freeBytes reports the free bytes on the filesystem holding path — the probe
// behind the pipeline's disk-space preflight. The guard exists for NAS/local
// Linux hubs: a full cache volume makes ffmpeg fail mid-encode and lets
// cache/sources staging copies fill the disk silently. disk_windows.go exists
// so the preflight compiles on Windows, where it degrades to "unknown" and is
// skipped rather than ever blocking a job.
func freeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
