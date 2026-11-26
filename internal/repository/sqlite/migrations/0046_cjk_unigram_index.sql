-- Re-tokenize the existing asset and shot indexes with the unigram terms.
-- The Hub rebuilds both indexes and marks this state ready in one transaction.
INSERT INTO fts_index_state(name, value, updated_at)
VALUES('cjk_bigram_unigram_v2', 'pending', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
ON CONFLICT(name) DO UPDATE SET value='pending', updated_at=excluded.updated_at;
