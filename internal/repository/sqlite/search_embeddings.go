package sqlite

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/search"
)

// Text-embedding storage. These rows are the derived retrieval layer (see the
// migration's doc comment): rebuildable from canonical shot text, replaceable
// by model. Vectors are float32 little-endian blobs because embedding
// dimensions make JSON storage too slow for a full-library cosine scan.

// UpsertShotTextEmbeddings writes the batch under each row's model,
// replacing any prior vector for the same shot.
func (r *Repository) UpsertShotTextEmbeddings(ctx context.Context, rows []search.ShotEmbeddingRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := formatTime(time.Now().UTC())
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO shot_text_embeddings(shot_id,model,vector_blob,source_text_hash,created_at) VALUES(?,?,?,?,?) ON CONFLICT(shot_id) DO UPDATE SET model=excluded.model,vector_blob=excluded.vector_blob,source_text_hash=excluded.source_text_hash,created_at=excluded.created_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range rows {
		if row.Shot.ID == "" || row.Model == "" || !validVector(row.Vector) {
			return fmt.Errorf("embedding row needs shot_id and model")
		}
		if _, err := stmt.ExecContext(ctx, row.Shot.ID, row.Model, encodeVectorBlob(row.Vector), row.SourceTextHash, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListShotTextEmbeddings returns every vector of one model joined with its
// full shot row, in deterministic (shot_id) order, for the engine's cosine
// scan. The model filter is the replacement boundary: vectors written under a
// superseded model never get scored against a query embedded by the current
// one.
func (r *Repository) ListShotTextEmbeddings(ctx context.Context, model string) ([]search.ShotEmbeddingRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),''),e.model,e.vector_blob,e.source_text_hash FROM shot_text_embeddings e JOIN asset_shots s ON s.id=e.shot_id WHERE e.model=? ORDER BY e.shot_id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.ShotEmbeddingRow
	for rows.Next() {
		var row search.ShotEmbeddingRow
		var tags, objects, actions, mood, created, filename string
		var blob []byte
		if err := rows.Scan(&row.Shot.ID, &row.Shot.AssetID, &row.Shot.SourceRunID, &row.Shot.Ordinal, &row.Shot.StartMS, &row.Shot.EndMS, &row.Shot.Description, &tags, &objects, &actions, &mood, &row.Shot.Confidence, &created, &filename, &row.Model, &blob, &row.SourceTextHash); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &row.Shot.Tags)
		_ = json.Unmarshal([]byte(objects), &row.Shot.Objects)
		_ = json.Unmarshal([]byte(actions), &row.Shot.Actions)
		_ = json.Unmarshal([]byte(mood), &row.Shot.Mood)
		if filename != "" {
			row.Shot.Filename = filepath.Base(filename)
		}
		row.Vector, err = decodeVectorBlob(blob)
		if err != nil {
			return nil, fmt.Errorf("decode embedding for shot %s: %w", row.Shot.ID, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// AllShotTextDocuments returns the derived text projection of every shot.
// The projection is exactly what the embedding source text is built from
// (description + tags + objects + actions + mood); transcript is deliberately
// absent — speech retrieval belongs to the transcript channel.
func (r *Repository) AllShotTextDocuments(ctx context.Context) ([]search.ShotSearchDocument, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,description,tags_json,objects_json,actions_json,mood_json FROM asset_shots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.ShotSearchDocument
	for rows.Next() {
		var doc search.ShotSearchDocument
		var tags, objects, actions, mood string
		if err := rows.Scan(&doc.ShotID, &doc.Description, &tags, &objects, &actions, &mood); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &doc.Tags)
		_ = json.Unmarshal([]byte(objects), &doc.Objects)
		_ = json.Unmarshal([]byte(actions), &doc.Actions)
		_ = json.Unmarshal([]byte(mood), &doc.Mood)
		out = append(out, doc)
	}
	return out, rows.Err()
}

// ShotTextDocumentsByAsset returns the derived text projection of one
// asset's shots — the incremental-rebuild input for the post-commit hook.
func (r *Repository) ShotTextDocumentsByAsset(ctx context.Context, assetID string) ([]search.ShotSearchDocument, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,description,tags_json,objects_json,actions_json,mood_json FROM asset_shots WHERE asset_id=? ORDER BY ordinal`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []search.ShotSearchDocument
	for rows.Next() {
		var doc search.ShotSearchDocument
		var tags, objects, actions, mood string
		if err := rows.Scan(&doc.ShotID, &doc.Description, &tags, &objects, &actions, &mood); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &doc.Tags)
		_ = json.Unmarshal([]byte(objects), &doc.Objects)
		_ = json.Unmarshal([]byte(actions), &doc.Actions)
		_ = json.Unmarshal([]byte(mood), &doc.Mood)
		out = append(out, doc)
	}
	return out, rows.Err()
}

// ShotTextEmbeddingHashes returns shot_id -> source_text_hash for one asset
// under one model, so the post-commit hook embeds only what changed.
func (r *Repository) ShotTextEmbeddingHashes(ctx context.Context, model, assetID string) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT shot_id,source_text_hash FROM shot_text_embeddings WHERE model=? AND shot_id IN (SELECT id FROM asset_shots WHERE asset_id=?)`, model, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var shotID, hash string
		if err := rows.Scan(&shotID, &hash); err != nil {
			return nil, err
		}
		out[shotID] = hash
	}
	return out, rows.Err()
}

// encodeVectorBlob serializes float32 vectors (the persisted form) as
// little-endian. float32 halves scan volume vs the provider's float64 output
// with no measurable retrieval difference at library scale.
func encodeVectorBlob(vector []float32) []byte {
	blob := make([]byte, 4*len(vector))
	for i, v := range vector {
		binary.LittleEndian.PutUint32(blob[4*i:], math.Float32bits(v))
	}
	return blob
}

func decodeVectorBlob(blob []byte) ([]float32, error) {
	if len(blob)%4 != 0 {
		return nil, fmt.Errorf("vector blob length %d is not divisible by 4", len(blob))
	}
	vector := make([]float32, len(blob)/4)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[4*i:]))
	}
	if !validVector(vector) {
		return nil, fmt.Errorf("vector blob contains non-finite or zero-norm values")
	}
	return vector, nil
}

func validVector(vector []float32) bool {
	if len(vector) == 0 {
		return false
	}
	var norm float64
	for _, value := range vector {
		v := float64(value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
		norm += v * v
	}
	return !math.IsNaN(norm) && !math.IsInf(norm, 0) && norm > 0
}
