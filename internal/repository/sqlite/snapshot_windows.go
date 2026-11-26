//go:build windows

package sqlite

import "golang.org/x/sys/windows"

// The pre-migration snapshot publication mirrors Worker config publication:
// the temporary file is flushed before the move and MoveFileEx with
// MOVEFILE_WRITE_THROUGH makes the replacement itself durable, so no separate
// directory handle sync is needed. Opening a directory and calling Sync on it
// fails with "Access is denied" on Windows, which aborted migrations that had
// already produced a valid snapshot.

// publishSnapshotFile publishes the flushed temporary snapshot atomically.
// MOVEFILE_WRITE_THROUGH flushes the rename itself, after the temporary
// file's FlushFileBuffers in preMigrationSnapshotWith.
func publishSnapshotFile(tempPath, snapshotPath string) error {
	from, err := windows.UTF16PtrFromString(tempPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(snapshotPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
