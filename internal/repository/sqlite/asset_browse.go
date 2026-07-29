package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/ev/timingdex/internal/domain"
)

const processingStatusSQL = `CASE
WHEN a.state='missing' THEN 'missing'
WHEN EXISTS (SELECT 1 FROM jobs jrunning WHERE jrunning.asset_id=a.id AND jrunning.state='running') THEN 'processing'
WHEN EXISTS (SELECT 1 FROM jobs jqueued WHERE jqueued.asset_id=a.id AND jqueued.state='pending') THEN 'queued'
WHEN EXISTS (SELECT 1 FROM jobs jfailed WHERE jfailed.asset_id=a.id AND jfailed.state='failed') THEN 'failed'
WHEN EXISTS (SELECT 1 FROM derived_artifacts dready WHERE dready.asset_id=a.id AND dready.artifact_type='proxy') THEN 'ready'
ELSE 'discovered' END`

func assetBrowseWhere(filter domain.AssetCardFilter) (string, []any) {
	where := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if filter.CapturedFrom != nil {
		where = append(where, `COALESCE(cm.captured_at,m.captured_at)>=?`)
		args = append(args, formatTime(filter.CapturedFrom.UTC()))
	}
	if filter.CapturedTo != nil {
		where = append(where, `COALESCE(cm.captured_at,m.captured_at)<?`)
		args = append(args, formatTime(filter.CapturedTo.UTC()))
	}
	if value := strings.TrimSpace(filter.RegionLabel); value != "" {
		where = append(where, `cm.region_label=?`)
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.CameraModel); value != "" {
		where = append(where, `LOWER(cm.model)=LOWER(?)`)
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.SessionID); value != "" {
		where = append(where, `EXISTS(SELECT 1 FROM asset_shoot_sessions sf WHERE sf.asset_id=a.id AND sf.session_id=?)`)
		args = append(args, value)
	}
	if value := strings.TrimSpace(string(filter.Status)); value != "" {
		where = append(where, processingStatusSQL+`=?`)
		args = append(args, value)
	}
	if len(where) == 0 {
		return "", args
	}
	return ` WHERE ` + strings.Join(where, ` AND `), args
}

func (r *Repository) ListAssetCardsFiltered(ctx context.Context, filter domain.AssetCardFilter) ([]domain.AssetCard, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	query := `SELECT a.id, COALESCE(al.relative_path,''), a.state,` + processingStatusSQL + `,
COALESCE(m.duration_ms,0), COALESCE(m.orientation,''), COALESCE(cm.captured_at,m.captured_at),
COALESCE(an.summary,''), COALESCE(an.asset_type,''), COALESCE(an.camera_motion,''),
COALESCE(an.lighting,''), COALESCE(an.has_speech,0), COALESCE(an.quality,''),
COALESCE(an.usable_as_json,'[]'), COALESCE(an.mood_tags_json,'[]'), COALESCE(t.full_text,''),
COALESCE(cm.model,''), COALESCE(cm.region_label,''),
COALESCE((SELECT session_id FROM asset_shoot_sessions ass WHERE ass.asset_id=a.id ORDER BY ass.is_primary DESC LIMIT 1),''),
COALESCE(cm.source_color,''), COALESCE(cm.color_profile,''), COALESCE(cm.raw_format,''), COALESCE(cm.preview_status,'')
FROM assets a
LEFT JOIN asset_locations al ON al.asset_id=a.id AND al.is_primary=1
LEFT JOIN media_metadata m ON m.asset_id=a.id
LEFT JOIN capture_metadata cm ON cm.asset_id=a.id
LEFT JOIN asset_analysis an ON an.asset_id=a.id
LEFT JOIN transcripts t ON t.id=(SELECT id FROM transcripts t2 WHERE t2.asset_id=a.id AND t2.status='succeeded' ORDER BY t2.created_at DESC LIMIT 1)`
	where, args := assetBrowseWhere(filter)
	query += where
	query += ` ORDER BY COALESCE(cm.captured_at,m.captured_at,a.first_seen_at) DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AssetCard
	for rows.Next() {
		var card domain.AssetCard
		var captured sql.NullString
		var speech int
		var usable, moods string
		if err := rows.Scan(
			&card.ID, &card.Filename, &card.State, &card.ProcessingStatus, &card.DurationMS, &card.Orientation, &captured,
			&card.Summary, &card.AssetType, &card.CameraMotion, &card.Lighting, &speech, &card.Quality,
			&usable, &moods, &card.Transcript, &card.CameraModel, &card.RegionLabel, &card.SessionID,
			&card.SourceColor, &card.ColorProfile, &card.RawFormat, &card.PreviewStatus,
		); err != nil {
			return nil, err
		}
		card.Filename = filepath.Base(card.Filename)
		card.HasSpeech = speech != 0
		if captured.Valid {
			value, _ := time.Parse(time.RFC3339Nano, captured.String)
			card.CapturedAt = &value
		}
		_ = json.Unmarshal([]byte(usable), &card.UsableAs)
		_ = json.Unmarshal([]byte(moods), &card.MoodTags)
		out = append(out, card)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(out) == 0 {
		return out, nil
	}
	assetIDs := make([]string, len(out))
	for i, card := range out {
		assetIDs[i] = card.ID
	}
	haveArtifact, err := r.assetArtifactPresence(ctx, assetIDs, []string{"thumbnail", "proxy"})
	if err != nil {
		return nil, err
	}
	for i := range out {
		if haveArtifact[artifactKey{out[i].ID, "thumbnail"}] {
			out[i].ThumbnailURL = "/api/v1/assets/" + out[i].ID + "/thumbnail"
		}
		if haveArtifact[artifactKey{out[i].ID, "proxy"}] {
			out[i].ProxyURL = "/api/v1/assets/" + out[i].ID + "/proxy"
		}
	}
	return out, nil
}

type artifactKey struct {
	assetID string
	typ     string
}

// assetArtifactPresence answers "does asset X have an artifact of type Y" for
// a batch of assets and types in one query, instead of one GetArtifact call
// per (card, type) pair. ListAssetCardsFiltered only needs presence, not the
// artifact's fields, so this doesn't need to pick the newest row per
// (asset_id, artifact_type) the way GetArtifact does.
func (r *Repository) assetArtifactPresence(ctx context.Context, assetIDs, artifactTypes []string) (map[artifactKey]bool, error) {
	if len(assetIDs) == 0 || len(artifactTypes) == 0 {
		return map[artifactKey]bool{}, nil
	}
	assetPlaceholders := strings.TrimRight(strings.Repeat("?,", len(assetIDs)), ",")
	typePlaceholders := strings.TrimRight(strings.Repeat("?,", len(artifactTypes)), ",")
	query := `SELECT DISTINCT asset_id, artifact_type FROM derived_artifacts WHERE asset_id IN (` + assetPlaceholders + `) AND artifact_type IN (` + typePlaceholders + `)`
	args := make([]any, 0, len(assetIDs)+len(artifactTypes))
	for _, id := range assetIDs {
		args = append(args, id)
	}
	for _, typ := range artifactTypes {
		args = append(args, typ)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[artifactKey]bool, len(assetIDs)*len(artifactTypes))
	for rows.Next() {
		var key artifactKey
		if err := rows.Scan(&key.assetID, &key.typ); err != nil {
			return nil, err
		}
		out[key] = true
	}
	return out, rows.Err()
}
