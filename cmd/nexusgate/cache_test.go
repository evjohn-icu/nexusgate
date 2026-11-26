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

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	sqliterepo "github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
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
	repo, err := sqliterepo.Open(filepath.Join(dataDir, "nexusgate.db"))
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

// TestCacheGCOrphansFixtureRemovesOrphanOnly is the command-surface
// acceptance for `--orphans --yes`: one real orphan asset directory is
// removed while the managed search-references tree, the active inference
// scratch and known assets all survive — and no job is enqueued for a mode
// that deletes nothing rebuildable.
func TestCacheGCOrphansFixtureRemovesOrphanOnly(t *testing.T) {
	repo, cfg, assets := cacheGCTestRepo(t, 1)
	for _, assetID := range assets {
		writeCacheArtifact(t, cfg.CacheDir, assetID, 0)
	}
	orphan := filepath.Join(cfg.CacheDir, "orphan-asset")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "odd.bin"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(cfg.CacheDir, "search-references", "abc.jpg")
	if err := os.MkdirAll(filepath.Dir(reference), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reference, []byte("query-image"), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(cfg.CacheDir, "inference", "inputs", "job1", "in.bin")
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scratch, []byte("active-scratch"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runCacheGC(context.Background(), repo, cfg, []string{"--orphans", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "freed") {
		t.Fatalf("orphan run must report the deletion: %s", out)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("the orphan directory must be removed, stat err = %v", err)
	}
	if _, err := os.Stat(reference); err != nil {
		t.Fatalf("the 24-hour query image must survive: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("live inference scratch must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, assets[0], "proxy-sw.mp4")); err != nil {
		t.Fatalf("a known asset's directory must survive --orphans: %v", err)
	}
	var jobs int
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("an orphan run must not enqueue rebuilds, jobs=%d", jobs)
	}
}

// TestCacheGCProtectsAssetsWithQueuedWork pins queued protection through the
// command surface: an asset with a nonterminal pending job keeps its
// rebuildable artifacts in a --rebuildable --yes run, while the unprotected
// asset is cleaned and re-enqueued as usual.
func TestCacheGCProtectsAssetsWithQueuedWork(t *testing.T) {
	repo, cfg, assets := cacheGCTestRepo(t, 2)
	for i, assetID := range assets {
		writeCacheArtifact(t, cfg.CacheDir, assetID, i)
	}
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err := repo.DB().Exec(`INSERT INTO jobs(id,asset_id,job_type,state,priority,attempt_count,max_attempts,run_after,input_hash,created_at,updated_at) VALUES(?,?,'derive','pending',10,0,3,?,?,?,?)`,
		"job-queued", assets[0], stamp, "hash", stamp, stamp); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runCacheGC(context.Background(), repo, cfg, []string{"--rebuildable", "--yes"}); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, assets[0], "proxy-sw.mp4")); err != nil {
		t.Fatalf("a queued asset's artifact must be protected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, assets[1], "proxy-sw.mp4")); !os.IsNotExist(err) {
		t.Fatalf("the unprotected asset's artifact must be removed, stat err = %v", err)
	}
	if !strings.Contains(out, "skipped active") {
		t.Fatalf("the report must account the protected artifact: %s", out)
	}
	var jobs int
	if err := repo.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE job_type='derive'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 2 {
		t.Fatalf("one pre-existing + one new re-derive job expected, jobs=%d", jobs)
	}
}
