package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// GetValidatedModelRun returns staged vision output to the current holder of
// the corresponding asset's lease. A legacy stage can resume from running so
// its next validated publication includes the complete shot envelope.
func (r *Repository) GetValidatedModelRun(ctx context.Context, runID, jobID, owner string) (string, bool, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return "", false, err
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	var assetID, state, capability string
	var parsed sql.NullString
	err = conn.QueryRowContext(ctx, `SELECT asset_id,state,capability,CASE WHEN state='validated' THEN parsed_json END FROM model_runs WHERE id=?`, runID).Scan(&assetID, &state, &capability, &parsed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := leaseGuard(ctx, conn, jobID, assetID, owner); err != nil {
		return "", false, err
	}
	if state != "validated" || capability != "vision" {
		return "", false, nil
	}
	var envelope struct {
		Version string `json:"version"`
	}
	if json.Unmarshal([]byte(parsed.String), &envelope) == nil && envelope.Version == "vision_stage.v1" {
		return parsed.String, true, nil
	}
	if _, err := conn.ExecContext(ctx, `UPDATE model_runs SET state='running' WHERE id=? AND state='validated'`, runID); err != nil {
		return "", false, err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return "", false, err
}
