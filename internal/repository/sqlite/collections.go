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
			collection.CreatedAt = parseStoredTimeString(existingCreated)
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
		return domain.AssetCollection{}, err
	}
	return collection, nil
}

func (r *Repository) ListAssetCollections(ctx context.Context) ([]domain.AssetCollection, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,name,description,filter_json,created_at,updated_at FROM asset_collections ORDER BY name COLLATE NOCASE,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	collections := make([]domain.AssetCollection, 0)
	for rows.Next() {
		collection, err := scanAssetCollection(rows)
		if err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	return collections, rows.Err()
}

func (r *Repository) GetAssetCollection(ctx context.Context, id string) (*domain.AssetCollection, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `SELECT id,name,description,filter_json,created_at,updated_at FROM asset_collections WHERE id=?`, id)
	collection, err := scanAssetCollection(row)
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

func scanAssetCollection(scanner collectionScanner) (domain.AssetCollection, error) {
	var collection domain.AssetCollection
	var rawFilter, createdAt, updatedAt string
	if err := scanner.Scan(&collection.ID, &collection.Name, &collection.Description, &rawFilter, &createdAt, &updatedAt); err != nil {
		return domain.AssetCollection{}, err
	}
	if err := json.Unmarshal([]byte(rawFilter), &collection.Filter); err != nil {
		return domain.AssetCollection{}, fmt.Errorf("decode collection filter: %w", err)
	}
	collection.CreatedAt = parseStoredTimeString(createdAt)
	collection.UpdatedAt = parseStoredTimeString(updatedAt)
	return collection, nil
}
