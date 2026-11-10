package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// The read side of the multimodal store. Every query joins the active head, so
// a staged or failed replacement is invisible: an operator sees the generation
// whose data is canonical, not the newest attempt.

func scanIntervals(raw string) domain.IntervalSet {
	if raw == "" {
		return domain.IntervalSet{}
	}
	var out domain.IntervalSet
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return domain.IntervalSet{}
	}
	return out
}

// ListAudioEvents returns the active sound events overlapping the viewport.
func (r *Repository) ListAudioEvents(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AudioEventRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id,e.generation_id,e.asset_id,e.start_ms,e.end_ms,e.event_label,e.confidence,e.raw_score,
		       e.evidence_status,e.boundary_resolution_ms,
		       COALESCE(g.model_id,''),COALESCE(g.model_revision,'')
		FROM audio_events e
		JOIN modality_heads h ON h.generation_id=e.generation_id
		JOIN modality_generations g ON g.id=e.generation_id
		WHERE e.asset_id=? AND e.start_ms<? AND e.end_ms>?
		ORDER BY e.start_ms,e.end_ms,e.id`, assetID, endMS, startMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AudioEventRow{}
	for rows.Next() {
		var row domain.AudioEventRow
		var confidence *float64
		if err := rows.Scan(&row.ID, &row.GenerationID, &row.AssetID, &row.StartMS, &row.EndMS, &row.Label,
			&confidence, &row.RawScore, &row.EvidenceStatus, &row.BoundaryResolutionMS,
			&row.ModelID, &row.ModelVersion); err != nil {
			return nil, err
		}
		row.Confidence = confidence
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListAudioWindows returns the timing of active audio embedding windows.
func (r *Repository) ListAudioWindows(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AudioWindowRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT w.generation_id,w.start_ms,w.end_ms
		FROM audio_embeddings w
		JOIN modality_heads h ON h.generation_id=w.generation_id
		WHERE w.asset_id=? AND w.start_ms<? AND w.end_ms>?
		GROUP BY w.generation_id,w.start_ms,w.end_ms
		ORDER BY w.start_ms,w.end_ms`, assetID, endMS, startMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AudioWindowRow{}
	for rows.Next() {
		var row domain.AudioWindowRow
		if err := rows.Scan(&row.GenerationID, &row.StartMS, &row.EndMS); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListSpeakerTurns returns active diarization turns overlapping the viewport.
// Overlapping turns are preserved: two people talking at once is a fact.
func (r *Repository) ListSpeakerTurns(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.SpeakerTurnRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT t.id,t.generation_id,t.asset_id,t.start_ms,t.end_ms,COALESCE(t.local_speaker_id,''),t.confidence,t.overlap
		FROM speaker_turns t
		JOIN modality_heads h ON h.generation_id=t.generation_id
		WHERE t.asset_id=? AND t.start_ms<? AND t.end_ms>?
		ORDER BY t.start_ms,t.end_ms,t.id`, assetID, endMS, startMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.SpeakerTurnRow{}
	for rows.Next() {
		var row domain.SpeakerTurnRow
		var confidence *float64
		var overlap int
		if err := rows.Scan(&row.ID, &row.GenerationID, &row.AssetID, &row.StartMS, &row.EndMS, &row.LocalSpeakerID, &confidence, &overlap); err != nil {
			return nil, err
		}
		row.Confidence = confidence
		row.Overlap = overlap != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListVisualRefs returns the timing of active visual vectors. Vector bytes stay
// in the database: a timeline names the observation, it does not ship 768
// floats per frame.
func (r *Repository) ListVisualRefs(ctx context.Context, assetID string) ([]domain.VisualRef, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT v.shot_id,v.sample_index,v.timestamp_ms,s.start_ms,s.end_ms,v.strategy,v.space_id
		FROM visual_embeddings v
		JOIN modality_heads h ON h.generation_id=v.generation_id
		JOIN asset_shots s ON s.id=v.shot_id
		WHERE v.asset_id=?
		GROUP BY v.shot_id,v.sample_index,v.space_id
		ORDER BY s.start_ms,v.sample_index,v.space_id`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.VisualRef{}
	for rows.Next() {
		var row domain.VisualRef
		if err := rows.Scan(&row.ShotID, &row.SampleIndex, &row.TimestampMS, &row.StartMS, &row.EndMS, &row.Strategy, &row.SpaceID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListModalityCoverage returns one row per active operation/config head.
func (r *Repository) ListModalityCoverage(ctx context.Context, assetID string) ([]domain.ModalityCoverageRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT g.operation,g.state,g.processed_intervals_json,g.failure_intervals_json,
		       COALESCE(g.model_id,''),COALESCE(g.model_revision,''),g.config_hash,g.updated_at
		FROM modality_heads h
		JOIN modality_generations g ON g.id=h.generation_id
		WHERE h.asset_id=?
		ORDER BY g.operation,g.config_hash`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModalityCoverageRow{}
	for rows.Next() {
		var row domain.ModalityCoverageRow
		var processed, failed string
		if err := rows.Scan(&row.Operation, &row.State, &processed, &failed, &row.ModelID, &row.ModelVersion, &row.ConfigHash, &row.CommittedAt); err != nil {
			return nil, err
		}
		row.ProcessedIntervals = scanIntervals(processed)
		row.FailureIntervals = scanIntervals(failed)
		out = append(out, row)
	}
	return out, rows.Err()
}

// HasModalityHead reports whether a committed head exists for one
// (asset, operation, config) triple. Rebuild planning uses it to skip work that
// is already canonical under the current identity.
func (r *Repository) HasModalityHead(ctx context.Context, assetID string, operation domain.InferenceOperation, configHash string) (bool, error) {
	var one int
	err := r.db.QueryRowContext(ctx, `
		SELECT 1 FROM modality_heads WHERE asset_id=? AND operation=? AND config_hash=?`,
		assetID, string(operation), configHash).Scan(&one)
	if err != nil {
		return true, nil
	}
	return false, nil
}

// ModalityHeadStale reports whether the committed head was produced under a
// different input hash; the API reports that separately from "no data".
func (r *Repository) ModalityHeadStale(ctx context.Context, assetID string, operation domain.InferenceOperation, configHash, inputHash string) (bool, error) {
	var stored string
	err := r.db.QueryRowContext(ctx, `
		SELECT g.input_hash FROM modality_heads h JOIN modality_generations g ON g.id=h.generation_id
		WHERE h.asset_id=? AND h.operation=? AND h.config_hash=?`,
		assetID, string(operation), configHash).Scan(&stored)
	if err != nil {
		return false, err
	}
	return stored != inputHash, nil
}

// PurgeModalityGeneration marks a generation failed and removes its rows. It is
// used by rebuild/repair paths that must not leave half-written data behind.
func (r *Repository) PurgeModalityGeneration(ctx context.Context, generationID string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `UPDATE modality_generations SET state='failed',updated_at=? WHERE id=?`, now, generationID); err != nil {
		return err
	}
	for _, table := range []string{"audio_events", "audio_embeddings", "visual_embeddings", "speaker_turns"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE generation_id=?`, table), generationID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FilteredShotUniverse returns every canonical shot that satisfies the facet
// and asset-context filters. It deliberately does not require a semantic
// vector: a shot with no VLM description still exists, and a modality channel
// must be able to match it. Applying this set inside each channel before its
// top-K is what stops an unfiltered KNN limit from discarding eligible rows.
func (r *Repository) FilteredShotUniverse(ctx context.Context, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) (map[string]bool, error) {
	clauses, args := facetWhere(facets)
	clauses, args = appendShotDurationBounds(clauses, args, facets)
	joins := ` LEFT JOIN asset_analysis an ON an.asset_id=s.asset_id`
	if hasAssetContext(assetFilter) {
		ctxClauses, ctxArgs := assetContextClauses(assetFilter)
		clauses = append(clauses, ctxClauses...)
		args = append(args, ctxArgs...)
		joins += ` JOIN assets a ON a.id=s.asset_id LEFT JOIN media_metadata m ON m.asset_id=a.id LEFT JOIN capture_metadata cm ON cm.asset_id=a.id`
	}
	query := `SELECT s.id FROM asset_shots s` + joins
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	universe := make(map[string]bool, 1024)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		universe[id] = true
	}
	return universe, rows.Err()
}

// encodeQueryVector renders a query vector as the little-endian float32 blob
// sqlite-vec expects. It rejects a wrong or degenerate vector rather than
// letting SQLite compute a meaningless distance.
func encodeQueryVector(vector []float32, dimension int) ([]byte, error) {
	if len(vector) == 0 {
		return nil, fmt.Errorf("query vector is empty")
	}
	if dimension > 0 && len(vector) != dimension {
		return nil, fmt.Errorf("query vector has dimension %d, want %d", len(vector), dimension)
	}
	if !validVector(vector) {
		return nil, fmt.Errorf("query vector is not finite or has zero norm")
	}
	return encodeVectorBlob(vector), nil
}

// universeJSON renders a shot-id set for the SQL json_each membership filter.
// An empty (non-nil) map means "nothing is eligible" and is rendered as [].
func universeJSON(universe map[string]bool) string {
	ids := make([]string, 0, len(universe))
	for id := range universe {
		ids = append(ids, id)
	}
	encoded, _ := json.Marshal(ids)
	return string(encoded)
}

// VisualWindowMatches runs an exact cosine search inside one visual space,
// restricted to the eligible shot set and the active generation. Ordering is
// by distance then stable id, so equal distances never depend on row order.
func (r *Repository) VisualWindowMatches(ctx context.Context, spaceID string, query []float32, universe map[string]bool, limit int) ([]domain.ModalityMatch, error) {
	space, err := r.embeddingSpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	blob, err := encodeQueryVector(query, space.Dimension)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 200
	}
	sqlText := `
		SELECT v.id,v.shot_id,v.asset_id,s.start_ms,s.end_ms,v.sample_index,
		       vec_distance_cosine(v.vector, ?) AS distance,
		       g.id,COALESCE(g.model_id,''),COALESCE(g.model_revision,''),COALESCE(g.backend,'')
		FROM visual_embeddings v
		JOIN modality_heads h ON h.generation_id=v.generation_id
		JOIN modality_generations g ON g.id=v.generation_id
		JOIN asset_shots s ON s.id=v.shot_id
		WHERE v.space_id=?`
	args := []any{blob, spaceID}
	if universe != nil {
		sqlText += ` AND v.shot_id IN (SELECT value FROM json_each(?))`
		args = append(args, universeJSON(universe))
	}
	sqlText += ` ORDER BY distance ASC, v.shot_id ASC, v.sample_index ASC, v.id ASC`
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModalityMatch{}
	for rows.Next() {
		var id int64
		var shotID, assetID, generationID, modelID, modelRevision, backend string
		var startMS, endMS int64
		var sampleIndex int
		var distance float64
		if err := rows.Scan(&id, &shotID, &assetID, &startMS, &endMS, &sampleIndex, &distance,
			&generationID, &modelID, &modelRevision, &backend); err != nil {
			return nil, err
		}
		out = append(out, domain.ModalityMatch{
			RecordID:       fmt.Sprintf("visual-%d", id),
			AssetID:        assetID,
			ShotID:         shotID,
			Modality:       domain.ModalityVisual,
			Label:          fmt.Sprintf("sample:%d", sampleIndex),
			StartMS:        startMS,
			EndMS:          endMS,
			Score:          1 - distance,
			EvidenceStatus: "possible",
			ModelID:        modelID,
			ModelRevision:  modelRevision,
			Backend:        backend,
			GenerationID:   generationID,
		})
	}
	return out, rows.Err()
}

// AudioWindowMatches runs the same exact search over audio windows.
func (r *Repository) AudioWindowMatches(ctx context.Context, spaceID string, query []float32, universe map[string]bool, limit int) ([]domain.ModalityMatch, error) {
	space, err := r.embeddingSpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	blob, err := encodeQueryVector(query, space.Dimension)
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 200
	}
	sqlText := `
		SELECT e.id,e.asset_id,e.start_ms,e.end_ms,
		       vec_distance_cosine(e.vector, ?) AS distance,
		       g.id,COALESCE(g.model_id,''),COALESCE(g.model_revision,''),COALESCE(g.backend,'')
		FROM audio_embeddings e
		JOIN modality_heads h ON h.generation_id=e.generation_id
		JOIN modality_generations g ON g.id=e.generation_id
		WHERE e.space_id=?`
	args := []any{blob, spaceID}
	if universe != nil {
		sqlText += ` AND e.asset_id IN (SELECT s.asset_id FROM asset_shots s WHERE s.id IN (SELECT value FROM json_each(?)))`
		args = append(args, universeJSON(universe))
	}
	sqlText += ` ORDER BY distance ASC, e.asset_id ASC, e.start_ms ASC, e.id ASC`
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModalityMatch{}
	for rows.Next() {
		var id int64
		var assetID, generationID, modelID, modelRevision, backend string
		var startMS, endMS int64
		var distance float64
		if err := rows.Scan(&id, &assetID, &startMS, &endMS, &distance, &generationID, &modelID, &modelRevision, &backend); err != nil {
			return nil, err
		}
		out = append(out, domain.ModalityMatch{
			RecordID:       fmt.Sprintf("audio-%d", id),
			AssetID:        assetID,
			Modality:       domain.ModalityAudio,
			StartMS:        startMS,
			EndMS:          endMS,
			Score:          1 - distance,
			EvidenceStatus: "possible",
			ModelID:        modelID,
			ModelRevision:  modelRevision,
			Backend:        backend,
			GenerationID:   generationID,
		})
	}
	return out, rows.Err()
}

// SoundEventMatches returns active sound events whose canonical class id is in
// labels, restricted to the eligible shots' assets.
func (r *Repository) SoundEventMatches(ctx context.Context, labels []string, universe map[string]bool, limit int) ([]domain.ModalityMatch, error) {
	if len(labels) == 0 {
		return []domain.ModalityMatch{}, nil
	}
	if limit == 0 {
		limit = 200
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(labels)), ",")
	args := make([]any, 0, len(labels)+2)
	for _, label := range labels {
		args = append(args, label)
	}
	sqlText := `
		SELECT e.id,e.asset_id,e.start_ms,e.end_ms,e.event_label,e.raw_score,e.confidence,e.evidence_status,
		       g.id,COALESCE(g.model_id,''),COALESCE(g.model_revision,''),COALESCE(g.backend,'')
		FROM audio_events e
		JOIN modality_heads h ON h.generation_id=e.generation_id
		JOIN modality_generations g ON g.id=e.generation_id
		WHERE e.event_label IN (` + placeholders + `)`
	if universe != nil {
		sqlText += ` AND e.asset_id IN (SELECT s.asset_id FROM asset_shots s WHERE s.id IN (SELECT value FROM json_each(?)))`
		args = append(args, universeJSON(universe))
	}
	sqlText += ` ORDER BY e.raw_score DESC, e.asset_id ASC, e.start_ms ASC, e.id ASC`
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModalityMatch{}
	for rows.Next() {
		var id, assetID, label, evidence, generationID, modelID, modelRevision, backend string
		var startMS, endMS int64
		var score float64
		var confidence *float64
		if err := rows.Scan(&id, &assetID, &startMS, &endMS, &label, &score, &confidence, &evidence,
			&generationID, &modelID, &modelRevision, &backend); err != nil {
			return nil, err
		}
		out = append(out, domain.ModalityMatch{
			RecordID:       "event-" + id,
			AssetID:        assetID,
			Modality:       domain.ModalitySoundEvent,
			Label:          label,
			StartMS:        startMS,
			EndMS:          endMS,
			Score:          score,
			EvidenceStatus: evidence,
			Confidence:     confidence,
			ModelID:        modelID,
			ModelRevision:  modelRevision,
			Backend:        backend,
			GenerationID:   generationID,
		})
	}
	return out, rows.Err()
}

// SpeakerTurnMatches returns active turns for one local speaker identity.
func (r *Repository) SpeakerTurnMatches(ctx context.Context, assetID, generationID, localSpeakerID string, universe map[string]bool, limit int) ([]domain.ModalityMatch, error) {
	if limit == 0 {
		limit = 200
	}
	sqlText := `
		SELECT t.id,t.asset_id,t.start_ms,t.end_ms,COALESCE(t.local_speaker_id,''),t.confidence,t.overlap,
		       g.id,COALESCE(g.model_id,''),COALESCE(g.model_revision,''),COALESCE(g.backend,'')
		FROM speaker_turns t
		JOIN modality_heads h ON h.generation_id=t.generation_id
		JOIN modality_generations g ON g.id=t.generation_id
		WHERE t.generation_id=? AND t.local_speaker_id=?`
	args := []any{generationID, localSpeakerID}
	if assetID != "" {
		sqlText += ` AND t.asset_id=?`
		args = append(args, assetID)
	}
	if universe != nil {
		sqlText += ` AND t.asset_id IN (SELECT s.asset_id FROM asset_shots s WHERE s.id IN (SELECT value FROM json_each(?)))`
		args = append(args, universeJSON(universe))
	}
	sqlText += ` ORDER BY t.asset_id ASC, t.start_ms ASC, t.id ASC`
	if limit > 0 {
		sqlText += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModalityMatch{}
	for rows.Next() {
		var id, turnAsset, speaker, generationIDValue, modelID, modelRevision, backend string
		var startMS, endMS int64
		var confidence *float64
		var overlap int
		if err := rows.Scan(&id, &turnAsset, &startMS, &endMS, &speaker, &confidence, &overlap,
			&generationIDValue, &modelID, &modelRevision, &backend); err != nil {
			return nil, err
		}
		out = append(out, domain.ModalityMatch{
			RecordID:       "turn-" + id,
			AssetID:        turnAsset,
			Modality:       domain.ModalitySpeaker,
			Label:          speaker,
			StartMS:        startMS,
			EndMS:          endMS,
			Score:          1,
			EvidenceStatus: "possible",
			Confidence:     confidence,
			ModelID:        modelID,
			ModelRevision:  modelRevision,
			Backend:        backend,
			GenerationID:   generationIDValue,
		})
	}
	return out, rows.Err()
}

// embeddingSpace loads one space by id.
func (r *Repository) embeddingSpace(ctx context.Context, spaceID string) (domain.EmbeddingSpace, error) {
	var space domain.EmbeddingSpace
	err := r.db.QueryRowContext(ctx, `
		SELECT id,modality,provider,model_id,model_revision,dimension,normalization,preprocessing_json,runtime_digest
		FROM embedding_spaces WHERE id=?`, spaceID).
		Scan(&space.ID, &space.Modality, &space.Provider, &space.ModelID, &space.ModelRevision,
			&space.Dimension, &space.Normalization, &space.PreprocessingJSON, &space.RuntimeDigest)
	return space, err
}
