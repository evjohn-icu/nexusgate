package staging

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyStagesSourceLocallyAndReusesCompleteCache(t *testing.T) {
	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "nas-footage.mov")
	original := []byte("source bytes must stay on the NAS")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	stager, err := New("copy", filepath.Join(t.TempDir(), "nexusslate-cache"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	if first == sourcePath || !strings.Contains(first, string(filepath.Separator)+"sources"+string(filepath.Separator)+"asset-1"+string(filepath.Separator)) {
		t.Fatalf("staged path=%q, want a local source-cache path", first)
	}
	staged, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != string(original) {
		t.Fatalf("staged=%q want=%q", staged, original)
	}

	second, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second stage=%q want cache reuse at %q", second, first)
	}
	current, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("source was changed: got=%q want=%q", current, original)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatal(err)
	}
	afterDisconnect, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatalf("completed local cache should remain usable after source disconnect: %v", err)
	}
	if afterDisconnect != first {
		t.Fatalf("after disconnect path=%q want cached %q", afterDisconnect, first)
	}
}

func TestNoneModeLeavesSourceInPlace(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "local-footage.mp4")
	if err := os.WriteFile(sourcePath, []byte("local source"), 0o600); err != nil {
		t.Fatal(err)
	}
	stager, err := New("none", filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1")
	if err != nil {
		t.Fatal(err)
	}
	if got != sourcePath {
		t.Fatalf("none mode path=%q want original %q", got, sourcePath)
	}
}

func TestNewRejectsUnknownSourceStagingMode(t *testing.T) {
	if _, err := New("mirror_nas", t.TempDir()); err == nil {
		t.Fatal("unknown staging mode should be rejected")
	}
}

func TestCopyRejectsSymlinkDestination(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.mov")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	destinationDir := filepath.Join(cacheDir, "sources", "asset-1")
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationDir, "input-v1.mov")
	target := filepath.Join(t.TempDir(), "outside.mov")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}
	stager, err := New("copy", cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stager.Stage(context.Background(), sourcePath, "asset-1", "input-v1"); err == nil {
		t.Fatal("staging should reject a symlink destination")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside" {
		t.Fatalf("symlink target changed to %q", got)
	}
}
