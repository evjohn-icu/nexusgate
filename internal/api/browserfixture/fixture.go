//go:build playwrightfixture

// Package browserfixture provides a deterministic, offline browser fixture.
package browserfixture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/evjohn-icu/nexusgate/internal/api"
	"github.com/evjohn-icu/nexusgate/internal/app"
	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/hubtls"
	"github.com/evjohn-icu/nexusgate/internal/media"
	"github.com/evjohn-icu/nexusgate/internal/remote"
	"github.com/evjohn-icu/nexusgate/internal/repository/sqlite"
)

const AdminToken = "pw-admin-fixture"

type Fixture struct {
	Repo           *sqlite.Repository
	Service        *app.Service
	Handler        http.Handler
	Dir            string
	TLSCertificate string
	TLSKey         string
	TLSFingerprint string

	// stop ends the query-encoder loop when Close runs, so the goroutine
	// never touches a closed database. closeOnce makes a double Close safe
	// instead of panicking on a second channel close.
	stop      chan struct{}
	closeOnce sync.Once
}

func New(ctx context.Context) (*Fixture, error) {
	dir, err := os.MkdirTemp("", "nexusgate-playwright-")
	if err != nil {
		return nil, err
	}
	keepDir := false
	defer func() {
		if !keepDir {
			_ = os.RemoveAll(dir)
		}
	}()
	repo, err := sqlite.Open(filepath.Join(dir, "nexusgate.db"))
	if err != nil {
		return nil, err
	}
	if err := repo.Migrate(ctx); err != nil {
		repo.Close()
		return nil, err
	}
	service, err := app.NewService(repo, config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), HubSecurity: config.HubSecurityConfig{AdminToken: AdminToken, AdminAuth: "required"}, Hardware: media.HardwareConfig{Mode: "software", AllowFallback: true}})
	if err != nil {
		repo.Close()
		return nil, err
	}
	// The pending-to-ready reference search needs one enabled query modality:
	// with visual embedding on, an upload enqueues the assetless visual task
	// the encoder loop below answers. The read path never encodes by itself,
	// so without this the page would poll pending forever.
	settings := domain.DefaultMultimodalSettings()
	settings.Enabled[domain.OpVisualEmbedding] = true
	if err := repo.PutMultimodalSettings(ctx, settings); err != nil {
		repo.Close()
		return nil, err
	}
	if err := seed(ctx, repo, dir); err != nil {
		repo.Close()
		return nil, err
	}
	certificate, key, fingerprint, err := hubtls.EnsureSelfSigned(dir)
	if err != nil {
		repo.Close()
		return nil, err
	}
	fixture := &Fixture{
		Repo:           repo,
		Service:        service,
		Handler:        api.NewTLSServer("127.0.0.1:4173", service, certificate, key).Handler(),
		Dir:            dir,
		TLSCertificate: certificate,
		TLSKey:         key,
		TLSFingerprint: fingerprint,
		stop:           make(chan struct{}),
	}
	go fixture.encodeQueryLoop()
	keepDir = true
	return fixture, nil
}

func (f *Fixture) Close() error {
	if f.stop != nil {
		f.closeOnce.Do(func() { close(f.stop) })
	}
	var closeErr error
	if f.Repo != nil {
		closeErr = f.Repo.Close()
		f.Repo = nil
	}
	removeErr := os.RemoveAll(f.Dir)
	return errors.Join(closeErr, removeErr)
}

// encodeQueryLoop stands in for the Hub's inference worker in the browser
// fixture. The real Hub never encodes a query image on the read path; a
// Worker leases the assetless visual task and commits the staged vector. The
// loop does exactly that against the repository, with a deterministic unit
// vector, so the browser suite can drive the page's pending-to-ready search
// without a network provider or model download. The repository reference is
// captured once: Close nils the fixture's field while a tick already in
// flight is still running, and a closed database only reports an error.
func (f *Fixture) encodeQueryLoop() {
	repo := f.Repo
	if repo == nil {
		return
	}
	stop := f.stop
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	ctx := context.Background()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			f.encodePendingQueries(ctx, repo)
		}
	}
}

func (f *Fixture) encodePendingQueries(ctx context.Context, repo *sqlite.Repository) {
	rows, err := repo.DB().QueryContext(ctx, `SELECT t.request_json FROM inference_tasks t JOIN jobs j ON j.id=t.job_id WHERE t.query_reference_id<>'' AND j.job_type='inference' AND j.state='pending' AND t.cancel_requested=0 LIMIT 8`)
	if err != nil {
		return
	}
	type leaseKey struct{ op, model, revision, config, space string }
	caps := make([]remote.InferenceCapability, 0, 4)
	seen := make(map[leaseKey]bool, 4)
	var scanErr error
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			scanErr = err
			break
		}
		var request domain.InferenceRequest
		if json.Unmarshal([]byte(raw), &request) != nil {
			continue
		}
		key := leaseKey{string(request.Operation), request.ModelID, request.ModelRevision, request.ConfigHash, request.SpaceID}
		if seen[key] {
			continue
		}
		seen[key] = true
		caps = append(caps, remote.InferenceCapability{
			Operation:     request.Operation,
			ModelID:       request.ModelID,
			ModelRevision: request.ModelRevision,
			ConfigHash:    request.ConfigHash,
			SpaceID:       request.SpaceID,
		})
	}
	rows.Close()
	if scanErr != nil {
		return
	}
	for _, capability := range caps {
		select {
		case <-f.stop:
			return
		default:
		}
		owner := "browser-query-encoder"
		job, err := repo.LeaseNextLocalInference(ctx, owner, time.Minute, []remote.InferenceCapability{capability}, 0, time.Now().UTC())
		if err != nil || job == nil || job.Inference == nil {
			continue
		}
		f.commitQueryVector(ctx, repo, owner, job)
	}
}

// commitQueryVector stages and commits a deterministic unit vector for the
// leased query task, exactly the transitions a real Worker performs, so the
// search's vector cache resolves and the page's poll observes "ready".
func (f *Fixture) commitQueryVector(ctx context.Context, repo *sqlite.Repository, owner string, job *remote.WorkerJob) {
	request := job.Inference
	if request == nil || request.Operation != domain.OpVisualEmbedding {
		return
	}
	space, err := repo.GetEmbeddingSpace(ctx, request.SpaceID)
	if err != nil {
		slog.Warn("browserfixture query encoder found no embedding space", "job_id", job.JobID, "space_id", request.SpaceID, "error", err)
		return
	}
	dimension := space.Dimension
	if dimension <= 0 {
		dimension = 2
	}
	vector := make([]float32, dimension)
	value := float32(1 / math.Sqrt(float64(dimension)))
	for i := range vector {
		vector[i] = value
	}
	result := domain.InferenceResult{
		SchemaVersion: request.SchemaVersion,
		OperationID:   request.OperationID,
		Operation:     request.Operation,
		ModelID:       request.ModelID,
		ModelRevision: request.ModelRevision,
		SpaceID:       request.SpaceID,
		ConfigHash:    request.ConfigHash,
		InputHash:     request.InputHash,
		Backend:       "browser-fixture",
		Embeddings:    []domain.EmbeddingRecord{{SampleIndex: -1, Vector: vector}},
	}
	if err := repo.StageInferenceTask(ctx, job.JobID, owner, result); err != nil {
		slog.Warn("browserfixture query encoder could not stage result", "job_id", job.JobID, "error", err)
		return
	}
	if err := repo.CommitInferenceTask(ctx, job.JobID, owner); err != nil {
		slog.Warn("browserfixture query encoder could not commit result", "job_id", job.JobID, "error", err)
	}
}

func seed(ctx context.Context, repo *sqlite.Repository, rootPath string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	db := repo.DB()
	// The library root is a dedicated media subdirectory, not the fixture temp
	// dir itself, so a scan of it is deterministic: the seeded asset's source
	// file is absent (thumbnail/proxy requests 404, exactly like a real
	// missing-mount test), and the only regular files present are the skipped
	// non-video ones below.
	mediaDir := filepath.Join(rootPath, "media")
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return err
	}
	// A supported-format fixture (referenced by the seeded asset location) and
	// the unsupported files a scan must report as skipped: an .mkv and a
	// sidecar, so POST /roots/{id}/scan returns skipped_files>0 with named
	// extensions and the zero-discovery warning path is exercisable.
	for _, name := range []string{"fixture.mp4", "skipped.mkv", "note.txt"} {
		if err := os.WriteFile(filepath.Join(mediaDir, name), []byte("fixture bytes"), 0o600); err != nil {
			return err
		}
	}
	root, err := repo.CreateLibraryRoot(ctx, mediaDir)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`, "asset-fixture", "fixture-fingerprint", 1234, "discovered", now, now); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO asset_locations(id,asset_id,root_id,relative_path,absolute_path,modified_ns,exists_now,is_primary,last_seen_at) VALUES(?,?,?,?,?,1,1,1,?)`, "location-fixture", "asset-fixture", root.ID, `"><img src=x onerror=window.__xss=1>.mp4`, filepath.Join(mediaDir, "fixture.mp4"), now); err != nil {
		return err
	}
	if err := repo.ReplaceAssetShots(ctx, "asset-fixture", "", []domain.AssetShot{{ID: "shot-fixture", AssetID: "asset-fixture", Ordinal: 0, StartMS: 0, EndMS: 5000, Description: `"><img src=x onerror=window.__xss=1>`, Tags: []string{`"><img src=x onerror=window.__xss=1>`}, Objects: []string{"camera"}, Confidence: .9, CreatedAt: time.Now().UTC()}}, "", ""); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO asset_tag_links(asset_id,raw_tag,normalized_tag,tag_type,source,created_at,updated_at) VALUES(?,?,?,?,'ai',?,?)`, "asset-fixture", "city", "city", "topic", now, now); err != nil {
		return err
	}
	collection, err := repo.SaveAssetCollection(ctx, domain.AssetCollection{ID: "collection-fixture", Name: `"><img src=x onerror=window.__xss=1>`, Description: `"><img src=x onerror=window.__xss=1>`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	if err := repo.AddShotToCollection(ctx, collection.ID, "shot-fixture"); err != nil {
		return err
	}
	if _, err := repo.UpsertProviderChannel(ctx, domain.ProviderChannel{ID: "channel-fixture", Capability: "video_analysis", Label: "Fixture channel", ProviderName: "fixture", Endpoint: "http://127.0.0.1:1", Model: "offline", Enabled: true, Members: []domain.ProviderChannelMember{{ID: "member-fixture", Label: "redacted-key", SecretRef: "fixture/secret", SecretReady: false, Enabled: true, Weight: 1, MaxInflight: 1}}}); err != nil {
		return err
	}
	for i, typ := range []domain.JobType{domain.JobProbe, domain.JobDerive, domain.JobAnalyze} {
		if err := repo.EnqueueJob(ctx, "asset-fixture", typ, "fixture-hash-"+string(rune('0'+i)), i); err != nil {
			return err
		}
	}
	if err := repo.EnqueueJob(ctx, "asset-fixture", domain.JobIndex, "fixture-hash-failed", 1); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET state='failed',terminal=1,last_error_code='configuration',last_error_message=? WHERE asset_id=? AND job_type=? AND input_hash=?`, "fixture provider failure: offline provider is intentionally unavailable", "asset-fixture", string(domain.JobIndex), "fixture-hash-failed"); err != nil {
		return err
	}
	if err := seedBrowsePagination(ctx, repo, db, now, olderThan(now)); err != nil {
		return err
	}
	// The long-timeline asset is seeded with the newest timestamp so its card
	// lands on the ordinary browse page the timeline scenarios open directly;
	// its probe job still keeps it out of the discovered saved view.
	return seedLongTimelineAsset(ctx, repo, db, now)
}

// olderThan returns a timestamp one hour earlier, so the seeded bulk cards
// (first seen "now") sort above the saved view's remainder pages and the
// long-timeline asset deterministically under the browse order
// (captured_at DESC, then id DESC).
func olderThan(now string) string {
	parsed, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		return now
	}
	return parsed.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
}

// seedBrowsePagination seeds 301 discovered assets so the browse and
// saved-view page loaders have a real second page: cards 0001–0300 fill the
// first 300-row page (newest first, id tie-break descending), and card 0301
// is the only row of the continuation, reachable only through Load more.
// The saved view (status=discovered) contains exactly these 301 cards: the
// fixture asset is "queued" (pending jobs) and the long-timeline asset is
// "queued" (probe job) so neither leaks into the view.
func seedBrowsePagination(ctx context.Context, repo *sqlite.Repository, db *sql.DB, now, older string) error {
	const cardCount = 301
	stmt, err := db.PrepareContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := 1; i <= cardCount; i++ {
		seen := now
		if i == cardCount {
			seen = older
		}
		if _, err := stmt.ExecContext(ctx, fmt.Sprintf("asset-card-%04d", i), fmt.Sprintf("card-%04d-fingerprint", i), 2048+i, "discovered", seen, seen); err != nil {
			return err
		}
	}
	_, err = repo.SaveAssetCollection(ctx, domain.AssetCollection{
		ID:          "view-discovered",
		Name:        "Discovery day view",
		Description: "Browser fixture saved view: every discovered card",
		Filter:      domain.AssetCollectionFilter{Status: domain.ProcessingStatusDiscovered},
	})
	if err != nil {
		return err
	}
	return repo.AddShotToCollection(ctx, "view-discovered", "shot-fixture")
}

// seedLongTimelineAsset seeds one asset whose transcript_words track exceeds
// the timeline endpoint's 1000-row per-track page, so the drawer shows the
// partial state and the final word arrives only through Load more. The probe
// job keeps the asset out of the discovered saved view; media_metadata gives
// it the duration the timeline needs.
func seedLongTimelineAsset(ctx context.Context, repo *sqlite.Repository, db *sql.DB, seen string) error {
	const assetID = "asset-timeline-long"
	const wordCount = 1001
	if _, err := db.ExecContext(ctx, `INSERT INTO assets(id,quick_fingerprint,file_size,state,first_seen_at,last_seen_at) VALUES(?,?,?,'discovered',?,?)`, assetID, "long-timeline-fingerprint", 4096, seen, seen); err != nil {
		return err
	}
	metadata, err := json.Marshal(map[string]any{
		"duration_ms": 240000, "width": 640, "height": 360, "fps": 25.0,
		"orientation": "landscape", "has_audio": false,
	})
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO media_metadata(asset_id,ffprobe_json,exiftool_json,normalized_json,probe_version,updated_at) VALUES(?,?,'{}',?,?,'{}')`, assetID, metadata, metadata, "fixture", seen); err != nil {
		return err
	}
	if err := repo.EnqueueJob(ctx, assetID, domain.JobProbe, "fixture-hash-long", 0); err != nil {
		return err
	}
	words := make([]domain.AlignmentWord, 0, wordCount)
	for i := range wordCount {
		words = append(words, domain.AlignmentWord{
			StartMS: int64(i) * 200,
			EndMS:   int64(i)*200 + 200,
			Text:    fmt.Sprintf("word-%04d", i+1),
		})
	}
	return repo.SaveAlignment(ctx, assetID, "fixture", "fixture-align", "fixture-align-hash", "{}", domain.AlignmentResult{Words: words, RawResponse: "{}"}, "", "")
}
