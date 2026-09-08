package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The default must stay empty. A non-empty default would mean shipping a LUT
// or pointing at one that may not exist, and the renderer's contract is that an
// empty path is reported as "not configured" rather than silently skipped.
func TestLoadPreviewLUTPathDefaultsToEmpty(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_PREVIEW_LUT_PATH", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewLUTPath != "" {
		t.Fatalf("preview lut path=%q want empty", cfg.PreviewLUTPath)
	}
}

func TestLoadReadsPreviewLUTPathFromEnvironment(t *testing.T) {
	t.Setenv("NEXUSGATE_DATA_DIR", t.TempDir())
	t.Setenv("NEXUSGATE_PREVIEW_LUT_PATH", "  /mnt/luts/apple-log-to-sdr.cube  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewLUTPath != "/mnt/luts/apple-log-to-sdr.cube" {
		t.Fatalf("preview lut path=%q want trimmed /mnt/luts/apple-log-to-sdr.cube", cfg.PreviewLUTPath)
	}
}

// Pins the v != "" semantics the whole environment-override block uses: an
// empty or whitespace-only variable means "unset", so it must not erase a value
// that came from config.json. A docker template that always emits the variable
// with an empty value would otherwise wipe the operator's LUT.
func TestLoadBlankPreviewLUTPathEnvironmentPreservesConfiguredValue(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		t.Run(raw, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
			t.Setenv("NEXUSGATE_PREVIEW_LUT_PATH", raw)
			if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"preview_lut_path":"/srv/luts/custom.cube"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PreviewLUTPath != "/srv/luts/custom.cube" {
				t.Fatalf("preview lut path=%q; blank environment must not clear the configured value", cfg.PreviewLUTPath)
			}
		})
	}
}

func TestLoadReadsPreviewLUTPathFromConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("NEXUSGATE_DATA_DIR", dataDir)
	t.Setenv("NEXUSGATE_PREVIEW_LUT_PATH", "")
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(`{"preview_lut_path":"/srv/luts/from-file.cube"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewLUTPath != "/srv/luts/from-file.cube" {
		t.Fatalf("preview lut path=%q want /srv/luts/from-file.cube", cfg.PreviewLUTPath)
	}
}
