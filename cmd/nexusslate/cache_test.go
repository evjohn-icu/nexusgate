package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	sqliterepo "github.com/evjohn-icu/nexusslate/internal/repository/sqlite"
)

func TestCacheGCRequeuesEveryRemovedRebuildableAsset(t *testing.T) {
	repo, cfg, assets := cacheGCTestRepo(t, 25)
	for i, assetID := range assets {
		writeCacheArtifact(t, cfg.CacheDir, assetID, i)
	}

	out := captureStdout(t, func() {
		if err := runCacheGC(context.Background(), repo, cfg, []string{"--rebuildable", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "showing first 20 of 25") {
		t.Fatalf("GC output does not identify capped report: %s", out)
	}
	var jobs int
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE job_type='derive'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 25 {
		t.Fatalf("re-derive jobs = %d, want 25", jobs)
	}
}

func TestCacheGCDryRunIsPure(t *testing.T) {
	repo, cfg, assets := cacheGCTestRepo(t, 25)
	for i, assetID := range assets {
		writeCacheArtifact(t, cfg.CacheDir, assetID, i)
	}

	out := captureStdout(t, func() {
		if err := runCacheGC(context.Background(), repo, cfg, []string{"--rebuildable"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "dry run: nothing deleted") {
		t.Fatalf("dry-run output = %s", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, ".cache-maintenance.lock")); !os.IsNotExist(err) {
		t.Fatalf("dry run created maintenance lock: %v", err)
	}
	var rows, jobs int
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM derived_artifacts`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if rows != 25 || jobs != 0 {
		t.Fatalf("dry run changed database: artifacts=%d jobs=%d", rows, jobs)
	}
	for _, assetID := range assets {
		if _, err := os.Stat(filepath.Join(cfg.CacheDir, assetID, "proxy-sw.mp4")); err != nil {
			t.Fatalf("dry run removed %s: %v", assetID, err)
		}
	}
}

func cacheGCTestRepo(t *testing.T, count int) (*sqliterepo.Repository, config.Config, []string) {
	t.Helper()
	dataDir := t.TempDir()
	repo, err := sqliterepo.Open(filepath.Join(dataDir, "nexusslate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	assets := make([]string, count)
	for i := range assets {
		id := fmt.Sprintf("asset-cache-%02d", i)
		assets[i] = id
		if _, err := repo.DB().Exec(`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,1,'ready',?,?)`, id, id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.DB().Exec(`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES(?,?,?,?)`, "root-"+id, "/tmp/"+id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.DB().Exec(`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "loc-"+id, id, "root-"+id, "clip.mov", "/tmp/"+id+".mov", now); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.DB().Exec(`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at) VALUES(?,'{}','{}',?,'test',?)`, id, `{"duration_ms":1000,"has_audio":false}`, now); err != nil {
			t.Fatal(err)
		}
		runID, _, err := repo.CreateModelRun(context.Background(), id, "vision", "test", "test", "hash-"+id, "p", "s", "{}", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.StageModelRun(context.Background(), runID, "{}", "{}", "", ""); err != nil {
			t.Fatal(err)
		}
		analysis := domain.StructuredAnalysis{AssetType: "clip", ShotSize: "close", CameraMotion: "static", AudioType: "none", Lighting: "day", Quality: "fine", Summary: "summary"}
		shot := domain.AssetShot{ID: "shot-" + id, AssetID: id, SourceRunID: runID, EndMS: 1000, Description: "shot", Confidence: 0.9}
		if err := repo.CommitAnalysisWithShots(context.Background(), id, runID, "s", analysis, []domain.AssetShot{shot}, "", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.DB().Exec(`INSERT INTO derived_artifacts(id,asset_id,artifact_type,profile_hash,local_path,size_bytes,created_at) VALUES(?,?,?,?,?,?,?)`, "artifact-"+id, id, "proxy", "proxy-sw", filepath.Join(cfgCacheDir(dataDir), id, "proxy-sw.mp4"), 1, now); err != nil {
			t.Fatal(err)
		}
	}
	return repo, config.Config{DataDir: dataDir, CacheDir: cfgCacheDir(dataDir)}, assets
}

func cfgCacheDir(dataDir string) string { return filepath.Join(dataDir, "cache") }

func writeCacheArtifact(t *testing.T, cacheDir, assetID string, index int) {
	t.Helper()
	dir := filepath.Join(cacheDir, assetID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "proxy-sw.mp4")
	if err := os.WriteFile(p, []byte{byte(index)}, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	out := <-done
	_ = r.Close()
	return out
}
