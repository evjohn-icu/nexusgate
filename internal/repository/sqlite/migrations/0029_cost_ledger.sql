-- v0.29 keeps per-channel cost metadata's accumulated effect. The ledger is
-- append-only: each committed model run with cost metadata writes one row, and
-- today/month estimates are SUMs over it. The estimate is a guide in the
-- operator-chosen relative unit, never a billing record, so no foreign keys
-- harden rows against later deletion of the asset or channel that produced
-- them.

CREATE TABLE cost_ledger (
    id TEXT PRIMARY KEY,
    day TEXT NOT NULL,            -- YYYY-MM-DD UTC
    capability TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    asset_id TEXT NOT NULL DEFAULT '',
    estimate REAL NOT NULL,       -- in the channel's configured unit
    created_at TEXT NOT NULL
);
CREATE INDEX idx_cost_ledger_day ON cost_ledger(day);

-- Per-channel optional cost metadata. The unit is deliberately unspecified:
-- the ledger's sums are a guide for cost tracking, never a billing record.
-- Zero/absent means "this channel declares no price" and contributes nothing.
ALTER TABLE provider_channels ADD COLUMN cost_per_request REAL NOT NULL DEFAULT 0;
ALTER TABLE provider_channels ADD COLUMN cost_per_video_minute REAL NOT NULL DEFAULT 0;
ALTER TABLE provider_channels ADD COLUMN cost_per_audio_minute REAL NOT NULL DEFAULT 0;
