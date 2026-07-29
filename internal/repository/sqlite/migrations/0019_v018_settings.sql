-- Settings live in a generic key/value table rather than having one typed
-- column per setting because runtime-editable configuration should not require a
-- schema migration every time a new setting is added. The provider-channel work
-- already established SQLite as the home for runtime-editable configuration, and
-- a JSON value column lets each setting evolve its shape independently.
CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
