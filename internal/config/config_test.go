package config

import "testing"

func TestLoadReadsSourceStagingModeFromEnvironment(t *testing.T) {
	t.Setenv("TIMINGDEX_DATA_DIR", t.TempDir())
	t.Setenv("TIMINGDEX_SOURCE_STAGING_MODE", "copy")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourceStaging.Mode != "copy" {
		t.Fatalf("source staging mode=%q want copy", cfg.SourceStaging.Mode)
	}
}
