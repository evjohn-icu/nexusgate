package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
	"unicode"

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

// ScoreCandidatesV2 is the asset-context-aware candidate scorer. A zero
// assetFilter delegates to the legacy scorer unchanged; a non-zero one narrows
// the candidate universe by the owning asset's capture/session/status
// predicates (assetContextClauses), so a shot whose asset is outside the
// selected date/status/region/camera/session is excluded before any ranking.
func (r *Repository) ScoreCandidatesV2(ctx context.Context, q string, facets domain.FacetFilter, assetFilter domain.AssetContextFilter) ([]domain.ShotSearchResult, error) {
	if !hasAssetContext(assetFilter) {
		return r.scoreShotCandidates(ctx, q, facets)
	}
	return r.scoreShotCandidatesWithContext(ctx, q, facets, assetFilter)
}

// hasAssetContext reports whether the asset-context filter carries any
// constraint. It mirrors the clause builder: trimmed free-text fields and a
// non-empty status count, so a filter that is all-empty is indistinguishable
// from the zero value.
func hasAssetContext(f domain.AssetContextFilter) bool {
	return f.CapturedFrom != nil || f.CapturedTo != nil ||
		strings.TrimSpace(f.RegionLabel) != "" || strings.TrimSpace(f.CameraModel) != "" ||
		strings.TrimSpace(f.SessionID) != "" || f.Status != ""
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
// The transcript channel reads two speech-source tables. The per-asset
// source rule keeps them disjoint: an asset contributes either its alignment
// words (transcript_words) or its ASR segments (asr_segments), never both —
// matching the aligned-wins fallback the transcript endpoint and analysis
// paths use. Retrieval runs the same ranked query over each source and merges
// the disjoint pools in Go, rather than a SQL UNION: modernc.org/sqlite plans
// a UNION with the correlated per-asset exclusion far more expensively, which
// the -race test suite amplifies to a timeout.
const (
	alignedSpeechTable = "transcript_words"
	asrSpeechTable     = "asr_segments"
)

// transcriptTokenPool runs the token-prefilter ranked query over one speech
// source, returning the scanned pool ordered by tscore DESC, id ASC (the
// tscore rides in the LexicalScore slot until validation renames it). When
// excludeAligned is set (the ASR source), assets that already have alignment
// words are excluded, so an asset never appears in both pools.
func (r *Repository) transcriptTokenPool(ctx context.Context, patterns []string, candidateLimit int, source string, excludeAligned bool) ([]domain.ShotSearchResult, error) {
	where := make([]string, 0, len(patterns))
	args := make([]any, 0, len(patterns))
	for _, p := range patterns {
		where = append(where, `w.text LIKE ? ESCAPE '\'`)
		args = append(args, p)
	}
	exclusion := ""
	if excludeAligned {
		exclusion = ` AND NOT EXISTS (SELECT 1 FROM transcript_words t WHERE t.asset_id = w.asset_id)`
	}
	query := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),''),MIN(SUM(COALESCE(w.confidence,1)),5.0)/5.0 AS tscore FROM ` + source + ` w JOIN asset_shots s ON s.asset_id=w.asset_id AND s.start_ms < w.end_ms AND s.end_ms > w.start_ms WHERE (` + strings.Join(where, ` OR `) + `)` + exclusion
	if len(patterns) > 1 {
		// Every phrase component must be present in the shot before the
		// bounded pool is formed. The Go pass still owns order, reuse, and
		// timing; this indexed pass only prevents partial-token saturation.
		allPatterns := make([]string, 0, len(patterns))
		for range patterns {
			allPatterns = append(allPatterns, `EXISTS (SELECT 1 FROM `+source+` wp WHERE wp.asset_id=s.asset_id AND s.start_ms < wp.end_ms AND s.end_ms > wp.start_ms AND wp.text LIKE ? ESCAPE '\')`)
		}
		query += ` GROUP BY s.id HAVING ` + strings.Join(allPatterns, ` AND `)
		args = append(args, args[:len(patterns)]...)
	} else {
		query += ` GROUP BY s.id`
	}
	query += ` ORDER BY tscore DESC, s.id ASC LIMIT ?`
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
		out = append(out, result)
	}
	return out, rows.Err()
}

// transcriptTokenPools merges the two sources' token pools. Both are ordered
// by (tscore DESC, id ASC) and are disjoint per asset, so a two-way merge
// keeps the globally best candidates first for the validation pass.
func (r *Repository) transcriptTokenPools(ctx context.Context, patterns []string, candidateLimit int) ([]domain.ShotSearchResult, error) {
	aligned, err := r.transcriptTokenPool(ctx, patterns, candidateLimit, alignedSpeechTable, false)
	if err != nil {
		return nil, err
	}
	asr, err := r.transcriptTokenPool(ctx, patterns, candidateLimit, asrSpeechTable, true)
	if err != nil {
		return nil, err
	}
	merged := make([]domain.ShotSearchResult, 0, len(aligned)+len(asr))
	i, j := 0, 0
	for i < len(aligned) && j < len(asr) {
		a, b := aligned[i], asr[j]
		if a.LexicalScore > b.LexicalScore || (a.LexicalScore == b.LexicalScore && a.ID < b.ID) {
			merged = append(merged, a)
			i++
		} else {
			merged = append(merged, b)
			j++
		}
	}
	merged = append(merged, aligned[i:]...)
	merged = append(merged, asr[j:]...)
	return merged, nil
}

func (r *Repository) TranscriptRankedShots(ctx context.Context, q string, limit int) ([]domain.ShotSearchResult, error) {
	if limit <= 0 || strings.TrimSpace(q) == "" {
		return []domain.ShotSearchResult{}, nil
	}
	patterns := transcriptPrefilterPatterns(q)
	if len(patterns) == 0 {
		return []domain.ShotSearchResult{}, nil
	}
	// Phrase-first pool: for a multi-component phrase, a shot must contain
	// every adjacent component pair inside a 1500ms window (the same bound the
	// validator enforces) before it may occupy a candidate slot. Scattered
	// partial-token shots therefore cannot starve the exact-phrase shot: the
	// exact shot always satisfies the windows, so it always lands inside the
	// bounded pool regardless of how many partials are ahead of it.
	components := search.SpeechComponents(strings.ToLower(strings.TrimSpace(q)))
	if len(components) > 1 {
		return r.transcriptPhraseFirst(ctx, q, limit, components)
	}

	candidateLimit := limit * 50
	if candidateLimit < 1000 {
		candidateLimit = 1000
	}
	if candidateLimit > 10000 {
		candidateLimit = 10000
	}
	pool, err := r.transcriptTokenPools(ctx, patterns, candidateLimit)
	if err != nil {
		return nil, err
	}
	spansByID, err := r.ShotTranscriptSpansBatch(ctx, transcriptSpanRequests(pool))
	if err != nil {
		return nil, err
	}
	var out []domain.ShotSearchResult
	for _, result := range pool {
		if !search.MatchAlignedSpeechPhrase(q, spansByID[result.ID]) {
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
	return out, nil
}

// transcriptPhraseFirst is the phrase-first pool for multi-component phrases.
// For every adjacent component pair (ci, ci+1) it requires a word matching the
// tail of ci and a word matching the head of ci+1 in the same shot within the
// validator's 1500ms gap bound. The exact-phrase shot satisfies every pair by
// construction, so it can never be crowded out of the candidate pool; shots
// that merely contain the phrase's characters scattered across time fail at
// least one window and never occupy a slot. Go-side validation still decides
// the final verdict; this pass only decides who enters the bounded pool. Like
// the token path it runs over both speech sources and merges the disjoint
// pools (per asset, ordered by id).
func (r *Repository) transcriptPhraseFirst(ctx context.Context, q string, limit int, components []search.SpeechComponent) ([]domain.ShotSearchResult, error) {
	windows := make([]string, 0, len(components)-1)
	args := make([]any, 0, 2*(len(components)-1))
	for i := 0; i+1 < len(components); i++ {
		ciTail, _ := headTail(components[i])
		_, ci1Head := headTail(components[i+1])
		windows = append(windows, `EXISTS (SELECT 1 FROM ${SRC} wa JOIN ${SRC} wb ON wb.asset_id=wa.asset_id AND wb.ordinal > wa.ordinal AND wb.start_ms - wa.end_ms <= 1500 WHERE wa.asset_id=s.asset_id AND s.start_ms < wa.end_ms AND s.end_ms > wa.start_ms AND wa.text LIKE ? ESCAPE '\' AND wb.asset_id=s.asset_id AND s.start_ms < wb.end_ms AND s.end_ms > wb.start_ms AND wb.text LIKE ? ESCAPE '\')`)
		args = append(args, `%`+escapeLike(ciTail)+`%`, `%`+escapeLike(ci1Head)+`%`)
	}
	candidateLimit := limit * 50
	if candidateLimit < 1000 {
		candidateLimit = 1000
	}
	if candidateLimit > 10000 {
		candidateLimit = 10000
	}
	base := `SELECT s.id,s.asset_id,COALESCE(s.source_run_id,''),s.ordinal,s.start_ms,s.end_ms,s.description,s.tags_json,s.objects_json,s.actions_json,s.mood_json,s.confidence,s.created_at,COALESCE((SELECT l.relative_path FROM asset_locations l JOIN library_roots lr ON lr.id=l.root_id WHERE l.asset_id=s.asset_id AND l.is_primary=1 AND l.exists_now=1 AND lr.health_state<>'unavailable' ORDER BY l.last_seen_at DESC,lr.created_at,lr.id,l.relative_path,l.id LIMIT 1),''),0.0 AS tscore FROM asset_shots s WHERE `
	runSource := func(source string, excludeAligned bool) ([]domain.ShotSearchResult, error) {
		windowsSrc := make([]string, 0, len(windows))
		for _, w := range windows {
			windowsSrc = append(windowsSrc, strings.ReplaceAll(w, "${SRC}", source))
		}
		exclusion := ""
		if excludeAligned {
			exclusion = ` AND NOT EXISTS (SELECT 1 FROM transcript_words t WHERE t.asset_id = s.asset_id)`
		}
		query := base + strings.Join(windowsSrc, ` AND `) + exclusion + ` ORDER BY s.id ASC LIMIT ?`
		queryArgs := append(append([]any{}, args...), candidateLimit)
		rows, err := r.db.QueryContext(ctx, query, queryArgs...)
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
			out = append(out, result)
		}
		return out, rows.Err()
	}
	aligned, err := runSource(alignedSpeechTable, false)
	if err != nil {
		return nil, err
	}
	asr, err := runSource(asrSpeechTable, true)
	if err != nil {
		return nil, err
	}
	// Both pools are ordered by id ASC and are disjoint per asset; two-way
	// merge by id.
	pool := make([]domain.ShotSearchResult, 0, len(aligned)+len(asr))
	i, j := 0, 0
	for i < len(aligned) && j < len(asr) {
		if aligned[i].ID < asr[j].ID {
			pool = append(pool, aligned[i])
			i++
		} else {
			pool = append(pool, asr[j])
			j++
		}
	}
	pool = append(pool, aligned[i:]...)
	pool = append(pool, asr[j:]...)
	spansByID, err := r.ShotTranscriptSpansBatch(ctx, transcriptSpanRequests(pool))
	if err != nil {
		return nil, err
	}
	var out []domain.ShotSearchResult
	for _, result := range pool {
		if !search.MatchAlignedSpeechPhrase(q, spansByID[result.ID]) {
			continue
		}
		result.TranscriptScore = 1.0
		result.LexicalScore = 0
		out = append(out, result)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func transcriptSpanRequests(candidates []domain.ShotSearchResult) []search.TranscriptSpanRequest {
	requests := make([]search.TranscriptSpanRequest, 0, len(candidates))
	for _, candidate := range candidates {
		requests = append(requests, search.TranscriptSpanRequest{
			ShotID:  candidate.ID,
			AssetID: candidate.AssetID,
			StartMS: candidate.StartMS,
			EndMS:   candidate.EndMS,
		})
	}
	return requests
}

// headTail returns the last and first rune of a phrase component for the
// window conditions. ASCII components must match their whole word, so the
// head is the word itself and the tail is the word itself.
func headTail(c search.SpeechComponent) (string, string) {
	if !c.IsCJK() {
		return c.Text(), c.Text()
	}
	r := []rune(c.Text())
	return string(r[len(r)-1]), string(r[0])
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

// transcriptPrefilterPatterns keeps the SQL candidate pass compatible with
// ASR systems that emit one CJK word per aligned row. The exact phrase
// validator remains responsible for joining those rows in order.
func transcriptPrefilterPatterns(q string) []string {
	seen := map[string]bool{}
	var patterns []string
	for _, chunk := range textindex.Chunks(q) {
		if chunk.CJK {
			for _, token := range chunk.Tokens {
				for _, r := range token {
					value := string(r)
					if !seen[value] {
						seen[value] = true
						patterns = append(patterns, `%`+escapeLike(value)+`%`)
					}
				}
			}
			continue
		}
		for _, token := range chunk.Tokens {
			if token != "" && !seen[token] {
				seen[token] = true
				patterns = append(patterns, token)
			}
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

// ShotTranscriptSpans returns the speech spans overlapping [startMS, endMS]
// of one asset — the transcript evidence a shot can actually claim. Aligned
// assets contribute their alignment words as-is; ASR-only assets contribute
// their timed ASR segments expanded to per-rune/per-word spans (see
// expandAsrSegments), so the exact-phrase validator can match a sub-phrase
// that a whole sentence-level segment alone would not satisfy.
//
// The source resolution is done here in Go rather than with the speech CTE
// because the evidence gate calls this once per candidate: the aligned path
// (the overwhelmingly common case) stays a single plain indexed query, and
// only ASR-only assets pay a second query.
func (r *Repository) ShotTranscriptSpans(ctx context.Context, assetID string, startMS, endMS int64) ([]domain.AlignmentWord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT start_ms,end_ms,text,confidence FROM transcript_words WHERE asset_id=? AND start_ms < ? AND end_ms > ? ORDER BY ordinal`, assetID, endMS, startMS)
	if err != nil {
		return nil, err
	}
	var out []domain.AlignmentWord
	for rows.Next() {
		var w domain.AlignmentWord
		var confidence sql.NullFloat64
		if err := rows.Scan(&w.StartMS, &w.EndMS, &w.Text, &confidence); err != nil {
			rows.Close()
			return nil, err
		}
		if confidence.Valid {
			w.Confidence = &confidence.Float64
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(out) > 0 {
		return out, nil
	}
	// ASR-only asset: expand its timed segments to per-rune/per-word spans.
	segRows, err := r.db.QueryContext(ctx, `SELECT start_ms,end_ms,text FROM asr_segments WHERE asset_id=? AND start_ms < ? AND end_ms > ? ORDER BY ordinal`, assetID, endMS, startMS)
	if err != nil {
		return nil, err
	}
	defer segRows.Close()
	for segRows.Next() {
		var seg domain.AlignmentWord
		if err := segRows.Scan(&seg.StartMS, &seg.EndMS, &seg.Text); err != nil {
			return nil, err
		}
		out = expandAsrSegments(out, seg)
	}
	return out, segRows.Err()
}

// isSpeechCJK matches the search package's speech-CJK definition (kana,
// unified CJK, hangul, and CJK-compatibility ideographs) — the boundary the
// exact-phrase validator uses to split a phrase into components.
func isSpeechCJK(r rune) bool {
	return r >= '\u3040' && r <= '\u30ff' || r >= '\u3400' && r <= '\u9fff' || r >= '\uac00' && r <= '\ud7af' || r >= '\uf900' && r <= '\ufaff'
}

// expandAsrSegments appends one timed ASR segment expanded into the span
// granularity the validator accumulates: every CJK rune becomes its own span,
// every ASCII word run becomes one span, each carrying the segment's time
// range. The validator joins spans in order (within the 1500ms gap) and
// requires the accumulated text to equal the phrase, so per-rune spans let a
// phrase that is a proper sub-phrase of a sentence-level segment still match —
// exactly the speech the segment contains.
func expandAsrSegments(out []domain.AlignmentWord, segment domain.AlignmentWord) []domain.AlignmentWord {
	runes := []rune(segment.Text)
	for i := 0; i < len(runes); {
		if isSpeechCJK(runes[i]) {
			j := i + 1
			for j < len(runes) && isSpeechCJK(runes[j]) {
				j++
			}
			for k := i; k < j; k++ {
				out = append(out, domain.AlignmentWord{StartMS: segment.StartMS, EndMS: segment.EndMS, Text: string(runes[k])})
			}
			i = j
			continue
		}
		if unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) || runes[i] == '_' {
			j := i + 1
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_') && !isSpeechCJK(runes[j]) {
				j++
			}
			out = append(out, domain.AlignmentWord{StartMS: segment.StartMS, EndMS: segment.EndMS, Text: string(runes[i:j])})
			i = j
			continue
		}
		i++
	}
	return out
}

// ShotTranscriptSpansBatch returns aligned words overlapping each requested
// shot window. The result is keyed by ShotID, so callers can perform the
// per-shot evidence/phrase pass in memory after one SQLite round trip. The
// request array is expanded by SQLite's JSON1 table-valued function rather
// than host parameters; this keeps the method bounded for the 10k speech
// validation pool and avoids SQLite's variable limit.
func (r *Repository) ShotTranscriptSpansBatch(ctx context.Context, requests []search.TranscriptSpanRequest) (map[string][]domain.AlignmentWord, error) {
	out := make(map[string][]domain.AlignmentWord, len(requests))
	if len(requests) == 0 {
		return out, nil
	}
	payload := make([]transcriptSpanBatchRequest, 0, len(requests))
	for i, request := range requests {
		payload = append(payload, transcriptSpanBatchRequest{
			Index:   i,
			ShotID:  request.ShotID,
			AssetID: request.AssetID,
			StartMS: request.StartMS,
			EndMS:   request.EndMS,
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
WITH requested AS (
    SELECT
        CAST(json_extract(value, '$.index') AS INTEGER) AS request_index,
        json_extract(value, '$.shot_id') AS shot_id,
        json_extract(value, '$.asset_id') AS asset_id,
        CAST(json_extract(value, '$.start_ms') AS INTEGER) AS start_ms,
        CAST(json_extract(value, '$.end_ms') AS INTEGER) AS end_ms
    FROM json_each(?)
),
aligned_assets AS (SELECT DISTINCT asset_id FROM transcript_words),
speech AS (
    SELECT w.asset_id, w.ordinal, w.start_ms, w.end_ms, w.text, w.confidence, 'aligned' AS source
    FROM transcript_words w JOIN aligned_assets a ON a.asset_id = w.asset_id
    UNION ALL
    SELECT s.asset_id, s.ordinal, s.start_ms, s.end_ms, s.text, NULL, 'asr'
    FROM asr_segments s LEFT JOIN aligned_assets a ON a.asset_id = s.asset_id
    WHERE a.asset_id IS NULL
)
SELECT requested.shot_id, words.start_ms, words.end_ms, words.text, words.confidence, words.source
FROM requested
JOIN speech words
  ON words.asset_id = requested.asset_id
 AND words.start_ms < requested.end_ms
 AND words.end_ms > requested.start_ms
ORDER BY requested.request_index, words.ordinal`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var shotID, source string
		var word domain.AlignmentWord
		var confidence sql.NullFloat64
		if err := rows.Scan(&shotID, &word.StartMS, &word.EndMS, &word.Text, &confidence, &source); err != nil {
			return nil, err
		}
		if confidence.Valid {
			value := confidence.Float64
			word.Confidence = &value
		}
		if source == "asr" {
			// ASR segments are sentence-level; expand to per-rune spans so
			// the exact-phrase validator can match a sub-phrase, matching the
			// per-candidate ShotTranscriptSpans.
			out[shotID] = expandAsrSegments(out[shotID], word)
			continue
		}
		out[shotID] = append(out[shotID], word)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type transcriptSpanBatchRequest struct {
	Index   int    `json:"index"`
	ShotID  string `json:"shot_id"`
	AssetID string `json:"asset_id"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
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

// NeighborShotsBatch returns the previous and next shot for each request in a
// single SQLite query. Ordinals need not be contiguous: each side is selected
// with a strict range predicate and an ordered LIMIT 1, matching
// NeighborShots exactly. Missing sides are left nil; requests with no sides
// are omitted from the returned map.
func (r *Repository) NeighborShotsBatch(ctx context.Context, requests []search.NeighborRequest) (map[string]search.Neighbors, error) {
	out := make(map[string]search.Neighbors, len(requests))
	if len(requests) == 0 {
		return out, nil
	}
	payload := make([]neighborBatchRequest, 0, len(requests))
	for i, request := range requests {
		payload = append(payload, neighborBatchRequest{
			Index:   i,
			ShotID:  request.ShotID,
			AssetID: request.AssetID,
			Ordinal: request.Ordinal,
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `
WITH requested AS (
    SELECT
        CAST(json_extract(value, '$.index') AS INTEGER) AS request_index,
        json_extract(value, '$.shot_id') AS shot_id,
        json_extract(value, '$.asset_id') AS asset_id,
        CAST(json_extract(value, '$.ordinal') AS INTEGER) AS ordinal
    FROM json_each(?)
), adjacent AS (
    SELECT requested.request_index, requested.shot_id, 'previous' AS side, shots.id,
	           shots.asset_id, COALESCE(shots.source_run_id,'') AS source_run_id, shots.ordinal,
           shots.start_ms, shots.end_ms, shots.description, shots.tags_json,
           shots.objects_json, shots.actions_json, shots.mood_json,
           shots.confidence, shots.created_at
    FROM requested
    JOIN asset_shots shots ON shots.id = (
        SELECT previous.id
        FROM asset_shots previous
        WHERE previous.asset_id = requested.asset_id
          AND previous.ordinal < requested.ordinal
        ORDER BY previous.ordinal DESC
        LIMIT 1
    )
    UNION ALL
    SELECT requested.request_index, requested.shot_id, 'next' AS side, shots.id,
	           shots.asset_id, COALESCE(shots.source_run_id,'') AS source_run_id, shots.ordinal,
           shots.start_ms, shots.end_ms, shots.description, shots.tags_json,
           shots.objects_json, shots.actions_json, shots.mood_json,
           shots.confidence, shots.created_at
    FROM requested
    JOIN asset_shots shots ON shots.id = (
        SELECT next_shot.id
        FROM asset_shots next_shot
        WHERE next_shot.asset_id = requested.asset_id
          AND next_shot.ordinal > requested.ordinal
        ORDER BY next_shot.ordinal ASC
        LIMIT 1
    )
)
SELECT request_index, shot_id, side, id, asset_id, source_run_id, ordinal,
       start_ms, end_ms, description, tags_json, objects_json, actions_json,
       mood_json, confidence, created_at
FROM adjacent
ORDER BY request_index, side`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var requestIndex int
		var shotID, side string
		row, err := scanAssetShotBatchRow(rows, &requestIndex, &shotID, &side)
		if err != nil {
			return nil, err
		}
		neighbors := out[shotID]
		if side == "previous" {
			neighbors.Previous = row
		} else {
			neighbors.Next = row
		}
		out[shotID] = neighbors
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type neighborBatchRequest struct {
	Index   int    `json:"index"`
	ShotID  string `json:"shot_id"`
	AssetID string `json:"asset_id"`
	Ordinal int    `json:"ordinal"`
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

func scanAssetShotBatchRow(row interface{ Scan(...any) error }, requestIndex *int, shotID, side *string) (*domain.AssetShot, error) {
	var shot domain.AssetShot
	var tags, objects, actions, mood, created string
	if err := row.Scan(requestIndex, shotID, side, &shot.ID, &shot.AssetID, &shot.SourceRunID, &shot.Ordinal, &shot.StartMS, &shot.EndMS, &shot.Description, &tags, &objects, &actions, &mood, &shot.Confidence, &created); err != nil {
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
