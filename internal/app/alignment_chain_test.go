package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/domain"
	videoanalysis "github.com/evjohn-icu/timingdex/internal/domain/video_analysis"
	"github.com/evjohn-icu/timingdex/internal/media"
	"github.com/evjohn-icu/timingdex/internal/providers"
	videoproviders "github.com/evjohn-icu/timingdex/internal/providers/video"
	"github.com/evjohn-icu/timingdex/internal/repository/sqlite"
)

// recordingVideoProviderForAlignment answers the analyze call and records the
// transcript the model was actually given — the whole point of the
// align→analyze chain is that the video model sees the aligned timeline.
type recordingVideoProviderForAlignment struct {
	saw *domain.Transcript
}

func (f *recordingVideoProviderForAlignment) Name() string  { return "fixture-video" }
func (f *recordingVideoProviderForAlignment) Model() string { return "fixture-v" }
func (f *recordingVideoProviderForAlignment) Capabilities() []videoproviders.Capability {
	return []videoproviders.Capability{videoproviders.CapabilityVideoAnalysis}
}
func (f *recordingVideoProviderForAlignment) Analyze(_ context.Context, input videoanalysis.Input) (videoanalysis.Result, string, error) {
	f.saw = input.Transcript
	return videoanalysis.Result{
		Summary: "alignment consumption fixture",
		Shots:   []videoanalysis.Shot{{StartMS: 0, EndMS: 5_000, Description: "opening shot"}},
	}, `{"fixture":true}`, nil
}

// fakeAlignmentProvider returns word timestamps for a fixed span of speech.
type fakeAlignmentProvider struct{}

func (fakeAlignmentProvider) Name() string  { return "fixture-aligner" }
func (fakeAlignmentProvider) Model() string { return "fixture-a" }
func (fakeAlignmentProvider) Align(_ context.Context, _ providers.AlignRequest) (domain.AlignmentResult, error) {
	return domain.AlignmentResult{Words: []domain.AlignmentWord{
		{StartMS: 310_000, EndMS: 313_000, Text: "hel"},
		{StartMS: 313_000, EndMS: 315_000, Text: "lo"},
	}}, nil
}

// TestAnalyzeConsumesAlignedTimelineWhenAlignmentSucceeded pins the chain:
// ASR produced only untimed text (the Qwen 0-0 placeholder shape), forced
// alignment placed it at 310-315s, and the analyze stage must hand the model
// the aligned timed transcript — not the untimed text, which sliceTranscript
// now withholds in split mode. Before the read path existed, alignment words
// were persisted and never consumed.
func TestAnalyzeConsumesAlignedTimelineWhenAlignmentSucceeded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := sqlite.Open(filepath.Join(dir, "align-chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	footage := filepath.Join(dir, "footage")
	if err := os.MkdirAll(footage, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := repo.CreateLibraryRoot(ctx, footage)
	if err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(root.Path, "clip.mov")
	if err := os.WriteFile(proxyPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := repo.UpsertScannedFile(ctx, root, "clip.mov", proxyPath, info, "fp-align-chain")
	if err != nil {
		t.Fatal(err)
	}
	assetID := scanned.AssetID
	if err := repo.SaveMediaMetadata(ctx, assetID, domain.MediaMetadata{DurationMS: 120_000}, "test"); err != nil {
		t.Fatal(err)
	}
	// Seed the proxy artifact through a real derive lease, the only write path
	// the schema allows.
	if err := repo.EnqueueJob(ctx, assetID, domain.JobDerive, "seed-artifact", 50); err != nil {
		t.Fatal(err)
	}
	job, err := repo.LeaseNextJob(ctx, "seed", nil, domain.LeaseFilter{})
	if err != nil || job == nil {
		t.Fatalf("lease for artifact seed: job=%+v err=%v", job, err)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: "art-proxy", AssetID: assetID, Type: "proxy", ProfileHash: "p1", LocalPath: proxyPath, SizeBytes: info.Size()}, job.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveArtifact(ctx, domain.DerivedArtifact{ID: "art-audio", AssetID: assetID, Type: "audio", ProfileHash: "a1", LocalPath: filepath.Join(dir, "audio.wav"), SizeBytes: 1}, job.ID, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := repo.CompleteJob(ctx, job.ID, "seed", domain.JobSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	untimed := domain.Transcript{
		Language: "zh",
		Text:     "整段视频的完整语音内容",
		Segments: []domain.TranscriptSegment{{StartMS: 0, EndMS: 0, Text: "整段视频的完整语音内容"}},
	}
	if err := repo.SaveTranscript(ctx, assetID, "qwen", "qwen3-asr-flash", "thash", untimed); err != nil {
		t.Fatal(err)
	}

	video := &recordingVideoProviderForAlignment{}
	pipeline := NewPipeline(repo, t.TempDir(), nil, nil, video, fakeAlignmentProvider{}, media.HardwarePlan{}, nil, providerRouteDeferral)
	if err := repo.EnqueueJob(ctx, assetID, domain.JobAlign, hashStrings("chain", "align"), 50); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.RunUntilIdle(ctx); err != nil {
		t.Fatal(err)
	}
	if video.saw == nil {
		t.Fatal("video provider received no transcript")
	}
	if !video.saw.Timed() {
		t.Fatalf("model must receive a timed transcript, got %+v", video.saw)
	}
	if len(video.saw.Segments) != 2 || video.saw.Segments[1].EndMS != 315_000 {
		t.Fatalf("model must receive the aligned word timeline, got %+v", video.saw.Segments)
	}
}
