package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

const processingStatusSQL = `CASE
WHEN a.state='missing' THEN 'missing'
WHEN EXISTS (SELECT 1 FROM jobs jrunning WHERE jrunning.asset_id=a.id AND jrunning.state='running') THEN 'processing'
WHEN EXISTS (SELECT 1 FROM jobs jqueued WHERE jqueued.asset_id=a.id AND jqueued.state='pending') THEN 'queued'
WHEN EXISTS (SELECT 1 FROM jobs jfailed WHERE jfailed.asset_id=a.id AND jfailed.state='failed') THEN 'failed'
WHEN EXISTS (SELECT 1 FROM derived_artifacts dready WHERE dready.asset_id=a.id AND dready.artifact_type='proxy') THEN 'ready'
ELSE 'discovered' END`

func assetBrowseWhere(filter domain.AssetCardFilter) (string, []any) {
	ctxFilter := domain.AssetContextFilter{
		CapturedFrom: filter.CapturedFrom,
		CapturedTo:   filter.CapturedTo,
		RegionLabel:  filter.RegionLabel,
		CameraModel:  filter.CameraModel,
		SessionID:    filter.SessionID,
		Status:       filter.Status,
	}
	where, args := assetContextClauses(ctxFilter)
	facetClauses, facetArgs := assetFacetWhereClauses(filter.Facets)
	where = append(where, facetClauses...)
	args = append(args, facetArgs...)
	if len(filter.IDs) > 0 {
		where = append(where, `a.id IN (`+strings.TrimRight(strings.Repeat(`?,`, len(filter.IDs)), `,`)+`)`)
		for _, id := range filter.IDs {
			args = append(args, id)
		}
	}
	if len(where) == 0 {
		return "", args
	}
	return ` WHERE ` + strings.Join(where, ` AND `), args
}

// assetContextClauses builds the capture/session/status predicates shared by
// asset browse (assetBrowseWhere) and the Search v2 asset-context filter
// (ScoreCandidatesV2). This is the single definition of "ready" and of the
// capture interval: CapturedTo is the exclusive upper bound (<), the session
// check is an EXISTS over asset_shoot_sessions, and status derives from
// processingStatusSQL. The aliases are those of assetCardColumns (a, cm, m).
func assetContextClauses(filter domain.AssetContextFilter) ([]string, []any) {
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
	return where, args
}

// facetWhere builds the WHERE-clause fragments for the six normalize.*Values
// enum fields, assuming asset_analysis is joined under the alias "an". It is
// shared by the asset browse query above and the shot-search queries in
// repository.go — see the FacetFilter doc comment in
// internal/domain/asset_browse.go for why a shot-level search still resolves
// these through the shot's asset rather than a per-shot column.
//
// Values must already be validated against normalize's exported *Values
// lists by the caller (internal/app); this only assembles placeholders, it
// does not check membership, so an invalid value here would silently build a
// clause that matches nothing rather than reporting a typo.
func facetWhere(f domain.FacetFilter) ([]string, []any) {
	var clauses []string
	var args []any
	in := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		clauses = append(clauses, column+` IN (`+strings.TrimRight(strings.Repeat(`?,`, len(values)), `,`)+`)`)
		for _, v := range values {
			args = append(args, v)
		}
	}
	in(`an.asset_type`, f.AssetTypes)
	in(`an.shot_size`, f.ShotSizes)
	in(`an.camera_motion`, f.CameraMotions)
	in(`an.audio_type`, f.AudioTypes)
	in(`an.quality`, f.Qualities)
	// usable_as is a list per asset (usable_as_json), not a single enum, so
	// membership needs json_each rather than a plain column comparison: an
	// asset matches if any of its usable_as values is one of the requested
	// values.
	if len(f.UsableAs) > 0 {
		clauses = append(clauses, `EXISTS (SELECT 1 FROM json_each(an.usable_as_json) usable_as_je WHERE usable_as_je.value IN (`+strings.TrimRight(strings.Repeat(`?,`, len(f.UsableAs)), `,`)+`))`)
		for _, v := range f.UsableAs {
			args = append(args, v)
		}
	}
	return clauses, args
}

// assetFacetWhereClauses builds the complete asset-level facet predicate:
// facetWhere's six vocabulary clauses (against alias "an") plus the asset's
// own duration bounds (against alias "m", media_metadata.duration_ms). It is
// shared by assetBrowseWhere and Repository.SearchFiltered's EXISTS guard so
// the two entry points cannot drift on what an asset-level facet means.
//
// Duration here is the asset's own probed length, unlike the shot-search
// facet queries in repository.go where the same MinDurationMS/MaxDurationMS
// bound a single shot's span instead (appendShotDurationBounds) — see the
// FacetFilter doc comment in internal/domain/asset_browse.go for why that
// asymmetry is deliberate and must not be unified. NULL duration_ms (not yet
// probed) fails both comparisons under SQL's NULL semantics, so an unprobed
// asset is correctly excluded rather than treated as zero-length.
func assetFacetWhereClauses(f domain.FacetFilter) ([]string, []any) {
	clauses, args := facetWhere(f)
	if f.MinDurationMS != nil {
		clauses = append(clauses, `m.duration_ms>=?`)
		args = append(args, *f.MinDurationMS)
	}
	if f.MaxDurationMS != nil {
		clauses = append(clauses, `m.duration_ms<=?`)
		args = append(args, *f.MaxDurationMS)
	}
	return clauses, args
}

// appendShotDurationBounds adds the shot-span duration clauses used by the
// shot-search queries in repository.go, where MinDurationMS/MaxDurationMS
// bound an individual shot's (end_ms-start_ms) rather than the asset's total
// duration — see the FacetFilter doc comment in
// internal/domain/asset_browse.go. Both bounds are inclusive, matching
// assetBrowseWhere's asset-level duration clauses.
func appendShotDurationBounds(clauses []string, args []any, f domain.FacetFilter) ([]string, []any) {
	if f.MinDurationMS != nil {
		clauses = append(clauses, `(s.end_ms-s.start_ms)>=?`)
		args = append(args, *f.MinDurationMS)
	}
	if f.MaxDurationMS != nil {
		clauses = append(clauses, `(s.end_ms-s.start_ms)<=?`)
		args = append(args, *f.MaxDurationMS)
	}
	return clauses, args
}

// assetCardColumns is the shared SELECT column list + FROM/JOIN clause for
// building an AssetCard. Used by ListAssetCardsFiltered (batch browsing) and
// assetCardForShot (single-shot detail); keeping the 22 columns and the six
// joins in one place means adding a card field cannot silently drift between
// the two callers.
const assetCardColumns = `SELECT a.id, COALESCE(al.relative_path,''), a.state,` + processingStatusSQL + `,
COALESCE(m.duration_ms,0), COALESCE(m.orientation,''), COALESCE(cm.captured_at,m.captured_at),
COALESCE(an.summary,''), COALESCE(an.asset_type,''), COALESCE(an.camera_motion,''),
COALESCE(an.lighting,''), COALESCE(an.has_speech,0), COALESCE(an.quality,''),
COALESCE(an.usable_as_json,'[]'), COALESCE(an.mood_tags_json,'[]'), COALESCE(t.full_text,''),
COALESCE(cm.model,''), COALESCE(cm.region_label,''),
COALESCE((SELECT session_id FROM asset_shoot_sessions ass WHERE ass.asset_id=a.id ORDER BY ass.is_primary DESC LIMIT 1),''),
COALESCE(cm.source_color,''), COALESCE(cm.color_profile,''), COALESCE(cm.raw_format,''), COALESCE(cm.preview_status,'')
FROM assets a
LEFT JOIN asset_locations al ON al.id=(SELECT l.id FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=a.id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1)
LEFT JOIN media_metadata m ON m.asset_id=a.id
LEFT JOIN capture_metadata cm ON cm.asset_id=a.id
LEFT JOIN asset_analysis an ON an.asset_id=a.id
LEFT JOIN transcripts t ON t.id=(SELECT id FROM transcripts t2 WHERE t2.asset_id=a.id AND t2.status='succeeded' ORDER BY t2.created_at DESC LIMIT 1)`

// scanAssetCardRow scans one row of assetCardColumns into a domain.AssetCard,
// applying the same field post-processing (basename, has_speech bool, time
// parsing, JSON arrays) as the batch path so the two stay in lockstep.
func scanAssetCardRow(rows *sql.Rows) (domain.AssetCard, error) {
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
		return card, err
	}
	card.Filename = filepath.Base(card.Filename)
	card.HasSpeech = speech != 0
	if captured.Valid {
		value, _ := time.Parse(time.RFC3339Nano, captured.String)
		card.CapturedAt = &value
	}
	_ = json.Unmarshal([]byte(usable), &card.UsableAs)
	_ = json.Unmarshal([]byte(moods), &card.MoodTags)
	return card, nil
}

// ListAssetCardsFiltered returns the browse projection for the given filter,
// most recently captured first, with a deterministic id tie-break so paging
// cannot duplicate or drop rows. It is the shared workhorse behind the asset
// list, collections, and the v1 browser.
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
	query := assetCardColumns
	where, args := assetBrowseWhere(filter)
	query += where
	query += ` ORDER BY COALESCE(cm.captured_at,m.captured_at,a.first_seen_at) DESC, a.id DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return r.assetCardsFromRows(ctx, rows)
}

// PagedAssetCardsFiltered is the paged form of ListAssetCardsFiltered: the
// same filter and ordering, but one probe row past the requested page so the
// caller can tell "this page is full" from "this is the last page" without a
// second query. The API list handler is its only consumer; ListAssetCards and
// ListAssetCardsInCollection keep using the un-paged form.
func (r *Repository) PagedAssetCardsFiltered(ctx context.Context, filter domain.AssetCardFilter) ([]domain.AssetCard, bool, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	if filter.Limit > 500 {
		filter.Limit = 500
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	limit := filter.Limit
	filter.Limit = limit + 1
	query := assetCardColumns
	where, args := assetBrowseWhere(filter)
	query += where
	query += ` ORDER BY COALESCE(cm.captured_at,m.captured_at,a.first_seen_at) DESC, a.id DESC LIMIT ? OFFSET ?`
	args = append(args, filter.Limit, filter.Offset)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	cards, err := r.assetCardsFromRows(ctx, rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(cards) > limit
	if hasMore {
		cards = cards[:limit]
	}
	return cards, hasMore, nil
}

// assetCardsFromRows scans a fully-materialized asset-card result set and
// enriches it with thumbnail/proxy artifact presence in one batched query.
// Both ListAssetCardsFiltered and PagedAssetCardsFiltered share it so the two
// can never disagree about what a card row means.
func (r *Repository) assetCardsFromRows(ctx context.Context, rows *sql.Rows) ([]domain.AssetCard, error) {
	defer rows.Close()
	var out []domain.AssetCard
	for rows.Next() {
		card, err := scanAssetCardRow(rows)
		if err != nil {
			return nil, err
		}
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
