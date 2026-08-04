package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// TestLexicalScoreFromBM25Boundaries pins the bm25()->lexical-score
// conversion at its decisive inputs. A non-negative rank — "matched, but
// with no discriminating power" — must score 0, not the 1 a naive
// 1/(1+|rank|) produces at rank 0. bm25HalfScore is asserted directly
// because it is the one number in the conversion that is a judgement call
// rather than arithmetic: changing what counts as a half-marks match is a
// retrieval decision, and it should not be possible to make it silently.
func TestLexicalScoreFromBM25Boundaries(t *testing.T) {
	cases := []struct {
		name string
		rank float64
		want float64
	}{
		{"half marks land on the documented saturation point", -bm25HalfScore, 0.5},
		{"clearly relevant match", -1, 1.0 / 3.2},
		{"zero rank must not be full marks", 0, 0},
		{"positive rank (should not occur, but must not score) ", 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lexicalScoreFromBM25(c.rank); got != c.want {
				t.Fatalf("lexicalScoreFromBM25(%v) = %v, want %v", c.rank, got, c.want)
			}
		})
	}
}

// TestHybridSearchLexicalScoresStayPositiveForGenuineMatches guards the
// blast radius of the zero-rank clamp: sending a non-negative rank to 0 must
// not spill onto ordinary negative-rank matches away from that boundary, and
// no match may reach 1 either, since bm25 is unbounded and full marks would
// claim a ceiling the signal does not have. The ordering between these two
// shots is asserted separately, below.
func TestHybridSearchLexicalScoresStayPositiveForGenuineMatches(t *testing.T) {
	ctx := context.Background()
	repo := newBeaconCorpus(t)

	hits, err := repo.HybridSearchShots(ctx, "beacon", 10)
	if err != nil {
		t.Fatal(err)
	}
	var weak, strong *domain.ShotSearchResult
	for i := range hits {
		switch hits[i].ID {
		case "shot-weak":
			weak = &hits[i]
		case "shot-strong":
			strong = &hits[i]
		}
	}
	if weak == nil || strong == nil {
		t.Fatalf("expected both shot-weak and shot-strong in hits: %+v", hits)
	}
	for _, hit := range []*domain.ShotSearchResult{weak, strong} {
		if hit.LexicalScore <= 0 || hit.LexicalScore >= 1 {
			t.Fatalf("a genuine (non-floored) match must score in (0,1): %+v", hit)
		}
	}
}

// newBeaconCorpus builds the ten-shot library both integration tests below
// score against. "beacon" appears in exactly 2 of the 10 shots, so its
// document frequency is low enough that bm25's IDF term is a real positive
// number rather than FTS5's near-zero floor (see the doc comment on
// lexicalScoreFromBM25) — that isolates term frequency as the only thing
// distinguishing the two matches, and the eight neutral shots exist only to
// hold that frequency down. Writes go through ReplaceAssetShots rather than
// straight into the FTS table so the rows carry the same tokenization and
// the same semantic vectors the pipeline would produce.
func newBeaconCorpus(t *testing.T) *Repository {
	t.Helper()
	ctx := context.Background()
	repo, err := Open(filepath.Join(t.TempDir(), "timingdex-lexical-strength.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	assets := []string{"asset-weak", "asset-strong"}
	for i := 0; i < 8; i++ {
		assets = append(assets, fmt.Sprintf("asset-neutral-%d", i))
	}
	for _, a := range assets {
		if _, err := repo.db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,100,'discovered',?,?)`, a, a+"-fp", now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-weak", "", []domain.AssetShot{{ID: "shot-weak", StartMS: 0, EndMS: 1000, Description: "beacon on the coast"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-strong", "", []domain.AssetShot{{ID: "shot-strong", StartMS: 0, EndMS: 1000, Description: "beacon beacon beacon signal beacon tower"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("shot-neutral-%d", i)
		if err := repo.ReplaceAssetShots(ctx, fmt.Sprintf("asset-neutral-%d", i), "", []domain.AssetShot{{ID: id, StartMS: 0, EndMS: 1000, Description: "unrelated content about weather"}}); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// TestLexicalScoreFromBM25IsMonotonicInMatchStrength is the direction test.
// hybridSearchShots ADDS the lexical term to the semantic one, so the term
// has to point the same way as a score: stronger match, larger number. bm25
// points the other way — more negative is more relevant, which is why this
// package's own searchShots sorts ORDER BY bm25(...) ascending — so the
// conversion is the only place the direction can be corrected, and if it
// gets it wrong every weaker match outranks every stronger one.
func TestLexicalScoreFromBM25IsMonotonicInMatchStrength(t *testing.T) {
	// Ordered weakest match to strongest, i.e. bm25 descending toward zero.
	ranks := []float64{0, -0.1, -1, -2.2, -5, -40}
	for i := 1; i < len(ranks); i++ {
		weaker, stronger := lexicalScoreFromBM25(ranks[i-1]), lexicalScoreFromBM25(ranks[i])
		if !(stronger > weaker) {
			t.Fatalf("bm25 %v is a stronger match than %v, so it must score higher: got %v <= %v", ranks[i], ranks[i-1], stronger, weaker)
		}
		if stronger >= 1 {
			t.Fatalf("bm25 has no upper bound, so no rank may reach full marks: lexicalScoreFromBM25(%v) = %v", ranks[i], stronger)
		}
	}
}

// TestHybridSearchLexicalScoreAgreesWithBM25Ordering is the same direction
// claim, but end to end through real FTS5 instead of hand-picked ranks: the
// lexical dimension of the hybrid blend must rank the same corpus the same
// way SQLite's own bm25 ordering does. searchShots is the reference because
// it consumes bm25 directly in SQL and never passes through the conversion,
// so a disagreement between the two is proof the conversion inverted it.
func TestHybridSearchLexicalScoreAgreesWithBM25Ordering(t *testing.T) {
	ctx := context.Background()
	repo := newBeaconCorpus(t)

	ordered, err := repo.SearchShots(ctx, "beacon", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 {
		t.Fatalf("expected exactly the two beacon shots from the reference ordering, got %+v", ordered)
	}
	if ordered[0].ID != "shot-strong" {
		t.Fatalf("reference ordering itself is not what this test assumes: %v then %v", ordered[0].ID, ordered[1].ID)
	}

	hits, err := repo.HybridSearchShots(ctx, "beacon", 10)
	if err != nil {
		t.Fatal(err)
	}
	lexical := map[string]float64{}
	for _, hit := range hits {
		lexical[hit.ID] = hit.LexicalScore
	}
	for i := 1; i < len(ordered); i++ {
		better, worse := ordered[i-1].ID, ordered[i].ID
		if !(lexical[better] > lexical[worse]) {
			t.Fatalf("bm25 ranks %s above %s, so its lexical score must be higher: %s=%v %s=%v", better, worse, better, lexical[better], worse, lexical[worse])
		}
	}
}
