package search

import (
	"context"
	"hash/fnv"
	"math"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
)

// fakeEmbedder is a deterministic, offline text embedder: each token hashes
// into a 256-dimension vector, distinct from the discovery FNV heuristic, so
// tests can prove the embedding channel contributes an orthogonal signal.
type fakeEmbedder struct {
	dim int
}

func (f fakeEmbedder) Name() string  { return "fake" }
func (f fakeEmbedder) Model() string { return "fake-embed-v1" }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i, text := range texts {
		out[i] = fakeVector(text, f.dim)
	}
	return out, nil
}

func fakeVector(text string, dim int) []float64 {
	vector := make([]float64, dim)
	seen := map[string]bool{}
	for _, token := range fakeTokens(text) {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		h := fnv.New64a()
		_, _ = h.Write([]byte(token))
		sum := h.Sum64()
		index := int(sum % uint64(dim))
		if sum&1 == 0 {
			vector[index] += 1
		} else {
			vector[index] -= 1
		}
	}
	var norm float64
	for _, v := range vector {
		norm += v * v
	}
	if norm == 0 {
		return vector
	}
	scale := 1 / math.Sqrt(norm)
	for i := range vector {
		vector[i] *= scale
	}
	return vector
}

func fakeTokens(text string) []string {
	// Whole ASCII words plus CJK bigrams, deterministically.
	var out []string
	current := []rune{}
	flush := func() {
		runes := current
		current = nil
		if len(runes) == 0 {
			return
		}
		word := string(runes)
		if len(runes) > 3 {
			for i := 0; i+1 < len(runes); i++ {
				out = append(out, string(runes[i:i+2]))
			}
			return
		}
		out = append(out, word)
	}
	for _, r := range text {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '\u3040' && r <= '\u9fff' {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return out
}

func fakeVector32(text string, dim int) []float32 {
	vector := fakeVector(text, dim)
	out := make([]float32, len(vector))
	for i, v := range vector {
		out[i] = float32(v)
	}
	return out
}

func TestEmbeddingRetrieverRanksByCosine(t *testing.T) {
	store := &fakeStore{
		embeddingRows: map[string][]ShotEmbeddingRow{
			"fake-embed-v1": {
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-car", Description: "car crossing the street"}}, Model: "fake-embed-v1", Vector: fakeVector32("car crossing the street", 256), SourceTextHash: "h1"},
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-dog", Description: "dog playing in the park"}}, Model: "fake-embed-v1", Vector: fakeVector32("dog playing in the park", 256), SourceTextHash: "h2"},
				{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s-empty", Description: "empty room"}}, Model: "fake-embed-v1", Vector: fakeVector32("empty room", 256), SourceTextHash: "h3"},
			},
		},
	}
	retriever := NewTextEmbeddingRetriever(store, fakeEmbedder{dim: 256})
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("embedding channel must return candidates when vectors exist")
	}
	if candidates[0].ShotID != "s-car" {
		t.Fatalf("car query must rank the car shot first, got %+v", candidates)
	}
	if candidates[0].Signals[SignalTextEmbedding] <= 0 {
		t.Fatalf("embedding signal must be positive, got %+v", candidates[0].Signals)
	}
}

func TestEmbeddingRetrieverDegradesWithoutVectors(t *testing.T) {
	store := &fakeStore{} // no rows under the model
	retriever := NewTextEmbeddingRetriever(store, fakeEmbedder{dim: 256})
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("no stored vectors must mean no candidates, got %d", len(candidates))
	}
}

func TestEmbeddingRetrieverNilEmbedderNoOp(t *testing.T) {
	store := &fakeStore{embeddingRows: map[string][]ShotEmbeddingRow{
		"x": {{Shot: domain.ShotSearchResult{AssetShot: domain.AssetShot{ID: "s1", Description: "anything"}}, Model: "x", Vector: fakeVector32("anything", 256)}},
	}}
	retriever := NewTextEmbeddingRetriever(store, nil)
	candidates, err := retriever.Retrieve(context.Background(), Compile("car"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("nil embedder must mean no candidates, got %d", len(candidates))
	}
}

func TestShotTextSourceAndHash(t *testing.T) {
	doc := ShotSearchDocument{ShotID: "s1", Description: "red car", Tags: []string{"traffic"}, Objects: []string{"car"}}
	source := ShotTextSource(doc)
	if source != "red car traffic car" {
		t.Fatalf("source = %q", source)
	}
	if ShotTextSourceHash(doc) != ShotTextSourceHash(doc) {
		t.Fatal("hash must be deterministic")
	}
	changed := doc
	changed.Objects = []string{"truck"}
	if ShotTextSourceHash(changed) == ShotTextSourceHash(doc) {
		t.Fatal("changed derived text must change the hash")
	}
}
