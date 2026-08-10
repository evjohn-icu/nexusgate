package app

import (
	"context"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// JobIssues returns the failure backlog grouped by category, the shape behind
// GET /api/v1/issues. It is a count of how much work is stuck and why, the
// same class of queue fact as JobSummary, and carries no failure text — the
// per-job last_error_message stays behind the admin token, as it does on the
// job list.
func (s *Service) JobIssues(ctx context.Context) ([]domain.JobIssue, error) {
	return s.repo.JobIssues(ctx)
}

// RequeueFailedJobsByCategory is RequeueFailedJobs narrowed to one failure
// category, the write side of the issues view's per-category retry. The
// category string is one of domain.JobFailureCategory, exactly what
// JobIssues reports — 'unknown' revives the jobs whose failure never carried
// a code.
func (s *Service) RequeueFailedJobsByCategory(ctx context.Context, category string) (int, error) {
	return s.pipeline.repo.RequeueFailedJobsByCategory(ctx, category)
}

// ResumeDeferredJobsByCategory releases the parked-deferral half of one
// issue category early. The repository's ResumeDeferredJobs is already
// category-scoped — its reason parameter is compared against last_error_code
// directly — so this is the same call the all-reasons path makes, with the
// caller's category (a JobFailureCategory that is also a deferral code:
// provider_route_exhausted or disk_space_low) instead of the hardcoded
// route-exhausted default.
func (s *Service) ResumeDeferredJobsByCategory(ctx context.Context, category string) (int, error) {
	return s.pipeline.repo.ResumeDeferredJobs(ctx, category)
}
