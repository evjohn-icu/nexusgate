package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/inferencecatalog"
)

func TestInferenceTaskKeyChangesWithEverySemanticInput(t *testing.T) {
	base := InferenceTaskKey{
		Operation:      domain.OpVisualEmbedding,
		SourceIdentity: "sha256:asset",
		ModelID:        "google/siglip2-base-patch16-224",
		ModelRevision:  "rev-a",
		ConfigHash:     "cfg-a",
		SpaceID:        "space-a",
	}
	key, err := base.Key()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if other, _ := base.Key(); other != key {
		t.Fatal("key must be deterministic")
	}
	mutations := map[string]func(*InferenceTaskKey){
		"operation":      func(k *InferenceTaskKey) { k.Operation = domain.OpDiarization },
		"source":         func(k *InferenceTaskKey) { k.SourceIdentity = "sha256:other" },
		"model id":       func(k *InferenceTaskKey) { k.ModelID = "tencent/WeMM-Embedding-2B" },
		"model revision": func(k *InferenceTaskKey) { k.ModelRevision = "rev-b" },
		"config":         func(k *InferenceTaskKey) { k.ConfigHash = "cfg-b" },
		"space":          func(k *InferenceTaskKey) { k.SpaceID = "space-b" },
		"consent":        func(k *InferenceTaskKey) { k.ConsentGeneration = 1 },
		"profile":        func(k *InferenceTaskKey) { k.ProfileGeneration = 1 },
	}
	for name, mutate := range mutations {
		changed := base
		mutate(&changed)
		got, err := changed.Key()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == key {
			t.Errorf("%s must change the task key", name)
		}
	}
	broken := base
	broken.ModelRevision = ""
	if _, err := broken.Key(); err == nil {
		t.Fatal("a key without an immutable revision must be rejected")
	}
}

func TestCanonicalSpaceIDDependsOnEveryIdentityField(t *testing.T) {
	space := domain.EmbeddingSpace{
		Modality:          domain.ModalityVisual,
		Provider:          "huggingface",
		ModelID:           "google/siglip2-base-patch16-224",
		ModelRevision:     "rev-a",
		Dimension:         768,
		Normalization:     "l2",
		PreprocessingJSON: `{"image":{"resize":[224,224]}}`,
		RuntimeDigest:     "torch-cu126",
	}
	id, err := CanonicalSpaceID(space)
	if err != nil {
		t.Fatalf("space id: %v", err)
	}
	if again, _ := CanonicalSpaceID(space); again != id {
		t.Fatal("space id must be deterministic")
	}
	altered := space
	altered.Dimension = 512
	if other, _ := CanonicalSpaceID(altered); other == id {
		t.Fatal("a different dimension is a different space")
	}
	altered = space
	altered.PreprocessingJSON = `{"image":{"resize":[384,384]}}`
	if other, _ := CanonicalSpaceID(altered); other == id {
		t.Fatal("different preprocessing is a different space")
	}
	altered = space
	altered.RuntimeDigest = "torch-cu126-fp16"
	if other, _ := CanonicalSpaceID(altered); other == id {
		t.Fatal("a different runtime digest is a different space")
	}
	if _, err := CanonicalSpaceID(domain.EmbeddingSpace{Dimension: 0}); err == nil {
		t.Fatal("a space without a dimension must be rejected")
	}
}

func TestShotSetHashIgnoresOrderButNotBoundaries(t *testing.T) {
	a := []domain.AssetShot{
		{ID: "b", StartMS: 0, EndMS: 1000},
		{ID: "a", StartMS: 1000, EndMS: 2000},
	}
	b := []domain.AssetShot{
		{ID: "a", StartMS: 1000, EndMS: 2000},
		{ID: "b", StartMS: 0, EndMS: 1000},
	}
	if ShotSetHash(a) != ShotSetHash(b) {
		t.Fatal("shot order must not change the set identity")
	}
	moved := []domain.AssetShot{
		{ID: "b", StartMS: 0, EndMS: 1200},
		{ID: "a", StartMS: 1200, EndMS: 2000},
	}
	if ShotSetHash(a) == ShotSetHash(moved) {
		t.Fatal("moved boundaries must change the set identity")
	}
}

func TestBuildSpaceHonoursDeclaredMatryoshkaOnly(t *testing.T) {
	wemm, ok := inferencecatalog.Lookup("visual-wemm-embedding-2b")
	if !ok {
		t.Fatal("catalog is missing the WeMM entry")
	}
	space, err := buildSpace(domain.OpVisualEmbedding, wemm, 256)
	if err != nil {
		t.Fatalf("declared truncation rejected: %v", err)
	}
	if space.Dimension != 256 {
		t.Fatalf("want 256, got %d", space.Dimension)
	}
	if _, err := buildSpace(domain.OpVisualEmbedding, wemm, 300); err == nil {
		t.Fatal("an undeclared truncation must be rejected")
	}
	siglip, _ := inferencecatalog.Lookup("visual-siglip2-base")
	if _, err := buildSpace(domain.OpVisualEmbedding, siglip, 128); err == nil {
		t.Fatal("a model without MRL must reject truncation")
	}
}

// TestCatalogMirrorMatchesLockFile keeps the Go mirror honest. The lock file is
// the authority the installer and the Python dispatcher read; a drift here
// would make the Hub request a revision the runtime does not have.
func TestCatalogMirrorMatchesLockFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "inference", "models.lock.json"))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	var lock struct {
		SchemaVersion string `json:"schema_version"`
		Models        []struct {
			CatalogID  string   `json:"catalog_id"`
			ModelID    string   `json:"model_id"`
			Revision   string   `json:"revision"`
			Operations []string `json:"operations"`
			Output     struct {
				Dimension int `json:"dimension"`
			} `json:"output"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatalf("parse lock file: %v", err)
	}
	if lock.SchemaVersion != inferencecatalog.LockSchemaVersion {
		t.Fatalf("lock schema %q does not match the mirror's %q", lock.SchemaVersion, inferencecatalog.LockSchemaVersion)
	}
	for _, entry := range lock.Models {
		mirror, ok := inferencecatalog.Lookup(entry.CatalogID)
		if !ok {
			t.Errorf("lock lists %q but the Go mirror does not", entry.CatalogID)
			continue
		}
		if mirror.ModelID != entry.ModelID || mirror.Revision != entry.Revision {
			t.Errorf("%s identity drifted: lock %s@%s, mirror %s@%s",
				entry.CatalogID, entry.ModelID, entry.Revision, mirror.ModelID, mirror.Revision)
		}
		if mirror.Dimension != entry.Output.Dimension {
			t.Errorf("%s dimension drifted: lock %d, mirror %d", entry.CatalogID, entry.Output.Dimension, mirror.Dimension)
		}
		if len(entry.Operations) != len(mirror.Operations) {
			t.Errorf("%s operation count drifted: lock %v, mirror %v", entry.CatalogID, entry.Operations, mirror.Operations)
		}
	}
	if len(lock.Models) != len(inferencecatalog.All()) {
		t.Errorf("mirror has %d models, lock has %d", len(inferencecatalog.All()), len(lock.Models))
	}
}
