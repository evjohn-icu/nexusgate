//go:build unix

package webdavspace

import "syscall"

// openProbeFlag opens media candidates without blocking on special files: a
// FIFO opened read-only would otherwise block the open until a writer
// appears. Regular files ignore the flag entirely, so real media is
// unaffected; openRooted's regular-file check rejects everything else.
const openProbeFlag = syscall.O_NONBLOCK
