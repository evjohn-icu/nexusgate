package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

func TestPlanAssetModalitiesBuildsOnlyEligibleTasks(t *testing.T) {
	ctx := context.Background()
	dir := secureDataDir(t)
	repo, err := sqlite.Open(filepath.Join(dir, "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, statement := range []string{
		`INSERT INTO library_roots(id,path,created_at,updated_at) VALUES('root','/media','` + now + `','` + now + `')`,
		`INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES('asset','fp',100,'ready','` + now + `','` + now + `')`,
		`INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES('loc','asset','root','clip.mp4','/media/clip.mp4',1,1,1,'` + now + `')`,
		`INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at,duration_ms) VALUES('asset','{}','{}','{"duration_ms":10000}','test','` + now + `',10000)`,
		`INSERT INTO asset_shots(id,asset_id,source_run_id,ordinal,start_ms,end_ms,description,tags_json,objects_json,actions_json,mood_json,confidence,created_at) VALUES('shot-1','asset',NULL,0,0,5000,'one','[]','[]','[]','[]',0.9,'` + now + `')`,
	} {
		if _, err := repo.DB().ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewService(repo, config.Config{
		DataDir: dir, CacheDir: filepath.Join(dir, "cache"),
		Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpVisualEmbedding] = true
	settings.Enabled[domain.OpSoundEvents] = true
	if err := repo.PutMultimodalSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	tasks, err := service.PlanAssetModalities(ctx, "asset", "input-hash")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Operation != domain.OpVisualEmbedding {
		t.Fatalf("planned tasks = %+v", tasks)
	}
	if _, ok, err := service.PlanAssetOperation(ctx, "asset", "input-hash", domain.OpSoundEvents, true); err != nil || ok {
		t.Fatalf("sound events without audio planned: ok=%v err=%v", ok, err)
	}
	if _, ok, err := service.PlanAssetOperation(ctx, "asset", "input-hash", domain.OpSpeakerEmbedding, true); err != nil || ok {
		t.Fatalf("speaker planned automatically: ok=%v err=%v", ok, err)
	}
}
