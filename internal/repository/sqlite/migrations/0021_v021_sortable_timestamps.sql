-- formatTime previously emitted time.RFC3339Nano, whose .999999999 layout strips
-- trailing zeros. SQL compares these stored strings lexicographically (run_after<=?,
-- lease_expires_at<=?, ORDER BY created_at, ...), so a variable-width fraction made
-- lexicographic order disagree with chronological order -- golang/go#19635. The
-- writer now uses a fixed-width 30-character layout; these UPDATEs rewrite every
-- row written before that change to the same instant in the fixed-width form. A
-- correct value is exactly 30 characters, so the length<>30 guard makes the whole
-- file idempotent: re-running it is a no-op.

UPDATE alignment_runs SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE alignment_runs SET finished_at = CASE
  WHEN instr(finished_at, '.') = 0
    THEN substr(finished_at, 1, length(finished_at) - 1) || '.000000000Z'
  ELSE substr(finished_at, 1, instr(finished_at, '.') - 1)
    || '.' || substr(substr(finished_at, 1, length(finished_at) - 1) || '000000000',
                     instr(finished_at, '.') + 1, 9) || 'Z'
END
WHERE finished_at IS NOT NULL AND length(finished_at) <> 30;

UPDATE asset_analysis SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE asset_collections SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE asset_collections SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE asset_locations SET last_seen_at = CASE
  WHEN instr(last_seen_at, '.') = 0
    THEN substr(last_seen_at, 1, length(last_seen_at) - 1) || '.000000000Z'
  ELSE substr(last_seen_at, 1, instr(last_seen_at, '.') - 1)
    || '.' || substr(substr(last_seen_at, 1, length(last_seen_at) - 1) || '000000000',
                     instr(last_seen_at, '.') + 1, 9) || 'Z'
END
WHERE last_seen_at IS NOT NULL AND length(last_seen_at) <> 30;

UPDATE asset_overrides SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE asset_shoot_sessions SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE asset_shots SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE asset_tag_links SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE asset_tag_links SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE asset_tags SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE assets SET first_seen_at = CASE
  WHEN instr(first_seen_at, '.') = 0
    THEN substr(first_seen_at, 1, length(first_seen_at) - 1) || '.000000000Z'
  ELSE substr(first_seen_at, 1, instr(first_seen_at, '.') - 1)
    || '.' || substr(substr(first_seen_at, 1, length(first_seen_at) - 1) || '000000000',
                     instr(first_seen_at, '.') + 1, 9) || 'Z'
END
WHERE first_seen_at IS NOT NULL AND length(first_seen_at) <> 30;

UPDATE assets SET last_seen_at = CASE
  WHEN instr(last_seen_at, '.') = 0
    THEN substr(last_seen_at, 1, length(last_seen_at) - 1) || '.000000000Z'
  ELSE substr(last_seen_at, 1, instr(last_seen_at, '.') - 1)
    || '.' || substr(substr(last_seen_at, 1, length(last_seen_at) - 1) || '000000000',
                     instr(last_seen_at, '.') + 1, 9) || 'Z'
END
WHERE last_seen_at IS NOT NULL AND length(last_seen_at) <> 30;

UPDATE assets SET missing_since = CASE
  WHEN instr(missing_since, '.') = 0
    THEN substr(missing_since, 1, length(missing_since) - 1) || '.000000000Z'
  ELSE substr(missing_since, 1, instr(missing_since, '.') - 1)
    || '.' || substr(substr(missing_since, 1, length(missing_since) - 1) || '000000000',
                     instr(missing_since, '.') + 1, 9) || 'Z'
END
WHERE missing_since IS NOT NULL AND length(missing_since) <> 30;

UPDATE capture_evidence SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE capture_metadata SET captured_at = CASE
  WHEN instr(captured_at, '.') = 0
    THEN substr(captured_at, 1, length(captured_at) - 1) || '.000000000Z'
  ELSE substr(captured_at, 1, instr(captured_at, '.') - 1)
    || '.' || substr(substr(captured_at, 1, length(captured_at) - 1) || '000000000',
                     instr(captured_at, '.') + 1, 9) || 'Z'
END
WHERE captured_at IS NOT NULL AND length(captured_at) <> 30;

UPDATE capture_metadata SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE capture_sidecars SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE derived_artifacts SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE favorites SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE fts_index_state SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE job_events SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE jobs SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE jobs SET last_failure_at = CASE
  WHEN instr(last_failure_at, '.') = 0
    THEN substr(last_failure_at, 1, length(last_failure_at) - 1) || '.000000000Z'
  ELSE substr(last_failure_at, 1, instr(last_failure_at, '.') - 1)
    || '.' || substr(substr(last_failure_at, 1, length(last_failure_at) - 1) || '000000000',
                     instr(last_failure_at, '.') + 1, 9) || 'Z'
END
WHERE last_failure_at IS NOT NULL AND length(last_failure_at) <> 30;

UPDATE jobs SET lease_expires_at = CASE
  WHEN instr(lease_expires_at, '.') = 0
    THEN substr(lease_expires_at, 1, length(lease_expires_at) - 1) || '.000000000Z'
  ELSE substr(lease_expires_at, 1, instr(lease_expires_at, '.') - 1)
    || '.' || substr(substr(lease_expires_at, 1, length(lease_expires_at) - 1) || '000000000',
                     instr(lease_expires_at, '.') + 1, 9) || 'Z'
END
WHERE lease_expires_at IS NOT NULL AND length(lease_expires_at) <> 30;

UPDATE jobs SET run_after = CASE
  WHEN instr(run_after, '.') = 0
    THEN substr(run_after, 1, length(run_after) - 1) || '.000000000Z'
  ELSE substr(run_after, 1, instr(run_after, '.') - 1)
    || '.' || substr(substr(run_after, 1, length(run_after) - 1) || '000000000',
                     instr(run_after, '.') + 1, 9) || 'Z'
END
WHERE run_after IS NOT NULL AND length(run_after) <> 30;

UPDATE jobs SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE library_roots SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE library_roots SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE library_summaries SET generated_at = CASE
  WHEN instr(generated_at, '.') = 0
    THEN substr(generated_at, 1, length(generated_at) - 1) || '.000000000Z'
  ELSE substr(generated_at, 1, instr(generated_at, '.') - 1)
    || '.' || substr(substr(generated_at, 1, length(generated_at) - 1) || '000000000',
                     instr(generated_at, '.') + 1, 9) || 'Z'
END
WHERE generated_at IS NOT NULL AND length(generated_at) <> 30;

UPDATE media_metadata SET captured_at = CASE
  WHEN instr(captured_at, '.') = 0
    THEN substr(captured_at, 1, length(captured_at) - 1) || '.000000000Z'
  ELSE substr(captured_at, 1, instr(captured_at, '.') - 1)
    || '.' || substr(substr(captured_at, 1, length(captured_at) - 1) || '000000000',
                     instr(captured_at, '.') + 1, 9) || 'Z'
END
WHERE captured_at IS NOT NULL AND length(captured_at) <> 30;

UPDATE media_metadata SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE model_runs SET committed_at = CASE
  WHEN instr(committed_at, '.') = 0
    THEN substr(committed_at, 1, length(committed_at) - 1) || '.000000000Z'
  ELSE substr(committed_at, 1, instr(committed_at, '.') - 1)
    || '.' || substr(substr(committed_at, 1, length(committed_at) - 1) || '000000000',
                     instr(committed_at, '.') + 1, 9) || 'Z'
END
WHERE committed_at IS NOT NULL AND length(committed_at) <> 30;

UPDATE model_runs SET finished_at = CASE
  WHEN instr(finished_at, '.') = 0
    THEN substr(finished_at, 1, length(finished_at) - 1) || '.000000000Z'
  ELSE substr(finished_at, 1, instr(finished_at, '.') - 1)
    || '.' || substr(substr(finished_at, 1, length(finished_at) - 1) || '000000000',
                     instr(finished_at, '.') + 1, 9) || 'Z'
END
WHERE finished_at IS NOT NULL AND length(finished_at) <> 30;

UPDATE model_runs SET started_at = CASE
  WHEN instr(started_at, '.') = 0
    THEN substr(started_at, 1, length(started_at) - 1) || '.000000000Z'
  ELSE substr(started_at, 1, instr(started_at, '.') - 1)
    || '.' || substr(substr(started_at, 1, length(started_at) - 1) || '000000000',
                     instr(started_at, '.') + 1, 9) || 'Z'
END
WHERE started_at IS NOT NULL AND length(started_at) <> 30;

UPDATE provider_channel_events SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE provider_channel_members SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE provider_channel_members SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE provider_channels SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE provider_channels SET deleted_at = CASE
  WHEN instr(deleted_at, '.') = 0
    THEN substr(deleted_at, 1, length(deleted_at) - 1) || '.000000000Z'
  ELSE substr(deleted_at, 1, instr(deleted_at, '.') - 1)
    || '.' || substr(substr(deleted_at, 1, length(deleted_at) - 1) || '000000000',
                     instr(deleted_at, '.') + 1, 9) || 'Z'
END
WHERE deleted_at IS NOT NULL AND length(deleted_at) <> 30;

UPDATE provider_channels SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE provider_credential_leases SET expires_at = CASE
  WHEN instr(expires_at, '.') = 0
    THEN substr(expires_at, 1, length(expires_at) - 1) || '.000000000Z'
  ELSE substr(expires_at, 1, instr(expires_at, '.') - 1)
    || '.' || substr(substr(expires_at, 1, length(expires_at) - 1) || '000000000',
                     instr(expires_at, '.') + 1, 9) || 'Z'
END
WHERE expires_at IS NOT NULL AND length(expires_at) <> 30;

UPDATE provider_credential_leases SET issued_at = CASE
  WHEN instr(issued_at, '.') = 0
    THEN substr(issued_at, 1, length(issued_at) - 1) || '.000000000Z'
  ELSE substr(issued_at, 1, instr(issued_at, '.') - 1)
    || '.' || substr(substr(issued_at, 1, length(issued_at) - 1) || '000000000',
                     instr(issued_at, '.') + 1, 9) || 'Z'
END
WHERE issued_at IS NOT NULL AND length(issued_at) <> 30;

UPDATE provider_files SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE provider_files SET expires_at = CASE
  WHEN instr(expires_at, '.') = 0
    THEN substr(expires_at, 1, length(expires_at) - 1) || '.000000000Z'
  ELSE substr(expires_at, 1, instr(expires_at, '.') - 1)
    || '.' || substr(substr(expires_at, 1, length(expires_at) - 1) || '000000000',
                     instr(expires_at, '.') + 1, 9) || 'Z'
END
WHERE expires_at IS NOT NULL AND length(expires_at) <> 30;

UPDATE provider_files SET last_used_at = CASE
  WHEN instr(last_used_at, '.') = 0
    THEN substr(last_used_at, 1, length(last_used_at) - 1) || '.000000000Z'
  ELSE substr(last_used_at, 1, instr(last_used_at, '.') - 1)
    || '.' || substr(substr(last_used_at, 1, length(last_used_at) - 1) || '000000000',
                     instr(last_used_at, '.') + 1, 9) || 'Z'
END
WHERE last_used_at IS NOT NULL AND length(last_used_at) <> 30;

UPDATE repurpose_plan_revisions SET approved_at = CASE
  WHEN instr(approved_at, '.') = 0
    THEN substr(approved_at, 1, length(approved_at) - 1) || '.000000000Z'
  ELSE substr(approved_at, 1, instr(approved_at, '.') - 1)
    || '.' || substr(substr(approved_at, 1, length(approved_at) - 1) || '000000000',
                     instr(approved_at, '.') + 1, 9) || 'Z'
END
WHERE approved_at IS NOT NULL AND length(approved_at) <> 30;

UPDATE repurpose_plan_revisions SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE repurpose_plans SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE repurpose_plans SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE schema_migrations SET applied_at = CASE
  WHEN instr(applied_at, '.') = 0
    THEN substr(applied_at, 1, length(applied_at) - 1) || '.000000000Z'
  ELSE substr(applied_at, 1, instr(applied_at, '.') - 1)
    || '.' || substr(substr(applied_at, 1, length(applied_at) - 1) || '000000000',
                     instr(applied_at, '.') + 1, 9) || 'Z'
END
WHERE applied_at IS NOT NULL AND length(applied_at) <> 30;

UPDATE settings SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE shoot_sessions SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE shoot_sessions SET ends_at = CASE
  WHEN instr(ends_at, '.') = 0
    THEN substr(ends_at, 1, length(ends_at) - 1) || '.000000000Z'
  ELSE substr(ends_at, 1, instr(ends_at, '.') - 1)
    || '.' || substr(substr(ends_at, 1, length(ends_at) - 1) || '000000000',
                     instr(ends_at, '.') + 1, 9) || 'Z'
END
WHERE ends_at IS NOT NULL AND length(ends_at) <> 30;

UPDATE shoot_sessions SET starts_at = CASE
  WHEN instr(starts_at, '.') = 0
    THEN substr(starts_at, 1, length(starts_at) - 1) || '.000000000Z'
  ELSE substr(starts_at, 1, instr(starts_at, '.') - 1)
    || '.' || substr(substr(starts_at, 1, length(starts_at) - 1) || '000000000',
                     instr(starts_at, '.') + 1, 9) || 'Z'
END
WHERE starts_at IS NOT NULL AND length(starts_at) <> 30;

UPDATE shoot_sessions SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE shot_semantic_vectors SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE speech_classifications SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE tag_aliases_v2 SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE tag_catalog SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE tag_catalog SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE tag_change_proposals SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE tag_change_proposals SET reviewed_at = CASE
  WHEN instr(reviewed_at, '.') = 0
    THEN substr(reviewed_at, 1, length(reviewed_at) - 1) || '.000000000Z'
  ELSE substr(reviewed_at, 1, instr(reviewed_at, '.') - 1)
    || '.' || substr(substr(reviewed_at, 1, length(reviewed_at) - 1) || '000000000',
                     instr(reviewed_at, '.') + 1, 9) || 'Z'
END
WHERE reviewed_at IS NOT NULL AND length(reviewed_at) <> 30;

UPDATE tag_clusters SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE tag_curation_runs SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE tag_curation_runs SET finished_at = CASE
  WHEN instr(finished_at, '.') = 0
    THEN substr(finished_at, 1, length(finished_at) - 1) || '.000000000Z'
  ELSE substr(finished_at, 1, instr(finished_at, '.') - 1)
    || '.' || substr(substr(finished_at, 1, length(finished_at) - 1) || '000000000',
                     instr(finished_at, '.') + 1, 9) || 'Z'
END
WHERE finished_at IS NOT NULL AND length(finished_at) <> 30;

UPDATE tag_embedding_runs SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE tag_embedding_runs SET finished_at = CASE
  WHEN instr(finished_at, '.') = 0
    THEN substr(finished_at, 1, length(finished_at) - 1) || '.000000000Z'
  ELSE substr(finished_at, 1, instr(finished_at, '.') - 1)
    || '.' || substr(substr(finished_at, 1, length(finished_at) - 1) || '000000000',
                     instr(finished_at, '.') + 1, 9) || 'Z'
END
WHERE finished_at IS NOT NULL AND length(finished_at) <> 30;

UPDATE tag_embeddings SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE tag_library_summaries SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;

UPDATE tag_suppressions SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE transcripts SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE worker_pairing_tokens SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE worker_pairing_tokens SET expires_at = CASE
  WHEN instr(expires_at, '.') = 0
    THEN substr(expires_at, 1, length(expires_at) - 1) || '.000000000Z'
  ELSE substr(expires_at, 1, instr(expires_at, '.') - 1)
    || '.' || substr(substr(expires_at, 1, length(expires_at) - 1) || '000000000',
                     instr(expires_at, '.') + 1, 9) || 'Z'
END
WHERE expires_at IS NOT NULL AND length(expires_at) <> 30;

UPDATE worker_pairing_tokens SET redeemed_at = CASE
  WHEN instr(redeemed_at, '.') = 0
    THEN substr(redeemed_at, 1, length(redeemed_at) - 1) || '.000000000Z'
  ELSE substr(redeemed_at, 1, instr(redeemed_at, '.') - 1)
    || '.' || substr(substr(redeemed_at, 1, length(redeemed_at) - 1) || '000000000',
                     instr(redeemed_at, '.') + 1, 9) || 'Z'
END
WHERE redeemed_at IS NOT NULL AND length(redeemed_at) <> 30;

UPDATE workers SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE workers SET last_seen_at = CASE
  WHEN instr(last_seen_at, '.') = 0
    THEN substr(last_seen_at, 1, length(last_seen_at) - 1) || '.000000000Z'
  ELSE substr(last_seen_at, 1, instr(last_seen_at, '.') - 1)
    || '.' || substr(substr(last_seen_at, 1, length(last_seen_at) - 1) || '000000000',
                     instr(last_seen_at, '.') + 1, 9) || 'Z'
END
WHERE last_seen_at IS NOT NULL AND length(last_seen_at) <> 30;

UPDATE workers SET revoked_at = CASE
  WHEN instr(revoked_at, '.') = 0
    THEN substr(revoked_at, 1, length(revoked_at) - 1) || '.000000000Z'
  ELSE substr(revoked_at, 1, instr(revoked_at, '.') - 1)
    || '.' || substr(substr(revoked_at, 1, length(revoked_at) - 1) || '000000000',
                     instr(revoked_at, '.') + 1, 9) || 'Z'
END
WHERE revoked_at IS NOT NULL AND length(revoked_at) <> 30;

UPDATE workflow_runs SET created_at = CASE
  WHEN instr(created_at, '.') = 0
    THEN substr(created_at, 1, length(created_at) - 1) || '.000000000Z'
  ELSE substr(created_at, 1, instr(created_at, '.') - 1)
    || '.' || substr(substr(created_at, 1, length(created_at) - 1) || '000000000',
                     instr(created_at, '.') + 1, 9) || 'Z'
END
WHERE created_at IS NOT NULL AND length(created_at) <> 30;

UPDATE workflow_runs SET updated_at = CASE
  WHEN instr(updated_at, '.') = 0
    THEN substr(updated_at, 1, length(updated_at) - 1) || '.000000000Z'
  ELSE substr(updated_at, 1, instr(updated_at, '.') - 1)
    || '.' || substr(substr(updated_at, 1, length(updated_at) - 1) || '000000000',
                     instr(updated_at, '.') + 1, 9) || 'Z'
END
WHERE updated_at IS NOT NULL AND length(updated_at) <> 30;
