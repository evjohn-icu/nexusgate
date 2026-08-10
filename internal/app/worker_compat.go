package app

import "github.com/evjohn-icu/timingdex/internal/domain"

// MinWorkerVersion is the earliest worker binary whose wire protocol this
// Hub accepts. The wire (enroll/heartbeat/lease/complete/artifact upload)
// carries no version of its own, so the Hub defines the floor: v0.26.0 is
// the last release whose worker wire matches the current Hub, and v0.28/
// v0.29 are still current-dev. Raise it with every protocol change; workers
// below it are refused leases and flagged 不兼容 on the workers page.
const MinWorkerVersion = domain.MinWorkerVersion

// WorkerCompatibility is re-exported from domain (the single source of
// truth) so the repository lease gate can use it without a package cycle:
// sqlite cannot import app, but an incompatible worker must never receive a
// lease, so the verdict logic lives where both layers can reach it and this
// alias keeps the two from disagreeing.
func WorkerCompatibility(version string) domain.WorkerCompat {
	return domain.WorkerCompatibility(version)
}
