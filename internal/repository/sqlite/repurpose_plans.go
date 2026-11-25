package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/idgen"
)

// SaveRepurposePlan refuses to overwrite a plan a human already approved.
// This check and the ones in SaveRepurposePlanRevision and
// ApproveRepurposePlanRevision below are where the human-approval boundary is
// actually enforced -- they are the only ones that run against state no
// concurrent caller can change underneath them, so they are also the only
// ones that can be trusted. They wrap domain sentinels rather than returning
// bare prose so the layers above can react to what was refused with errors.Is
// instead of re-deriving it from a second, unlocked read; see
// internal/domain/errors.go.
//
// The refusal is carried by the upsert itself (ON CONFLICT ... DO UPDATE ...
// WHERE repurpose_plans.status='draft'), not by a separate unlocked read: a
// plan that is approved when the statement runs matches zero rows and the
// save is refused, and the generic save API only ever writes draft status, so
// an approval can neither be overwritten nor bypassed through this path.
func (r *Repository) SaveRepurposePlan(ctx context.Context, plan domain.RepurposePlan) (domain.RepurposePlan, error) {
	now := time.Now().UTC()
	if plan.ID == "" {
		plan.ID = idgen.New()
	}
	if plan.Status == "" || plan.Status == "approved" {
		plan.Status = "draft"
	}
	if plan.CreatedAt.IsZero() {
		plan.CreatedAt = now
	}
	plan.UpdatedAt = now
	encoded, err := json.Marshal(plan)
	if err != nil {
		return domain.RepurposePlan{}, err
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO repurpose_plans(id,brief,duration_ms,style,audience,title,status,provider,model,plan_json,created_at,updated_at) VALUES(?,?,?,?,?,?,'draft',?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET brief=excluded.brief,duration_ms=excluded.duration_ms,style=excluded.style,audience=excluded.audience,title=excluded.title,status='draft',provider=excluded.provider,model=excluded.model,plan_json=excluded.plan_json,updated_at=excluded.updated_at WHERE repurpose_plans.status='draft'`, plan.ID, plan.Brief, plan.DurationMS, plan.Style, plan.Audience, plan.Title, plan.Provider, plan.Model, string(encoded), formatTime(plan.CreatedAt), formatTime(plan.UpdatedAt))
	if err != nil {
		return domain.RepurposePlan{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.RepurposePlan{}, err
	}
	if n == 0 {
		return domain.RepurposePlan{}, fmt.Errorf("%w: plan %s is approved and accepts no further writes", domain.ErrPlanImmutable, plan.ID)
	}
	return plan, nil
}

func (r *Repository) GetRepurposePlan(ctx context.Context, id string) (*domain.RepurposePlan, error) {
	var planJSON, created, updated string
	err := r.db.QueryRowContext(ctx, `SELECT plan_json,created_at,updated_at FROM repurpose_plans WHERE id=?`, id).Scan(&planJSON, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plan domain.RepurposePlan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, fmt.Errorf("decode repurpose plan: %w", err)
	}
	plan.ID = id
	plan.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	plan.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &plan, nil
}

// ListRepurposePlans returns the plan inbox projection, most recently
// updated first. status filters to draft or approved; any other value ("" or
// "all") returns both. The projection comes from denormalized columns plus a
// revision subquery — never the full candidate payloads.
func (r *Repository) ListRepurposePlans(ctx context.Context, status string, limit int) ([]domain.RepurposePlanSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := `SELECT p.id,p.title,p.brief,p.status,p.duration_ms,p.updated_at,
	COALESCE((SELECT COUNT(*) FROM repurpose_plan_revisions r WHERE r.plan_id=p.id),0),
	COALESCE((SELECT MAX(r.revision) FROM repurpose_plan_revisions r WHERE r.plan_id=p.id),0),
	COALESCE(json_array_length(COALESCE(json_extract(p.plan_json,'$.missing_needs'),'[]')),0)
	FROM repurpose_plans p`
	args := make([]any, 0, 2)
	if status == "draft" || status == "approved" {
		query += ` WHERE p.status=?`
		args = append(args, status)
	}
	query += ` ORDER BY p.updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RepurposePlanSummary
	for rows.Next() {
		var s domain.RepurposePlanSummary
		var updated string
		if err := rows.Scan(&s.ID, &s.Title, &s.Brief, &s.Status, &s.DurationMS, &updated, &s.RevisionCount, &s.LatestRevision, &s.MissingNeedsCount); err != nil {
			return nil, err
		}
		s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, s)
	}
	return out, rows.Err()
}

// PagedRepurposePlans is the paged form of ListRepurposePlans with an id
// tie-break on the updated_at ordering and a probe row for has-more.
func (r *Repository) PagedRepurposePlans(ctx context.Context, status string, limit, offset int) ([]domain.RepurposePlanSummary, bool, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	query := `SELECT p.id,p.title,p.brief,p.status,p.duration_ms,p.updated_at,
	COALESCE((SELECT COUNT(*) FROM repurpose_plan_revisions r WHERE r.plan_id=p.id),0),
	COALESCE((SELECT MAX(r.revision) FROM repurpose_plan_revisions r WHERE r.plan_id=p.id),0),
	COALESCE(json_array_length(COALESCE(json_extract(p.plan_json,'$.missing_needs'),'[]')),0)
	FROM repurpose_plans p`
	args := make([]any, 0, 2)
	if status == "draft" || status == "approved" {
		query += ` WHERE p.status=?`
		args = append(args, status)
	}
	query += ` ORDER BY p.updated_at DESC, p.id LIMIT ? OFFSET ?`
	args = append(args, limit+1, offset)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]domain.RepurposePlanSummary, 0, limit+1)
	for rows.Next() {
		var s domain.RepurposePlanSummary
		var updated string
		if err := rows.Scan(&s.ID, &s.Title, &s.Brief, &s.Status, &s.DurationMS, &updated, &s.RevisionCount, &s.LatestRevision, &s.MissingNeedsCount); err != nil {
			return nil, false, err
		}
		s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// SaveRepurposePlanRevision records an editor's new draft and its snapshot.
// The writer lock protects the draft check, plan update, and revision insert.
// Failure to publish the snapshot rolls the entire plan update back.
func (r *Repository) SaveRepurposePlanRevision(ctx context.Context, plan domain.RepurposePlan, editorNote string) (domain.RepurposePlanRevision, error) {
	tx, err := r.beginImmediateWrite(ctx)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	defer rollbackAndClose(tx)
	var createdAt, status string
	err = tx.QueryRowContext(ctx, `SELECT created_at,status FROM repurpose_plans WHERE id=?`, plan.ID).Scan(&createdAt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: no plan matches id %s", domain.ErrPlanNotFound, plan.ID)
	}
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if status != "draft" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s is approved and accepts no further revisions", domain.ErrPlanImmutable, plan.ID)
	}
	parsedCreated, _ := time.Parse(time.RFC3339Nano, createdAt)
	now := time.Now().UTC()
	plan.Status = "draft"
	plan.CreatedAt = parsedCreated
	plan.UpdatedAt = now
	encoded, err := json.Marshal(plan)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE repurpose_plans SET brief=?,duration_ms=?,style=?,audience=?,title=?,status='draft',provider=?,model=?,plan_json=?,updated_at=? WHERE id=? AND status='draft'`, plan.Brief, plan.DurationMS, plan.Style, plan.Audience, plan.Title, plan.Provider, plan.Model, string(encoded), formatTime(now), plan.ID)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if n == 0 {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s is approved and accepts no further revisions", domain.ErrPlanImmutable, plan.ID)
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM repurpose_plan_revisions WHERE plan_id=?`, plan.ID).Scan(&latest); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	created := time.Now().UTC()
	revision := domain.RepurposePlanRevision{ID: idgen.New(), PlanID: plan.ID, Revision: latest + 1, State: "draft", Plan: plan, EditorNote: strings.TrimSpace(editorNote), CreatedAt: created}
	if _, err := tx.ExecContext(ctx, `INSERT INTO repurpose_plan_revisions(id,plan_id,revision,state,snapshot_json,editor_note,created_at) VALUES(?,?,?,?,?,?,?)`, revision.ID, revision.PlanID, revision.Revision, revision.State, string(encoded), revision.EditorNote, formatTime(created)); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `COMMIT`); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	return revision, nil
}

func (r *Repository) ListRepurposePlanRevisions(ctx context.Context, planID string) ([]domain.RepurposePlanRevision, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,revision,state,snapshot_json,editor_note,created_at,approved_at FROM repurpose_plan_revisions WHERE plan_id=? ORDER BY revision DESC`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.RepurposePlanRevision, 0)
	for rows.Next() {
		var item domain.RepurposePlanRevision
		var snapshot, created string
		var approved sql.NullString
		if err := rows.Scan(&item.ID, &item.Revision, &item.State, &snapshot, &item.EditorNote, &created, &approved); err != nil {
			return nil, err
		}
		item.PlanID = planID
		if err := json.Unmarshal([]byte(snapshot), &item.Plan); err != nil {
			return nil, fmt.Errorf("decode repurpose plan revision: %w", err)
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if approved.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, approved.String)
			item.ApprovedAt = &parsed
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// ApproveRepurposePlanRevision is the write that makes a human's approval
// real, so its three preconditions -- the revision exists, it is still a
// draft, and it is the plan's latest -- are checked inside the same
// transaction as the two UPDATEs rather than by the caller beforehand. A
// caller's check cannot hold anything between its read and this write, so a
// concurrent approval can always land in that gap; only these can't be raced.
// They wrap domain sentinels for the reason SaveRepurposePlan's comment
// gives.
func (r *Repository) ApproveRepurposePlanRevision(ctx context.Context, planID string, revisionNumber int) (domain.RepurposePlanRevision, error) {
	tx, err := r.beginImmediateWrite(ctx)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	defer rollbackAndClose(tx)
	var id, state, snapshot, editorNote, created string
	err = tx.QueryRowContext(ctx, `SELECT id,state,snapshot_json,editor_note,created_at FROM repurpose_plan_revisions WHERE plan_id=? AND revision=?`, planID, revisionNumber).Scan(&id, &state, &snapshot, &editorNote, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: plan %s has no revision %d", domain.ErrPlanRevisionNotFound, planID, revisionNumber)
	}
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if state != "draft" {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: %s/%d is %s", domain.ErrPlanRevisionNotDraft, planID, revisionNumber, state)
	}
	var latest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM repurpose_plan_revisions WHERE plan_id=?`, planID).Scan(&latest); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if revisionNumber != latest {
		return domain.RepurposePlanRevision{}, fmt.Errorf("%w: %s/%d, latest is %d", domain.ErrPlanRevisionNotLatest, planID, revisionNumber, latest)
	}
	var plan domain.RepurposePlan
	if err := json.Unmarshal([]byte(snapshot), &plan); err != nil {
		return domain.RepurposePlanRevision{}, fmt.Errorf("decode repurpose plan snapshot: %w", err)
	}
	now := time.Now().UTC()
	plan.Status = "approved"
	plan.UpdatedAt = now
	approvedSnapshot, err := json.Marshal(plan)
	if err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repurpose_plan_revisions SET state='approved',snapshot_json=?,approved_at=? WHERE id=?`, string(approvedSnapshot), formatTime(now), id); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repurpose_plans SET status='approved',plan_json=?,updated_at=? WHERE id=?`, string(approvedSnapshot), formatTime(now), planID); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	if _, err := tx.ExecContext(ctx, `COMMIT`); err != nil {
		return domain.RepurposePlanRevision{}, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, created)
	return domain.RepurposePlanRevision{ID: id, PlanID: planID, Revision: revisionNumber, State: "approved", Plan: plan, EditorNote: editorNote, CreatedAt: createdAt, ApprovedAt: &now}, nil
}

func (r *Repository) beginImmediateWrite(ctx context.Context) (*sql.Conn, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func rollbackAndClose(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	_ = conn.Close()
}
