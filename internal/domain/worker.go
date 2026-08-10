package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Worker verdicts. The Hub treats every verdict below Compatible as
// advisory or blocking depending on the value: UpgradeRecommended never
// blocks a lease, Incompatible always does.
const (
	WorkerVerdictCompatible         = "compatible"
	WorkerVerdictUpgradeRecommended = "upgrade_recommended"
	WorkerVerdictIncompatible       = "incompatible"
)

// MinWorkerVersion is the earliest worker binary whose wire protocol this
// Hub accepts. The Hub<->Worker wire (enroll/heartbeat/lease/complete/
// artifact upload) has no version field of its own, so this constant is the
// Hub's floor: v0.26.0 is the last release before the current-dev line
// (v0.28/v0.29) and any protocol-touching change since its wire was frozen.
// Raise it whenever the protocol changes; workers below it are refused
// leases and shown as 不兼容 on the workers page.
const MinWorkerVersion = "v0.26.0"

// WorkerCompat is the Hub's compatibility verdict for one worker binary. It
// rides on the workers list API response so the page can render the verdict
// without re-deriving it in JavaScript. Message is set only for
// Incompatible; the other verdicts are self-explanatory pills.
type WorkerCompat struct {
	Verdict       string `json:"verdict"`
	MinVersion    string `json:"min_version"`
	WorkerVersion string `json:"worker_version"`
	Message       string `json:"message,omitempty"`
}

// WorkerCompatibility lives here rather than in internal/app so the
// repository lease gate can apply it without an import cycle: sqlite cannot
// import app, but an incompatible worker must never receive a lease, and the
// gate has to sit at the repository boundary where the lease is granted.
// internal/app re-exports it as an alias so the two layers cannot disagree.
//
// Rules:
//   - "" or "dev" (a locally built binary that never got ldflags) is
//     UpgradeRecommended: unknown versions are advisory, not blocking -- a
//     dev-built worker should not be refused, but an operator-built release
//     missing its version should be told.
//   - Anything that does not parse as "vX.Y.Z" (1-3 numeric components,
//     optional "v" prefix and pre-release suffix) is also UpgradeRecommended,
//     for the same reason: unknown, do not hard-block.
//   - A different major version is Incompatible: the wire may have broken
//     across the major boundary and the Hub cannot know.
//   - Same major but minor below MinWorkerVersion's minor is Incompatible.
//   - Otherwise Compatible.
func WorkerCompatibility(version string) WorkerCompat {
	verdict := WorkerCompat{
		MinVersion:    MinWorkerVersion,
		WorkerVersion: version,
	}
	trimmed := strings.TrimSpace(version)
	if trimmed == "" || trimmed == "dev" {
		verdict.Verdict = WorkerVerdictUpgradeRecommended
		return verdict
	}
	major, minor, ok := splitSemver(trimmed)
	if !ok {
		verdict.Verdict = WorkerVerdictUpgradeRecommended
		return verdict
	}
	minMajor, minMinor, _ := splitSemver(MinWorkerVersion)
	switch {
	case major != minMajor:
		verdict.Verdict = WorkerVerdictIncompatible
	case minor < minMinor:
		verdict.Verdict = WorkerVerdictIncompatible
	default:
		verdict.Verdict = WorkerVerdictCompatible
	}
	if verdict.Verdict == WorkerVerdictIncompatible {
		verdict.Message = fmt.Sprintf("worker %s is incompatible with this Hub (minimum %s); update the worker binary", version, MinWorkerVersion)
	}
	return verdict
}

// splitSemver parses a semver-ish "vX.Y.Z" into (major, minor). The "v"
// prefix and any pre-release suffix are tolerated ("v0.26.0-alpha" reads as
// 0.26.0), one to three numeric components are accepted with the missing
// trailing ones counted as zero, and everything else is rejected so callers
// can fall back to an advisory verdict. Deliberately no dependency: the only
// comparison ever needed is "is this worker's major/minor at least the
// Hub's floor", and strconv handles it.
func splitSemver(version string) (major, minor int, ok bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return 0, 0, false
	}
	nums := [3]int{}
	for i, part := range parts {
		if part == "" {
			return 0, 0, false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], true
}
