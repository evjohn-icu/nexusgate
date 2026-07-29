package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ev/timingdex/internal/domain"
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

// ListShootSessions returns the browse-safe session projection. Exact
// latitude/longitude values are not selected or populated by this API.
func (r *Repository) ListShootSessions(ctx context.Context, filter domain.ShootSessionFilter) ([]domain.ShootSession, error) {
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
FROM shoot_sessions WHERE ` + strings.Join(where, " AND ") + ` ORDER BY starts_at DESC,id LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	sessions := make([]domain.ShootSession, 0)
	for rows.Next() {
		var session domain.ShootSession
		var rootID, startsAt, endsAt, createdAt, updatedAt sql.NullString
		if err := rows.Scan(&session.ID, &rootID, &session.Title, &session.State, &startsAt, &endsAt, &session.RegionLabel, &session.CameraLabel, &session.Confidence, &createdAt, &updatedAt); err != nil {
			rows.Close()
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
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

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
		asset.CreatedAt = parseStoredTimeString(createdAt)
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
	parsed := parseStoredTimeString(value.String)
	return &parsed
}

func parseStoredTime(value sql.NullString) time.Time {
	if !value.Valid {
		return time.Time{}
	}
	return parseStoredTimeString(value.String)
}

func parseStoredTimeString(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}
