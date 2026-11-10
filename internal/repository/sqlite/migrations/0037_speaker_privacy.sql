-- 0037_speaker_privacy: opt-in biometric profiles and their purge records.
--
-- The guarantee is logical deletion: rows, references and association state
-- stop being reachable and encrypted payloads are removed. It is not a claim of
-- physical erasure from SQLite pages, WAL files or rotated backups, and the
-- operator-facing copy says so.

CREATE TABLE speaker_consent (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    enabled INTEGER NOT NULL DEFAULT 0,
    generation INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL
);
INSERT INTO speaker_consent(singleton, enabled, generation, updated_at)
VALUES (1, 0, 0, '1970-01-01T00:00:00.000000000Z');

-- A profile is one asset-local speaker's eligible samples, not a person.
CREATE TABLE speaker_profiles (
    id TEXT PRIMARY KEY,
    generation INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL CHECK (state IN ('active','deleted')),
    created_at TEXT NOT NULL,
    deleted_at TEXT
);

CREATE TABLE speaker_profile_sources (
    profile_id TEXT NOT NULL REFERENCES speaker_profiles(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    diarization_generation_id TEXT NOT NULL,
    local_speaker_id TEXT NOT NULL,
    PRIMARY KEY (profile_id, asset_id, diarization_generation_id, local_speaker_id)
);

-- secret_ref names an encrypted payload in the Hub-only secret store. The
-- vector itself never lands in this database, in model_runs or in any HTTP
-- response.
CREATE TABLE speaker_embeddings (
    id TEXT PRIMARY KEY,
    profile_id TEXT NOT NULL REFERENCES speaker_profiles(id) ON DELETE CASCADE,
    asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    generation_id TEXT NOT NULL,
    space_id TEXT NOT NULL,
    start_ms INTEGER NOT NULL,
    end_ms INTEGER NOT NULL,
    speech_duration_ms INTEGER NOT NULL,
    quality_json TEXT NOT NULL DEFAULT '{}',
    secret_ref TEXT NOT NULL UNIQUE,
    consent_generation INTEGER NOT NULL,
    profile_generation INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX idx_speaker_embeddings_profile ON speaker_embeddings(profile_id, space_id);

CREATE TABLE speaker_associations (
    id TEXT PRIMARY KEY,
    left_profile_id TEXT NOT NULL REFERENCES speaker_profiles(id) ON DELETE CASCADE,
    right_profile_id TEXT NOT NULL REFERENCES speaker_profiles(id) ON DELETE CASCADE,
    state TEXT NOT NULL CHECK (state IN ('possible_match','confirmed_by_user','rejected','unknown')),
    similarity REAL NOT NULL,
    calibration_id TEXT,
    consent_generation INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (left_profile_id, right_profile_id),
    CHECK (left_profile_id < right_profile_id)
);

-- Durable deletion worklist. A secret reference is recorded here before the
-- ciphertext is written, so a crash between "encrypted" and "published" leaves
-- a deletion obligation the recovery drain can still honour.
CREATE TABLE biometric_purge_refs (
    secret_ref TEXT PRIMARY KEY,
    created_at TEXT NOT NULL,
    last_error_code TEXT
);

-- Staging metadata for one biometric extraction. The staged vector lives in the
-- secret store under secret_ref; this row only says whose it is and which
-- generations must still be current at commit time.
CREATE TABLE speaker_embedding_staging (
    operation_id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    profile_id TEXT NOT NULL,
    profile_generation INTEGER NOT NULL,
    consent_generation INTEGER NOT NULL,
    secret_ref TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
