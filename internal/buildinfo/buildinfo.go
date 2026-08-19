// Package buildinfo holds the single-source-of-truth binary version for the
// timingdex executables. It is a leaf package with no dependencies so both the
// Hub and Worker binaries can import it without dragging anything in.
package buildinfo

// Version is the binary version, injected at release build time via
//
//	-ldflags "-X github.com/evjohn-icu/timingdex/internal/buildinfo.Version=vX.Y.Z-alpha"
//
// and left at the "dev" default for any local build.
var Version = "dev"

// VersionString returns the embedded binary version, never empty. Callers that
// persist or report a version (worker enrollment, heartbeats) must use this so
// an empty string can only mean "not yet observed", not "build forgot to set
// it".
func VersionString() string {
	if Version == "" {
		return "dev"
	}
	return Version
}
