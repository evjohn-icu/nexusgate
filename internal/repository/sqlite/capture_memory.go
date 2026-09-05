package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

const (
	defaultShootSessionLimit = 100
	maxShootSessionLimit     = 1000
)

// SaveShootSession upserts one session and replaces its asset memberships.
// Repeating the same save is therefore safe and cannot duplicate links.
func (r *Repository) SaveShootSession(ctx context.Context, session domain.ShootSession) error {
	if strings.TrimSpace(session.ID) == "" {
		return errors.New("shoot session id is required")
	}
	if strings.TrimSpace(session.State) == "" {
		session.State = "manual"
	}

	now := time.Now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO shoot_sessions(id,root_id,title,state,starts_at,ends_at,region_label,camera_label,confidence,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET root_id=excluded.root_id,title=excluded.title,state=excluded.state,starts_at=excluded.starts_at,ends_at=excluded.ends_at,region_label=excluded.region_label,camera_label=excluded.camera_label,confidence=excluded.confidence,updated_at=excluded.updated_at`,
		session.ID, nullString(session.RootID), session.Title, session.State, nullableTime(session.StartsAt), nullableTime(session.EndsAt), session.RegionLabel, session.CameraLabel, session.Confidence, formatTime(now), formatTime(now))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM asset_shoot_sessions WHERE session_id=?`, session.ID); err != nil {
		return err
	}

	seen := make(map[string]struct{}, len(session.AssetIDs))
	for _, assetID := range session.AssetIDs {
		assetID = strings.TrimSpace(assetID)
		if assetID == "" {
			continue
		}
		if _, ok := seen[assetID]; ok {
			continue
		}
		seen[assetID] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO asset_shoot_sessions(asset_id,session_id,is_primary,created_at) VALUES(?,?,1,?)`, assetID, session.ID, formatTime(now)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// shootSessionFilterWhere compiles a ShootSessionFilter into a WHERE clause
// and its bound arguments (excluding limit/offset, which callers append). It
// is shared by ListShootSessions and PagedShootSessions so the two can never
// disagree about which sessions a filter selects.
func shootSessionFilterWhere(filter domain.ShootSessionFilter) (string, []any) {
	where := []string{"1=1"}
	args := make([]any, 0, 10)
	if value := strings.TrimSpace(filter.RootID); value != "" {
		where = append(where, "root_id=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.State); value != "" {
		where = append(where, "state=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.RegionLabel); value != "" {
		where = append(where, "region_label=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.CameraLabel); value != "" {
		where = append(where, "camera_label=?")
		args = append(args, value)
	}
	if filter.StartsAfter != nil {
		where = append(where, "starts_at>=?")
		args = append(args, formatTime(*filter.StartsAfter))
	}
	if filter.StartsBefore != nil {
		where = append(where, "starts_at<=?")
		args = append(args, formatTime(*filter.StartsBefore))
	}
	if filter.EndsAfter != nil {
		where = append(where, "ends_at>=?")
		args = append(args, formatTime(*filter.EndsAfter))
	}
	if filter.EndsBefore != nil {
		where = append(where, "ends_at<=?")
		args = append(args, formatTime(*filter.EndsBefore))
	}
	return strings.Join(where, " AND "), args
}

// ListShootSessions returns the browse-safe session projection. Exact
// latitude/longitude values are not selected or populated by this API.
func (r *Repository) ListShootSessions(ctx context.Context, filter domain.ShootSessionFilter) ([]domain.ShootSession, error) {
	where, args := shootSessionFilterWhere(filter)
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultShootSessionLimit
	}
	if limit > maxShootSessionLimit {
		limit = maxShootSessionLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)

	query := `SELECT id,COALESCE(root_id,''),title,state,starts_at,ends_at,region_label,camera_label,confidence,created_at,updated_at
FROM shoot_sessions WHERE ` + where + ` ORDER BY starts_at DESC,id LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	sessions, err := r.scanShootSessions(rows)
	if err != nil {
		return nil, err
	}
	return r.populateShootSessionAssets(ctx, sessions)
}

// PagedShootSessions is the paged form of ListShootSessions: one probe row
// past the requested page so the caller can distinguish "this page is full"
// from "this is the last page" without a second query. The existing ordering
// already carries the id tie-break.
func (r *Repository) PagedShootSessions(ctx context.Context, filter domain.ShootSessionFilter) ([]domain.ShootSession, bool, error) {
	where, args := shootSessionFilterWhere(filter)
	limit := filter.Limit
	if limit <= 0 {
		limit = defaultShootSessionLimit
	}
	if limit > maxShootSessionLimit {
		limit = maxShootSessionLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit+1, offset)

	query := `SELECT id,COALESCE(root_id,''),title,state,starts_at,ends_at,region_label,camera_label,confidence,created_at,updated_at
FROM shoot_sessions WHERE ` + where + ` ORDER BY starts_at DESC,id LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	sessions, err := r.scanShootSessions(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(sessions) > limit
	if hasMore {
		sessions = sessions[:limit]
	}
	sessions, err = r.populateShootSessionAssets(ctx, sessions)
	if err != nil {
		return nil, false, err
	}
	return sessions, hasMore, nil
}

// scanShootSessions scans a materialized shoot-session result set into
// browse-safe projections.
func (r *Repository) scanShootSessions(rows *sql.Rows) ([]domain.ShootSession, error) {
	defer rows.Close()
	sessions := make([]domain.ShootSession, 0)
	for rows.Next() {
		var session domain.ShootSession
		var rootID, startsAt, endsAt, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&session.ID, &rootID, &session.Title, &session.State, &startsAt, &endsAt, &session.RegionLabel, &session.CameraLabel, &session.Confidence, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		session.RootID = rootID.String
		session.StartsAt = parseNullableTime(startsAt)
		session.EndsAt = parseNullableTime(endsAt)
		session.CreatedAt = parseStoredTime(createdAt)
		session.UpdatedAt = parseStoredTime(updatedAt)
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

// populateShootSessionAssets fills the AssetIDs of a page of sessions with one
// batched membership lookup instead of one query per session.
func (r *Repository) populateShootSessionAssets(ctx context.Context, sessions []domain.ShootSession) ([]domain.ShootSession, error) {
	if len(sessions) == 0 {
		return sessions, nil
	}
	sessionIDs := make([]string, len(sessions))
	for i, session := range sessions {
		sessionIDs[i] = session.ID
	}
	assetIDsBySession, err := r.listShootSessionAssetIDsBatch(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	for index := range sessions {
		sessions[index].AssetIDs = assetIDsBySession[sessions[index].ID]
	}
	return sessions, nil
}

// GetShootSession returns the browse-safe detail projection for one session.
// Coordinates remain intentionally unavailable at this boundary.
func (r *Repository) GetShootSession(ctx context.Context, sessionID string) (*domain.ShootSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}

	var session domain.ShootSession
	var rootID, startsAt, endsAt, createdAt, updatedAt sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id,COALESCE(root_id,''),title,state,starts_at,ends_at,region_label,camera_label,confidence,created_at,updated_at FROM shoot_sessions WHERE id=?`, sessionID).Scan(
		&session.ID, &rootID, &session.Title, &session.State, &startsAt, &endsAt, &session.RegionLabel, &session.CameraLabel, &session.Confidence, &createdAt, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	session.RootID = rootID.String
	session.StartsAt = parseNullableTime(startsAt)
	session.EndsAt = parseNullableTime(endsAt)
	session.CreatedAt = parseStoredTime(createdAt)
	session.UpdatedAt = parseStoredTime(updatedAt)
	session.AssetIDs, err = r.listShootSessionAssetIDs(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// ListShootSessionAssets lists the persisted memberships for one session.
func (r *Repository) ListShootSessionAssets(ctx context.Context, sessionID string) ([]domain.ShootSessionAsset, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("shoot session id is required")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT asset_id,session_id,is_primary,created_at FROM asset_shoot_sessions WHERE session_id=? ORDER BY is_primary DESC,asset_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assets := make([]domain.ShootSessionAsset, 0)
	for rows.Next() {
		var asset domain.ShootSessionAsset
		var primary int
		var createdAt string
		if err := rows.Scan(&asset.AssetID, &asset.SessionID, &primary, &createdAt); err != nil {
			return nil, err
		}
		asset.IsPrimary = primary != 0
		asset.CreatedAt = parseStoredTimeString(createdAt, "asset_shoot_sessions.created_at")
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (r *Repository) listShootSessionAssetIDs(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT asset_id FROM asset_shoot_sessions WHERE session_id=? ORDER BY is_primary DESC,asset_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assetIDs := make([]string, 0)
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			return nil, err
		}
		assetIDs = append(assetIDs, assetID)
	}
	return assetIDs, rows.Err()
}

// listShootSessionAssetIDsBatch answers listShootSessionAssetIDs for every
// session in sessionIDs with one query instead of one query per session, so
// ListShootSessions doesn't run a page's worth (up to maxShootSessionLimit)
// of separate lookups.
func (r *Repository) listShootSessionAssetIDsBatch(ctx context.Context, sessionIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(sessionIDs)), ",")
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
	}
	query := `SELECT session_id, asset_id FROM asset_shoot_sessions WHERE session_id IN (` + placeholders + `) ORDER BY session_id, is_primary DESC, asset_id`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID, assetID string
		if err := rows.Scan(&sessionID, &assetID); err != nil {
			return nil, err
		}
		out[sessionID] = append(out[sessionID], assetID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range sessionIDs {
		if out[id] == nil {
			out[id] = make([]string, 0)
		}
	}
	return out, nil
}

func parseNullableTime(value sql.NullString) *time.Time {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	parsed := parseStoredTimeString(value.String, "capture_metadata.captured_at")
	return &parsed
}

func parseStoredTime(value sql.NullString) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return parseStoredTimeString(value.String, "capture_metadata.reference_time")
}

func parseStoredTimeString(value string, source string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil && strings.TrimSpace(value) != "" {
		slog.Debug("failed to parse stored time", "value", value, "source", source, "error", err)
	}
	return parsed
}
