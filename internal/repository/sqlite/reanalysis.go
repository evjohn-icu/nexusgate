package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/idgen"
)

// EnqueueReanalysis forces the analyze stage to run again for an asset whose
// canonical analysis is already committed. Prompt/schema/validator changes
// must not require deleting the library: the old model run stays immutable
// and auditable, this call records the operator's request, and the new job's
// input hash is nonced so it can never collide with the succeeded job's
// (INSERT OR IGNORE dedup would otherwise swallow the reanalysis wholesale).
// When the new job runs, CreateModelRun's own dedup key differs on input
// hash, so a fresh run is produced and CommitAnalysisWithShots switches the
// canonical rows to it; JobIndex then rebuilds FTS and vectors.
func (r *Repository) EnqueueReanalysis(ctx context.Context, assetID, reason string) error {
	// Analyze requires the proxy and the media metadata; the transcript is
	// optional (the stage handles a nil transcript, and an untimed one is
	// withheld in split mode anyway). Requiring both up front keeps a doomed
	// job — one that would permanently fail before ever calling a provider —
	// out of the queue.
	proxy, err := r.GetArtifact(ctx, assetID, "proxy")
	if err != nil {
		return err
	}
	metadata, err := r.GetMediaMetadata(ctx, assetID)
	if err != nil {
		return err
	}
	if proxy == nil || metadata == nil {
		return fmt.Errorf("asset %s has no proxy artifact or media metadata; reanalysis has nothing to run on", assetID)
	}
	hash := reanalysisHash(assetID, reason)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := formatTime(time.Now().UTC())
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,?,'pending',30,0,3,?,?,?,?)`,
		idgen.New(), assetID, string(domain.JobAnalyze), formatTime(time.Now()), hash, now, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reanalysis_requests(id,asset_id,reason,input_hash,created_at) VALUES(?,?,?,?,?)`,
		idgen.New(), assetID, reason, hash, now); err != nil {
		return err
	}
	return tx.Commit()
}

// reanalysisHash mirrors app.hashStrings without importing app: the input
// hash is the run identity that must break every dedup, so it includes the
// operator's stated reason and a per-invocation nonce.
func reanalysisHash(assetID, reason string) string {
	h := sha256.New()
	for _, s := range []string{"reanalysis-v1", assetID, reason, idgen.New()} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
