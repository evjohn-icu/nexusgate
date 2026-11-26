//go:build !unix

package webdavspace

// openProbeFlag has no portable meaning outside Unix. Windows named pipes
// are reparse points, which os.Root refuses to open outright, so there is
// nothing to defuse there.
const openProbeFlag = 0
