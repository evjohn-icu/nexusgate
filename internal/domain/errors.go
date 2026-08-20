package domain

import "errors"

// ErrShotVectorNotFound reports that a shot has no semantic vector under the
// current embedding scheme. It lives here, in the one package every layer
// already imports, because the alternative was worse: the API used to decide
// between 404 and 500 with strings.HasPrefix(err.Error(), "shot not found:"),
// so rewording a repository error — which is ordinary maintenance — silently
// turned a missing shot into a 500. Callers must match this with errors.Is.
//
// The condition is deliberately broader than "no such shot". A shot embedded
// under a superseded scheme is filtered out by the model predicate on
// shot_semantic_vectors and lands here too: from the caller's side both mean
// "nothing to compare against", and the distinction that matters to an
// operator belongs in the wrapped message, not in the sentinel.
var ErrShotVectorNotFound = errors.New("no semantic vector for shot")

// ErrJobLeaseLost reports that a caller's asserted ownership of a job's lease
// no longer matches the jobs row. Two call shapes produce it, and both wrap
// it through leaseLostErr (internal/repository/sqlite/repository.go):
//
//   - A Hub-local completion write (CompleteJob, FailJobTerminally, RetryJob,
//     DeferJob, SaveArtifact) loses its compare-and-swap: the lease was
//     already reclaimed by another owner by the time the write landed.
//   - A Worker's HTTP request (internal/repository/sqlite/remote_jobs.go:
//     artifact prepare/commit, credential leasing, progress/event reporting,
//     RetryWorkerJob/CompleteWorkerJob) finds the ownership check itself
//     fails: the job was never leased to this Worker, the lease expired, or
//     someone else's write already took it.
//
// These read as a write racing and a read confirming the same race was
// already lost, which is one condition, not two, so they share this sentinel
// rather than each minting its own name for it. What differs is what the
// caller does about it, not what it means: RunUntilIdle
// (internal/app/pipeline.go) treats the Hub-local case as expected
// contention — log and move to the next job, since the new holder's attempt
// is the one of record — while internal/api/server.go reports the
// Worker-facing case back to the Worker as 409 Conflict, so a node that no
// longer holds a job's lease learns that from the response instead of a
// generic failure it might retry forever.
//
// It lives here, in the one package every layer already imports, so it can
// be matched with errors.Is instead of a message substring. A substring
// match ties correctness to prose no compiler checks — the exact trap
// ErrShotVectorNotFound's comment describes — and this case is worse than
// that one: a reworded string would not just misroute an HTTP status, it
// would make the pipeline treat a lost lease as a fatal error and abort an
// entire run, or (if the wording drifted the other way and started
// resembling a wrapped io error) swallow a genuine failure as ordinary
// contention.
var ErrJobLeaseLost = errors.New("job lease no longer held")

// The five sentinels below are the human-approval boundary CLAUDE.md calls
// non-negotiable -- "immutable revisions; approval is human-only and locks
// the plan. Agents may draft; humans approve" -- given errors.Is-matchable
// identities.
//
// They live here for the reason ErrShotVectorNotFound's comment gives, plus
// one this boundary adds on top. The checks that enforce the rule run inside
// the repository's transaction (internal/repository/sqlite/repository.go),
// which is the only place they cannot be raced; the layers that must react to
// them are internal/app and internal/api. Naming them in internal/app instead
// left the enforcement site unable to say what it had refused, so every layer
// above it re-derived "does this revision exist / is it a draft / is it the
// latest" from a second, unlocked read -- once as a pre-check before the
// write and once again after the write failed. The same rule ended up written
// three times across two packages, which is three chances for it to drift
// apart. Wrapped once at the enforcement site and carried up by errors.Is, it
// is written once.
//
// internal/app re-exports these under the same names (internal/app/service.go,
// internal/app/export.go) so the API layer keeps reading in terms of app's
// vocabulary. Those are plain aliases, not new errors.New values: one
// identity per condition, so errors.Is against app's name and against this
// one can never disagree.

// ErrPlanNotFound reports that a repurpose plan id names nothing.
// GetRepurposePlan returns (nil, nil) rather than sql.ErrNoRows for a missing
// plan, so the absence carries no structural marker of its own -- callers
// used to recognize it by the literal phrase "not found" in a message, which
// meant an unrelated lower-layer error containing the same phrase read as a
// 404, and rewording the message read as a 500. It is deliberately distinct
// from ErrPlanRevisionNotFound: this one means the plan id names nothing,
// that one means the plan is real but the revision number is not one of its
// revisions.
var ErrPlanNotFound = errors.New("repurpose plan not found")

// ErrPlanImmutable reports a write attempted against a plan a human has
// already approved. The human-approval boundary is only as real as this
// check: approval is meant to be the last write a plan ever accepts, so once
// its status is "approved" every further revision has to be refused, not
// silently recorded as a draft nobody approved. It used to be reported by
// matching the literal word "immutable" in the message internal/api built
// from the plan's own state, so rewording that message -- ordinary
// maintenance -- would have let a 409 fall through as a 500.
var ErrPlanImmutable = errors.New("approved repurpose plan is immutable")

// ErrPlanRevisionNotFound reports that the requested revision number is not
// one of the plan's revisions. A caller working from a stale revision list
// hits this rather than ErrPlanNotFound; see that sentinel for why the two
// are not one.
var ErrPlanRevisionNotFound = errors.New("repurpose plan revision not found")

// ErrPlanRevisionNotDraft reports that approval was requested for a revision
// that is no longer in the "draft" state -- most often because it was already
// approved. Only a draft can become the approved revision; approving an
// already-approved revision a second time is not idempotent, it is a caller
// acting on state that already moved.
var ErrPlanRevisionNotDraft = errors.New("repurpose plan revision is not a draft")

// ErrPlanRevisionNotLatest reports that approval was requested for a revision
// that is not the plan's newest. Approving an older revision would resurrect
// a selection the operator already superseded by drafting a newer one,
// silently discarding the edit they made in between.
var ErrPlanRevisionNotLatest = errors.New("only the latest repurpose plan revision can be approved")

// ErrCommitRunNotValidated reports that CommitAnalysis or CommitAnalysisWithShots
// was called with a run whose state is not 'validated'. Only a run that passed
// validation (StageModelRun) can be committed to canonical tables; a run in
// 'running', 'failed', or 'committed' state is refused, and the transaction
// rolls back without writing to asset_analysis, asset_shots, or asset_tag_links.
//
// The guard lives inside commitAnalysisTx (the single code path both public
// methods share) as a SELECT before any INSERT, so the check and the write are
// inside the same transaction and cannot race. The existing UPDATE … WHERE
// state='validated' clause on the model_runs row stays as a second layer: in
// the unlikely event of a concurrent commit from another connection the UPDATE
// would affect 0 rows, and the caller would see a commit with no model_runs
// transition, which is also caught.
//
// It is wrapped with domain.Permanent at the enforcement site because a retry
// with the same run ID would find the same state — the condition is structural,
// not transient — but callers that want to distinguish "wrong state" from
// "model output garbage" can still match this sentinel through Permanent's
// Unwrap chain.
var ErrCommitRunNotValidated = errors.New("commit requires a validated model run")

// ErrModelRunNotRunning reports a strict transition attempted against a run
// that is not currently running. Model-run completion is deliberately
// non-idempotent so stale provider callbacks cannot overwrite a newer result.
var ErrModelRunNotRunning = errors.New("model run is not running")

// ErrPermanentFailure marks a failure that a retry cannot change. It replaces
// the list of message substrings isRetryableJobError (internal/app/pipeline.go)
// used to keep, and it exists because that list could not stay correct by
// construction — CLAUDE.md's own warning about it ("if you add a new
// permanent failure mode that isn't an HTTP status, add its phrase there")
// describes a rule enforced only by memory, and memory had already failed
// three ways at once:
//
//   - It went stale, and stale failed *open*. Two of the four rejections
//     validateAnalysisShots can return ("shot description is required",
//     "ends after asset duration") were never given a phrase, so a model
//     answer that failed validation was re-requested from the same model
//     through the whole 1s→2s→4s backoff: a paid call per attempt for an
//     answer already known to be unusable.
//   - It rotted, invisibly. "validation error" (with a space) had no
//     producer anywhere in the tree. A phrase that matches nothing raises
//     nothing — no compile error, no vet warning, no failing test — it just
//     quietly stops classifying whatever it was written for.
//   - It matched by accident. The phrases were tested against the whole
//     chain's text, so a 5xx body echoing "not configured", or FFmpeg stderr
//     containing "unsupported ", was declared permanent on the strength of
//     prose nobody in this repository wrote. isRetryableJobError already
//     defends against exactly that on its status-code path; the substring
//     path had no such defence.
//
// Marking the error where it is created moves the decision to the only place
// the answer is actually known, and frees the message to be reworded — which
// is what a message is for.
//
// The default is unchanged and must stay unchanged: an unmarked error is
// retried. A network blip, a locked database, a provider having a bad minute
// all arrive unmarked, and they are the majority. The bar for marking is not
// "I would rather not retry this" but a structural argument: the next attempt
// would present identical inputs to a deterministic decision, so its outcome
// is already known.
//
// It is deliberately one sentinel, not one per family ("model output failed
// validation", "provider channel switched off", "required derived artifact
// missing"). Those mean different things to an operator but the same thing to
// the retry decision, and the operator-facing difference is already carried
// by the wrapped message, by model_runs.failure_code and by
// jobs.last_error_message. Splitting it would rebuild the thing being deleted:
// isRetryableJobError would again hold a list — of sentinels instead of
// phrases — and a fourth family added without touching that list would be
// retried, which is this comment's first bullet in a new spelling. One
// sentinel is what leaves no list to forget.
var ErrPermanentFailure = errors.New("permanent failure")

// Permanent marks err without touching its text. The message is what reaches
// jobs.last_error_message and the /progress page, so it stays the sentence
// the operator needs; the marker rides alongside it where only errors.Is can
// see it. Wrapping with fmt.Errorf("%w: %w", ErrPermanentFailure, err) would
// have prefixed every terminal failure in the UI with a word the job's own
// state already says.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err}
}

type permanentError struct{ error }

// Is answers for the marker only, and Unwrap keeps the wrapped chain
// reachable, so errors.Is and errors.As against whatever the error already
// carried — a *common.StatusError, ErrJobLeaseLost — still resolve through
// it. A marked error is an ordinary error with one more thing true about it,
// never a replacement for what it wraps.
func (*permanentError) Is(target error) bool { return target == ErrPermanentFailure }
func (e *permanentError) Unwrap() error      { return e.error }

// ErrJobNotAssignable reports that a job cannot be assigned because it is
// currently running. SetDeriveWorkerAssignment wraps this when a caller tries
// to change the assignment of a job that is already being processed; the API
// maps it to 409 Conflict.
var ErrJobNotAssignable = errors.New("job is running and cannot be reassigned")

// ErrInvalidAssignment reports that a worker assignment cannot be fulfilled
// because the job, worker, or combination is invalid — the job doesn't exist,
// the worker doesn't exist, or the worker has been revoked. The API maps it
// to 400 Bad Request.
var ErrInvalidAssignment = errors.New("invalid worker assignment")

// ErrCollectionExists reports that an asset collection cannot be saved
// because its name is already taken. The asset_collections.name column has a
// UNIQUE constraint; the repository layer wraps the SQLite constraint
// violation with this sentinel so the API can map it to 409 Conflict rather
// than a generic 500.
var ErrCollectionExists = errors.New("collection name already exists")

// ErrShotNotFound reports that a shot id names nothing in asset_shots. The
// shot basket's AddShotToCollection (internal/repository/sqlite/collections.go)
// checks existence before the insert; without the sentinel the plain string
// error would fall through the API classifier's default branch and answer 500
// for what is a caller mistake (a stale or mistyped shot id). The API maps it
// to 404 Not Found.
var ErrShotNotFound = errors.New("shot not found")

// ErrCollectionNotFound reports that a collection id names nothing.
var ErrCollectionNotFound = errors.New("collection not found")

// ErrWorkerNotFound reports that a worker id names nothing in the workers
// table. RevokeWorker (internal/repository/sqlite/repository.go) returns it
// when its UPDATE touches no row — the id was never enrolled, or its row was
// removed — so the API can answer 404 for what is a caller mistake (a stale
// or mistyped id) instead of the classifier's default 500. It lives here, in
// domain, for the same reason ErrShotNotFound does: the repository enforces
// the condition at the only place that cannot be raced, and the layers above
// match it with errors.Is rather than by reading a message that maintenance
// may reword.
var ErrWorkerNotFound = errors.New("worker not found")

// ErrReorderInvalid reports that a collection shot reorder cannot be applied
// because the submitted list does not match the collection's current pins:
// a length mismatch, a duplicate id, or a shot that is not in the collection.
// All three are a stale client racing the basket's current state (or a
// client-side bug), never a server fault, so the API maps it to 409 Conflict
// and the caller refreshes the basket.
var ErrReorderInvalid = errors.New("collection shot reorder does not match the current pins")
