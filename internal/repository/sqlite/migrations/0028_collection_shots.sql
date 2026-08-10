-- v0.29: shot baskets — a collection may also pin individual shots, in an
-- explicit user-chosen display order, while its saved-filter semantics stay
-- untouched. A shot in a collection is a selection (a reference to
-- asset_shots.id), not a copy: deleting the collection removes the row via the
-- foreign key. The shot FK cascades too: a re-analysis replaces a shot row
-- with fresh ids, and a pin on a shot that no longer exists must disappear
-- with it rather than orphan silently (an orphan would overcount shot_count,
-- vanish from the join, and reject reorders).
CREATE TABLE collection_shots (
    collection_id TEXT NOT NULL REFERENCES asset_collections(id) ON DELETE CASCADE,
    shot_id TEXT NOT NULL REFERENCES asset_shots(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (collection_id, shot_id)
);
CREATE INDEX idx_collection_shots_collection ON collection_shots(collection_id, position);
