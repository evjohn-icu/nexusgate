package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
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
