package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/idgen"
)

func collectionToAssetCardFilter(filter domain.AssetCollectionFilter) domain.AssetCardFilter {
	return domain.AssetCardFilter{
		CapturedFrom: filter.CapturedFrom,
		CapturedTo:   filter.CapturedTo,
		RegionLabel:  filter.RegionLabel,
		CameraModel:  filter.CameraModel,
		SessionID:    filter.SessionID,
		Status:       filter.Status,
		Facets:       filter.FacetFilter,
	}
}

func (r *Repository) SaveAssetCollection(ctx context.Context, collection domain.AssetCollection) (domain.AssetCollection, error) {
	collection.Name = strings.TrimSpace(collection.Name)
	if collection.Name == "" {
		return domain.AssetCollection{}, fmt.Errorf("collection name is required")
	}
	if len(collection.Name) > 200 {
		return domain.AssetCollection{}, fmt.Errorf("collection name is too long")
	}
	if len(collection.Description) > 2000 {
		return domain.AssetCollection{}, fmt.Errorf("collection description is too long")
	}
	if collection.ID == "" {
		collection.ID = idgen.New()
	}
	now := time.Now().UTC()
	if collection.CreatedAt.IsZero() {
		var existingCreated string
		err := r.db.QueryRowContext(ctx, `SELECT created_at FROM asset_collections WHERE id=?`, collection.ID).Scan(&existingCreated)
		switch {
		case err == nil:
			collection.CreatedAt = parseStoredTimeString(existingCreated, "asset_collections.created_at")
		case errors.Is(err, sql.ErrNoRows):
			collection.CreatedAt = now
		default:
			return domain.AssetCollection{}, err
		}
	}
	if collection.UpdatedAt.IsZero() {
		collection.UpdatedAt = now
	}
	encoded, err := json.Marshal(collection.Filter)
	if err != nil {
		return domain.AssetCollection{}, fmt.Errorf("encode collection filter: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `INSERT INTO asset_collections(id,name,description,filter_json,created_at,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name,description=excluded.description,filter_json=excluded.filter_json,updated_at=excluded.updated_at`,
		collection.ID, collection.Name, collection.Description, string(encoded), formatTime(collection.CreatedAt), formatTime(collection.UpdatedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") && strings.Contains(err.Error(), "asset_collections.name") {
			return domain.AssetCollection{}, fmt.Errorf("%w: %s", domain.ErrCollectionExists, collection.Name)
		}
		return domain.AssetCollection{}, err
	}
	return collection, nil
}

func (r *Repository) ListAssetCollections(ctx context.Context) ([]domain.CollectionSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.id,c.name,c.description,c.filter_json,c.created_at,c.updated_at,COUNT(cs.shot_id),COALESCE(SUM(s.end_ms-s.start_ms),0)
FROM asset_collections c
LEFT JOIN collection_shots cs ON cs.collection_id=c.id
LEFT JOIN asset_shots s ON s.id=cs.shot_id
GROUP BY c.id ORDER BY c.name COLLATE NOCASE,c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	collections := make([]domain.CollectionSummary, 0)
	for rows.Next() {
		collection, err := scanCollectionSummary(rows)
		if err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	return collections, rows.Err()
}

func (r *Repository) GetAssetCollection(ctx context.Context, id string) (*domain.CollectionSummary, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `SELECT c.id,c.name,c.description,c.filter_json,c.created_at,c.updated_at,COUNT(cs.shot_id),COALESCE(SUM(s.end_ms-s.start_ms),0)
FROM asset_collections c
LEFT JOIN collection_shots cs ON cs.collection_id=c.id
LEFT JOIN asset_shots s ON s.id=cs.shot_id
WHERE c.id=? GROUP BY c.id`, id)
	collection, err := scanCollectionSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &collection, nil
}

func (r *Repository) DeleteAssetCollection(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("collection id is required")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM asset_collections WHERE id=?`, id)
	return err
}

func (r *Repository) ListAssetCardsInCollection(ctx context.Context, collectionID string, limit, offset int) ([]domain.AssetCard, error) {
	collection, err := r.GetAssetCollection(ctx, collectionID)
	if err != nil {
		return nil, err
	}
	if collection == nil {
		return nil, nil
	}
	// collectionToAssetCardFilter carries the facets across, so a saved
	// collection narrows its cards the same way its summary does — the two
	// must never describe different sets of assets.
	cardFilter := collectionToAssetCardFilter(collection.Filter)
	cardFilter.Limit = limit
	cardFilter.Offset = offset
	return r.ListAssetCardsFiltered(ctx, cardFilter)
}

func (r *Repository) GetAssetProcessingSummary(ctx context.Context, filter domain.AssetCollectionFilter) (domain.AssetProcessingSummary, error) {
	cardFilter := collectionToAssetCardFilter(filter)
	where, args := assetBrowseWhere(cardFilter)
	// assetBrowseWhere emits facet predicates against the alias "an", which is
	// why the analysis join below is needed at all. It must be a LEFT JOIN: an
	// inner join would silently drop every asset with no asset_analysis row —
	// in a freshly scanned library, nearly all of them — and the count strip
	// would read near-zero with no filter applied at all.
	query := `SELECT ` + processingStatusSQL + ` AS processing_status,COUNT(*)
FROM assets a
LEFT JOIN media_metadata m ON m.asset_id=a.id
LEFT JOIN capture_metadata cm ON cm.asset_id=a.id
LEFT JOIN asset_analysis an ON an.asset_id=a.id` + where + ` GROUP BY processing_status`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return domain.AssetProcessingSummary{ByStatus: map[domain.ProcessingStatus]int{}}, err
	}
	defer rows.Close()
	summary := domain.AssetProcessingSummary{ByStatus: map[domain.ProcessingStatus]int{
		domain.ProcessingStatusDiscovered: 0,
		domain.ProcessingStatusQueued:     0,
		domain.ProcessingStatusProcessing: 0,
		domain.ProcessingStatusReady:      0,
		domain.ProcessingStatusFailed:     0,
		domain.ProcessingStatusMissing:    0,
	}}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return domain.AssetProcessingSummary{}, err
		}
		summary.ByStatus[domain.ProcessingStatus(status)] = count
		summary.Total += count
	}
	if err := rows.Err(); err != nil {
		return domain.AssetProcessingSummary{}, err
	}
	return summary, nil
}

type collectionScanner interface {
	Scan(dest ...any) error
}

func scanCollectionSummary(scanner collectionScanner) (domain.CollectionSummary, error) {
	var collection domain.CollectionSummary
	var rawFilter, createdAt, updatedAt string
	if err := scanner.Scan(&collection.ID, &collection.Name, &collection.Description, &rawFilter, &createdAt, &updatedAt, &collection.ShotCount, &collection.TotalDurationMS); err != nil {
		return domain.CollectionSummary{}, err
	}
	if err := json.Unmarshal([]byte(rawFilter), &collection.Filter); err != nil {
		return domain.CollectionSummary{}, fmt.Errorf("decode collection filter: %w", err)
	}
	collection.CreatedAt = parseStoredTimeString(createdAt, "asset_collections.created_at")
	collection.UpdatedAt = parseStoredTimeString(updatedAt, "asset_collections.updated_at")
	return collection, nil
}

// AddShotToCollection pins a shot into the collection's basket. The shot must
// exist in asset_shots; position is appended after the current tail. Adding a
// shot that is already in the collection is a no-op (the primary key absorbs
// it) and returns nil.
func (r *Repository) AddShotToCollection(ctx context.Context, collectionID, shotID string) error {
	var exists int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM asset_shots WHERE id=?`, shotID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: shot %s does not exist", domain.ErrShotNotFound, shotID)
		}
		return err
	}
	// INSERT OR IGNORE keeps the call idempotent: re-adding an already pinned
	// shot is absorbed by the primary key, and the MAX(position) select over a
	// collection with no rows yields NULL, which COALESCE turns into position 0.
	_, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO collection_shots(collection_id,shot_id,position,created_at)
SELECT ?,?,COALESCE(MAX(position),-1)+1,?
FROM collection_shots WHERE collection_id=?`, collectionID, shotID, formatTime(time.Now().UTC()), collectionID)
	return err
}

func (r *Repository) RemoveShotFromCollection(ctx context.Context, collectionID, shotID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM collection_shots WHERE collection_id=? AND shot_id=?`, collectionID, shotID)
	return err
}

// ListCollectionShots returns the pinned shots in display order, joined with
// the owning asset's name and the shot fields a basket needs.
func (r *Repository) ListCollectionShots(ctx context.Context, collectionID string) ([]domain.CollectionShotDetail, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT cs.collection_id,cs.shot_id,cs.position,cs.created_at,s.asset_id,s.start_ms,s.end_ms,s.description,s.objects_json,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),'')
FROM collection_shots cs
JOIN asset_shots s ON s.id=cs.shot_id
WHERE cs.collection_id=? ORDER BY cs.position,cs.shot_id`, collectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.CollectionShotDetail, 0)
	for rows.Next() {
		var detail domain.CollectionShotDetail
		var createdAt, objects string
		if err := rows.Scan(&detail.CollectionID, &detail.ShotID, &detail.Position, &createdAt, &detail.AssetID, &detail.StartMS, &detail.EndMS, &detail.Description, &objects, &detail.Filename); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(objects), &detail.Objects)
		detail.CreatedAt = parseStoredTimeString(createdAt, "collection_shots.created_at")
		out = append(out, detail)
	}
	return out, rows.Err()
}

// ReorderCollectionShots assigns display positions from the given list. The
// list must be exactly the collection's current pinned shots: a length
// mismatch or an id that is not in the collection rejects the whole reorder,
// so a stale client can never silently drop or inject shots.
func (r *Repository) ReorderCollectionShots(ctx context.Context, collectionID string, shotIDs []string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT shot_id FROM collection_shots WHERE collection_id=?`, collectionID)
	if err != nil {
		return err
	}
	current := make(map[string]struct{})
	for rows.Next() {
		var shotID string
		if err := rows.Scan(&shotID); err != nil {
			rows.Close()
			return err
		}
		current[shotID] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(shotIDs) != len(current) {
		return fmt.Errorf("%w: reorder needs exactly the collection's %d shots, got %d", domain.ErrReorderInvalid, len(current), len(shotIDs))
	}
	// A duplicate id in the list passes both the length and the membership
	// checks while silently stranding another shot at its old position — the
	// client's intended order would be dropped without an error. The distinct
	// count is the cheap way to reject it.
	distinct := make(map[string]struct{}, len(shotIDs))
	for _, shotID := range shotIDs {
		distinct[shotID] = struct{}{}
	}
	if len(distinct) != len(shotIDs) {
		return fmt.Errorf("%w: reorder list contains duplicates", domain.ErrReorderInvalid)
	}
	for i, shotID := range shotIDs {
		if _, ok := current[shotID]; !ok {
			return fmt.Errorf("%w: shot %s is not in collection %s", domain.ErrReorderInvalid, shotID, collectionID)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collection_shots SET position=? WHERE collection_id=? AND shot_id=?`, i, collectionID, shotID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
