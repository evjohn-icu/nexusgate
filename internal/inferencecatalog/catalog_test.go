package inferencecatalog

import (
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/domain"
)

func TestCatalogLookupsAndDigest(t *testing.T) {
	all := All()
	if len(all) < 7 {
		t.Fatalf("catalog has %d models", len(all))
	}
	all[0].CatalogID = "mutated"
	if All()[0].CatalogID == "mutated" {
		t.Fatal("All returned the backing slice")
	}
	visual, ok := Lookup("visual-siglip2-base")
	if !ok || visual.Dimension != 768 {
		t.Fatalf("Lookup visual = %+v %v", visual, ok)
	}
	if _, ok := Lookup("missing-model"); ok {
		t.Fatal("unknown catalog id resolved")
	}
	byIdentity, ok := LookupByModel(visual.ModelID, visual.Revision)
	if !ok || byIdentity.CatalogID != visual.CatalogID {
		t.Fatalf("LookupByModel = %+v %v", byIdentity, ok)
	}
	if _, ok := LookupByModel(visual.ModelID, "wrong"); ok {
		t.Fatal("wrong revision resolved")
	}
	models := ForOperation(domain.OpSpeakerEmbedding)
	if len(models) != 2 {
		t.Fatalf("speaker models = %d", len(models))
	}
	defaultVisual, ok := DefaultFor(domain.OpVisualEmbedding)
	if !ok || defaultVisual.CatalogID != "visual-siglip2-base" {
		t.Fatalf("DefaultFor visual = %+v %v", defaultVisual, ok)
	}
	if _, ok := DefaultFor(domain.InferenceOperation("unknown")); ok {
		t.Fatal("unknown operation has a default")
	}
	if err := visual.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(visual.ManifestDigest(), "sha256:") || len(visual.ManifestDigest()) != 71 {
		t.Fatalf("manifest digest = %q", visual.ManifestDigest())
	}
	if visual.ManifestDigest() != byIdentity.ManifestDigest() {
		t.Fatal("same identity produced different digests")
	}
	broken := visual
	broken.Dimension = 0
	if err := broken.Validate(); err == nil {
		t.Fatal("dimensionless model validated")
	}
}
