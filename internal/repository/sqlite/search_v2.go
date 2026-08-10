package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/search"
	"github.com/evjohn-icu/timingdex/internal/textindex"
)

// This file implements the search.ShotStore contract (declared in
// internal/search/store.go) on *Repository, structurally — the sqlite package
// never imports the search package. Each method is one retrieval channel or
// evidence/context lookup the v2 engine consumes. The legacy search methods
// (searchShots, hybridSearchShots, ...) in repository.go stay untouched: they
// are the golden-pinned compatibility baseline.

// ScoreCandidates is the legacy candidate scorer: lexical bm25 + heuristic
// semantic cosine with the shared-token gate, facet-aware. The v2 semantic
// channel and the compatibility path both consume it.
func (r *Repository) ScoreCandidates(ctx context.Context, q string, facets domain.FacetFilter) ([]domain.ShotSearchResult, error) {
	return r.scoreShotCandidates(ctx, q, facets)
}

// LexicalRankedShots is the field-aware lexical channel. weights is indexed
// [description, tags, objects, actions, mood] and maps onto FTS5's bm25
// column weights. The FTS table declares shot_id and asset_id UNINDEXED, so
// they consume the first two weight positions and are pinned to 0; a flat
// all-1.0 weight set must equal the plain bm25(asset_shot_search) score, which
// the v2 tests pin (TestSearchV2LexicalFlatEqualsLegacy).
func (r *Repository) LexicalRankedShots(ctx context.Context, q string, weights [5]float64, limit int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	ftsQuery := buildFTSQuery(q)
	if ftsQuery == "" {
		return []domain.ShotSearchResult{}, nil
	}
	sanitized := [5]float64{}
	for i, w := range weights {
		if w > 0 {
			sanitized[i] = w
		}
	}
	query := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),''),bm25(asset_shot_search,0,0,?,?,?,?,?) AS rank FROM asset_shot_search JOIN asset_shots s ON s.id=asset_shot_search.shot_id WHERE asset_shot_search MATCH ? ORDER BY rank LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, sanitized[0], sanitized[1], sanitized[2], sanitized[3], sanitized[4], ftsQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ShotSearchResult
	for rows.Next() {
		result, err := scanShotRowWithFilename(rows, "rank")
		if err != nil {
			return nil, err
		}
		result.LexicalScore = lexicalScoreFromBM25(result.LexicalScore)
		if result.LexicalScore > 0 {
			out = append(out, result)
		}
	}
	return out, rows.Err()
}

// TranscriptRankedShots is the speech channel: query tokens matched against
// aligned transcript words, scoring only shots whose time range overlaps a
// matched word. The time-overlap JOIN is the boundary that keeps speech
// evidence inside its interval — a word spoken at 310s never scores a shot at
// 0-300s. CJK tokens match as substrings (Chinese has no word boundaries in
// ASR output either); ASCII tokens match the whole aligned word, so "car"
// cannot match a word like "carefree".
func (r *Repository) TranscriptRankedShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	patterns := transcriptPatterns(q)
	if len(patterns) == 0 {
		return []domain.ShotSearchResult{}, nil
	}
	where := make([]string, 0, len(patterns))
	args := make([]any, 0, len(patterns))
	for _, p := range patterns {
		where = append(where, `w.text LIKE ? ESCAPE '\'`)
		args = append(args, p)
	}
	query := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),''),MIN(SUM(COALESCE(w.confidence,1)),5.0)/5.0 AS tscore FROM transcript_words w JOIN asset_shots s ON s.asset_id=w.asset_id AND s.start_ms < w.end_ms AND s.end_ms > w.start_ms WHERE ` + strings.Join(where, ` OR `) + ` GROUP BY s.id ORDER BY tscore DESC LIMIT ?`
	// SQL only narrows the candidate set. Exact phrase validation below must
	// see enough candidates that partial token hits cannot consume the limit.
	candidateLimit := limit * 10
	if candidateLimit < 100 {
		candidateLimit = 100
	}
	args = append(args, candidateLimit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ShotSearchResult
	for rows.Next() {
		result, err := scanShotRowWithFilename(rows, "tscore")
		if err != nil {
			return nil, err
		}
		spans, err := r.ShotTranscriptSpans(ctx, result.AssetID, result.StartMS, result.EndMS)
		if err != nil {
			return nil, err
		}
		if !search.MatchAlignedSpeechPhrase(q, spans) {
			continue
		}
		result.TranscriptScore = result.LexicalScore // slot carries tscore
		result.LexicalScore = 0
		if result.TranscriptScore > 0 {
			out = append(out, result)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, rows.Err()
}

// transcriptPatterns builds LIKE patterns for the query's tokens. CJK tokens
// are substrings; ASCII tokens must equal the aligned word (a LIKE pattern
// that only matches a whole word is `w.text = token`), keeping the whole-word
// discipline that prevents car→carefree. The LIKE wildcards % and _ inside
// tokens are escaped so a token containing one is matched literally.
func transcriptPatterns(q string) []string {
	tokens := textindex.Tokens(q)
	seen := map[string]bool{}
	patterns := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		if textindex.IsCJKToken(token) {
			patterns = append(patterns, `%`+escapeLike(token)+`%`)
		} else {
			patterns = append(patterns, token)
		}
	}
	return patterns
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value
}

// MetadataRankedShots is the weak asset-level channel: it matches the
// owning asset's FILENAME only (score 1.0, spread over the asset's shots).
// Asset-level summary/scene/subject text is deliberately NOT matched: the
// golden corpus pins the opposite behaviour — an asset-global tag must never
// pollute a shot that never saw the object — and the metadata channel is a
// retrieval hint, never shot-level evidence (the evidence gate ignores it).
func (r *Repository) MetadataRankedShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	tokens := textindex.Tokens(q)
	if len(tokens) == 0 {
		return []domain.ShotSearchResult{}, nil
	}
	// ASCII tokens match whole words (the discovery alias discipline), CJK
	// tokens match as substrings.
	ascii := map[string]bool{}
	var cjk []string
	for _, token := range tokens {
		if textindex.IsCJKToken(token) {
			cjk = append(cjk, token)
		} else {
			ascii[token] = true
		}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT a.id,COALESCE(l.relative_path,'') FROM assets a LEFT JOIN asset_locations l ON l.id=(SELECT l2.id FROM asset_locations l2 JOIN library_roots lr ON lr.id=l2.root_id WHERE l2.asset_id=a.id AND l2.is_primary=1 AND l2.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY a.id,l2.last_seen_at DESC,lr.created_at,lr.id,l2.relative_path,l2.id LIMIT 1) ORDER BY a.id`)
	if err != nil {
		return nil, err
	}
	assetIDs := make([]string, 0, 64)
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, err
		}
		if path != "" && filenameMatches(strings.ToLower(filepath.Base(path)), ascii, cjk) {
			assetIDs = append(assetIDs, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(assetIDs) == 0 {
		return []domain.ShotSearchResult{}, nil
	}
	var out []domain.ShotSearchResult
	const chunkSize = 500
	for start := 0; start < len(assetIDs) && len(out) < limit; start += chunkSize {
		end := start + chunkSize
		if end > len(assetIDs) {
			end = len(assetIDs)
		}
		chunk := assetIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat(`?,`, len(chunk)), `,`)
		shotRows, err := r.db.QueryContext(ctx, `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),'') FROM asset_shots s WHERE s.asset_id IN (`+placeholders+`) ORDER BY s.asset_id,s.ordinal`, strSliceToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for shotRows.Next() && len(out) < limit {
			result, err := scanShotRowWithFilename(shotRows, "")
			if err != nil {
				shotRows.Close()
				return nil, err
			}
			result.MetadataScore = 1.0
			out = append(out, result)
		}
		if err := shotRows.Err(); err != nil {
			shotRows.Close()
			return nil, err
		}
		shotRows.Close()
	}
	return out, nil
}

// filenameMatches reports whether the basename carries any query token as a
// whole ASCII word or as a CJK substring. ASCII tokens are compared against
// the filename's own word tokens, so "car" never matches "carefree.mov".
func filenameMatches(basename string, ascii map[string]bool, cjk []string) bool {
	for _, part := range strings.FieldsFunc(basename, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	}) {
		if ascii[part] {
			return true
		}
	}
	for _, token := range cjk {
		if strings.Contains(basename, token) {
			return true
		}
	}
	return false
}

// textMatchesTokens applies the same rule to free text: whole ASCII words or
// CJK substrings. Kept for future metadata use — the current metadata channel
// is filename-only (see MetadataRankedShots for why).
func textMatchesTokens(text string, ascii map[string]bool, cjk []string) bool {
	words := map[string]bool{}
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	}) {
		words[f] = true
	}
	for token := range ascii {
		if words[token] {
			return true
		}
	}
	for _, token := range cjk {
		if strings.Contains(text, token) {
			return true
		}
	}
	return false
}

func strSliceToAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// ShotTranscriptSpans returns the aligned words overlapping [startMS, endMS]
// of one asset — the transcript evidence a shot can actually claim.
func (r *Repository) ShotTranscriptSpans(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AlignmentWord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT start_ms,end_ms,text,confidence FROM transcript_words WHERE asset_id=? AND start_ms < ? AND end_ms > ? ORDER BY ordinal`, assetID, endMS, startMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AlignmentWord
	for rows.Next() {
		var w domain.AlignmentWord
		var confidence sql.NullFloat64
		if err := rows.Scan(&w.StartMS, &w.EndMS, &w.Text, &confidence); err != nil {
			return nil, err
		}
		if confidence.Valid {
			w.Confidence = &confidence.Float64
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// NeighborShots returns the shots adjacent to (assetID, ordinal) by ordinal,
// which may be non-contiguous — the lookups use < and > with ORDER BY rather
// than arithmetic on ordinals.
func (r *Repository) NeighborShots(ctx context.Context, assetID string, ordinal int) (*domain.AssetShot, *domain.AssetShot, error) {
	prevRow := r.db.QueryRowContext(ctx, `SELECT id,asset_id,COALESCE(source_run_id,''),ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at FROM asset_shots WHERE asset_id=? AND ordinal < ? ORDER BY ordinal DESC LIMIT 1`, assetID, ordinal)
	prev, err := scanSingleAssetShot(prevRow)
	if err != nil && !isNoRows(err) {
		return nil, nil, err
	}
	nextRow := r.db.QueryRowContext(ctx, `SELECT id,asset_id,COALESCE(source_run_id,''),ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at FROM asset_shots WHERE asset_id=? AND ordinal > ? ORDER BY ordinal ASC LIMIT 1`, assetID, ordinal)
	next, err := scanSingleAssetShot(nextRow)
	if err != nil && !isNoRows(err) {
		return nil, nil, err
	}
	return prev, next, nil
}

func isNoRows(err error) bool {
	return err == sql.ErrNoRows
}

func scanSingleAssetShot(row *sql.Row) (*domain.AssetShot, error) {
	var shot domain.AssetShot
	var tags, objects, actions, mood, created string
	if err := row.Scan(&shot.ID, &shot.AssetID, &shot.SourceRunID, &shot.Ordinal, &shot.StartMS, &shot.EndMS, &shot.Description, &tags, &objects, &actions, &mood, &shot.Confidence, &created); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tags), &shot.Tags)
	_ = json.Unmarshal([]byte(objects), &shot.Objects)
	_ = json.Unmarshal([]byte(actions), &shot.Actions)
	_ = json.Unmarshal([]byte(mood), &shot.Mood)
	shot.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &shot, nil
}

// ShotSession returns the shoot-session id of an asset, "" when it has none.
func (r *Repository) ShotSession(ctx context.Context, assetID string) (string, error) {
	var sessionID string
	err := r.db.QueryRowContext(ctx, `SELECT session_id FROM asset_shoot_sessions WHERE asset_id=? ORDER BY is_primary DESC, created_at DESC LIMIT 1`, assetID).Scan(&sessionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return sessionID, nil
}

// ShotSessions is the batch form of ShotSession: it returns the
// asset_id -> session_id mapping for the given asset IDs in one query per
// chunk, so session diversity never performs per-result lookups. Assets
// without a shoot session are absent from the map. Empty input returns an
// empty map.
//
// Multiple sessions per asset resolve exactly like ShotSession (primary
// first, then most recent); the ORDER BY and first-wins scan keep the two
// answers consistent. Inputs are chunked at 500 because SQLite's default
// max variable limit is 999, mirroring the provider-channel chunking.
func (r *Repository) ShotSessions(ctx context.Context, assetIDs []string) (map[string]string, error) {
	sessions := make(map[string]string, len(assetIDs))
	const chunkSize = 500
	for start := 0; start < len(assetIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(assetIDs) {
			end = len(assetIDs)
		}
		chunk := assetIDs[start:end]
		placeholders := strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")
		rows, err := r.db.QueryContext(ctx, `SELECT asset_id, session_id FROM asset_shoot_sessions WHERE asset_id IN (`+placeholders+`) ORDER BY asset_id, is_primary DESC, created_at DESC`, strSliceToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var assetID, sessionID string
			if err := rows.Scan(&assetID, &sessionID); err != nil {
				rows.Close()
				return nil, err
			}
			if _, seen := sessions[assetID]; !seen {
				sessions[assetID] = sessionID
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return sessions, nil
}

// scanShotRowWithFilename scans a shot row that ends with the primary
// location's relative_path and (optionally) one extra float64 value in the
// last column. The extra float is returned inside LexicalScore as a slot —
// callers move it to the field they actually mean — which keeps the two scan
// paths in one helper. A rank column of 0 means "no rank asked".
func scanShotRowWithFilename(rows interface{ Scan(...any) error }, extraColumn string) (domain.ShotSearchResult, error) {
	var result domain.ShotSearchResult
	var tags, objects, actions, mood, created, filename string
	var extra float64
	fields := []any{&result.ID, &result.AssetID, &result.SourceRunID, &result.Ordinal, &result.StartMS, &result.EndMS, &result.Description, &tags, &objects, &actions, &mood, &result.Confidence, &created, &filename}
	if extraColumn != "" {
		fields = append(fields, &extra)
	}
	if err := rows.Scan(fields...); err != nil {
		return result, err
	}
	_ = json.Unmarshal([]byte(tags), &result.Tags)
	_ = json.Unmarshal([]byte(objects), &result.Objects)
	_ = json.Unmarshal([]byte(actions), &result.Actions)
	_ = json.Unmarshal([]byte(mood), &result.Mood)
	result.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if filename != "" {
		result.Filename = filepath.Base(filename)
	}
	if extraColumn != "" {
		result.LexicalScore = extra
	}
	return result, nil
}
