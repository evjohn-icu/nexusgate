-- v0.16 soft-delete for provider channels. A tombstone column replaces the
-- pre-v0.16 in-memory deletedProviderChannels map so the deletion survives
-- Hub restart. The UNIQUE(capability,label) constraint still blocks reuse of
-- a deleted label; this is a known limitation documented at the application
-- layer.

ALTER TABLE provider_channels ADD COLUMN deleted_at TEXT;
