package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	videoanalysis "github.com/evjohn-icu/nexusslate/internal/domain/video_analysis"
	"github.com/evjohn-icu/nexusslate/internal/providerchannels"
	"github.com/evjohn-icu/nexusslate/internal/providers"
	"github.com/evjohn-icu/nexusslate/internal/providers/common"
	videoproviders "github.com/evjohn-icu/nexusslate/internal/providers/video"
)

type runtimeChannelRepo struct {
	mu       sync.Mutex
	channels []domain.ProviderChannel
}

func (r *runtimeChannelRepo) ListProviderChannels(_ context.Context, capability string) ([]domain.ProviderChannel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]domain.ProviderChannel, 0, len(r.channels))
	for _, channel := range r.channels {
		if capability == "" || channel.Capability == capability {
			copyChannel := channel
			copyChannel.Members = append([]domain.ProviderChannelMember(nil), channel.Members...)
			result = append(result, copyChannel)
		}
	}
	return result, nil
}

type runtimeSecrets struct{ values map[string]string }

func (s *runtimeSecrets) Has(_ context.Context, ref string) (bool, error) {
	_, ok := s.values[ref]
	return ok, nil
}

func (s *runtimeSecrets) Resolve(ref string) (string, bool, error) {
	value, ok := s.values[ref]
	return value, ok, nil
}

type runtimeLegacyASR struct {
	name  string
	model string
	text  string
	calls int
}

func (p *runtimeLegacyASR) Name() string  { return p.name }
func (p *runtimeLegacyASR) Model() string { return p.model }
func (p *runtimeLegacyASR) Transcribe(context.Context, common.TranscribeRequest) (domain.Transcript, error) {
	p.calls++
	return domain.Transcript{Language: "zh", Text: p.text}, nil
}

func TestProviderChannelRuntimeASRUsesExecutorRetryAndKeepsKeysOutOfErrors(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		calls = append(calls, key)
		w.Header().Set("Content-Type", "application/json")
		if len(calls) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`provider key-a should not escape`))
			return
		}
		if key != "Bearer key-b" {
			t.Fatalf("second request authorization=%q; want key-b", key)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"来自第二个通道"}}]}`))
	}))
	defer server.Close()

	audioPath := filepath.Join(t.TempDir(), "sample.wav")
	if err := os.WriteFile(audioPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &runtimeChannelRepo{channels: []domain.ProviderChannel{{
		ID: "channel-qwen", Capability: "asr", Label: "Qwen ASR", ProviderName: "qwen",
		Endpoint: server.URL + "/v1", Model: "qwen-test", Enabled: true, RouteOrder: 1,
		Members: []domain.ProviderChannelMember{
			{ID: "member-a", Label: "key-a", SecretRef: "provider/qwen-a", Enabled: true},
			{ID: "member-b", Label: "key-b", SecretRef: "provider/qwen-b", Enabled: true},
		},
	}}}
	secrets := &runtimeSecrets{values: map[string]string{"provider/qwen-a": "key-a", "provider/qwen-b": "key-b"}}
	runtime := newProviderChannelRuntime(repo, config.Config{Providers: config.ProvidersConfig{Qwen: config.ProviderConfig{Enabled: true, BaseURL: server.URL + "/v1", Path: "chat/completions", Model: "qwen-default"}}}, secrets, nil, nil, nil, nil, nil, nil)

	transcript, err := runtime.asr().Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if transcript.Text != "来自第二个通道" || len(calls) != 2 {
		t.Fatalf("transcript=%+v calls=%v", transcript, calls)
	}
	second, err := runtime.asr().Transcribe(context.Background(), common.TranscribeRequest{AudioPath: audioPath, Language: "zh"})
	if err != nil {
		t.Fatal(err)
	}
	if second.Text != "来自第二个通道" || len(calls) != 3 || calls[2] != "Bearer key-b" {
		t.Fatalf("second transcript=%+v calls=%v; cached Executor should preserve key-a cooldown", second, calls)
	}
	for _, value := range calls {
		if !strings.Contains(value, "key-") {
			t.Fatalf("fixture did not observe the expected in-memory credential: %q", value)
		}
	}
	if strings.Contains(fmt.Sprint(transcript), "key-a") || strings.Contains(fmt.Sprint(transcript), "key-b") {
		t.Fatalf("transcript leaked provider credential: %+v", transcript)
	}
}

func TestProviderChannelRuntimeVideoUsesPersistedChannelAndRedactsProviderResponse(t *testing.T) {
	const secret = "video-channel-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Fatalf("authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"video-channel-secret\",\"raw_tags\":[\"urban_night\"]}"}}]}`))
	}))
	defer server.Close()

	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("video-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &runtimeChannelRepo{channels: []domain.ProviderChannel{{
		ID: "channel-volc-video", Capability: "video_analysis", Label: "Ark Video", ProviderName: "volcengine_video",
		Protocol: "openai_video", Endpoint: server.URL + "/v1", Model: "vision-test", Enabled: true,
		Members: []domain.ProviderChannelMember{{ID: "member-video", Label: "primary", SecretRef: "provider/video", Enabled: true}},
	}}}
	secrets := &runtimeSecrets{values: map[string]string{"provider/video": secret}}
	runtime := newProviderChannelRuntime(repo, config.Config{Providers: config.ProvidersConfig{VolcVideo: config.ProviderConfig{Enabled: true, Protocol: "openai_video", BaseURL: server.URL + "/v1", Path: "chat/completions", Model: "default"}}}, secrets, nil, nil, nil, nil, nil, nil)

	result, raw, err := runtime.video().Analyze(context.Background(), videoanalysis.Input{VideoPath: videoPath, MIMEType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "[REDACTED]" || strings.Contains(raw, secret) || strings.Contains(fmt.Sprint(result), secret) {
		t.Fatalf("provider response was not redacted: result=%+v raw=%q", result, raw)
	}
}

func TestProviderChannelRuntimePreservesLegacyFallbackWhenNoCompatibleChannelExists(t *testing.T) {
	legacy := &runtimeLegacyASR{name: "legacy-asr", model: "legacy-model", text: "旧配置回退"}
	runtime := newProviderChannelRuntime(
		&runtimeChannelRepo{channels: []domain.ProviderChannel{{
			ID: "video-only", Capability: string(providerchannels.CapabilityVideoAnalysis), ProviderName: "volcengine_video", Label: "video", Enabled: true,
		}}},
		config.Config{}, &runtimeSecrets{values: map[string]string{}}, legacy, nil, nil, nil, nil, nil,
	)

	transcript, err := runtime.asr().Transcribe(context.Background(), common.TranscribeRequest{Language: "zh"})
	if err != nil || transcript.Text != "旧配置回退" || legacy.calls != 1 {
		t.Fatalf("transcript=%+v calls=%d err=%v", transcript, legacy.calls, err)
	}
}

func TestProviderChannelRuntimeUsesCuratorEmbeddingAndPlannerChannels(t *testing.T) {
	const secret = "shared-channel-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+secret {
			t.Fatalf("authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/embeddings" {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`))
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		var content string
		if body.Model == "planner-model" {
			content = `{"title":"夜景翻新","sections":[{"role":"opening","query":"night city","duration_ms":5000,"required":true,"rationale":"建立氛围"}]}`
		} else {
			content = `{"groups":[{"canonical_name":"urban_night","aliases":["night city"],"category":"scene","confidence":0.9,"reason":"same semantic"}]}`
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.ReplaceAll(content, `"`, `\"`) + `"}}]}`))
	}))
	defer server.Close()

	ref := func(capability, provider, model string) domain.ProviderChannel {
		protocol := "openai_chat"
		if capability == "embedding" {
			protocol = "openai_embeddings"
		}
		return domain.ProviderChannel{ID: capability + "-channel", Capability: capability, Label: capability, ProviderName: provider, Protocol: protocol, Endpoint: server.URL + "/v1", Model: model, Enabled: true, Members: []domain.ProviderChannelMember{{ID: capability + "-member", Label: "primary", SecretRef: "provider/" + capability, Enabled: true}}}
	}
	repo := &runtimeChannelRepo{channels: []domain.ProviderChannel{
		ref("tag_curator", "openai_chat", "curator-model"),
		ref("embedding", "openai_embeddings", "embedding-model"),
		ref("repurpose", "openai_chat", "planner-model"),
	}}
	secrets := &runtimeSecrets{values: map[string]string{
		"provider/tag_curator": secret, "provider/embedding": secret, "provider/repurpose": secret,
	}}
	runtime := newProviderChannelRuntime(repo, config.Config{Providers: config.ProvidersConfig{
		TagCurator: config.ProviderConfig{Enabled: true, Protocol: "openai_chat", BaseURL: server.URL + "/v1", Path: "chat/completions", Model: "curator-default"},
		Embedding:  config.ProviderConfig{Enabled: true, Protocol: "openai_embeddings", BaseURL: server.URL + "/v1", Path: "embeddings", Model: "embedding-default"},
		Repurpose:  config.ProviderConfig{Enabled: true, Protocol: "openai_chat", BaseURL: server.URL + "/v1", Path: "chat/completions", Model: "planner-default"},
	}}, secrets, nil, nil, nil, nil, nil, nil)

	proposals, err := runtime.curator().Curate(context.Background(), []domain.UnresolvedTag{{NormalizedTag: "night city"}}, nil)
	if err != nil || len(proposals) != 1 || proposals[0].CanonicalName != "urban_night" {
		t.Fatalf("curator proposals=%+v err=%v", proposals, err)
	}
	vectors, err := runtime.embedder().Embed(context.Background(), []string{"urban night"})
	if err != nil || len(vectors) != 1 || len(vectors[0]) != 2 {
		t.Fatalf("embedding vectors=%v err=%v", vectors, err)
	}
	draft, err := runtime.planner().Plan(context.Background(), domain.RepurposeBrief{Brief: "深圳夜景", DurationMS: 5000})
	if err != nil || draft.Title != "夜景翻新" || len(draft.Sections) != 1 {
		t.Fatalf("planner draft=%+v err=%v", draft, err)
	}
}

func TestProviderChannelRuntimeGeminiPreparationBindsAnalyzeToSelectedChannel(t *testing.T) {
	const secret = "gemini-channel-key"
	var paths []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != secret {
			t.Fatalf("x-goog-api-key=%q", got)
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/upload/v1beta/files":
			w.Header().Set("X-Goog-Upload-URL", server.URL+"/upload/finalize")
		case "/upload/finalize":
			_, _ = w.Write([]byte(`{"file":{"name":"files/fixture","uri":"https://generativelanguage.example/files/fixture","mimeType":"video/mp4","state":"ACTIVE"}}`))
		case "/v1beta/models/gemini-test:generateContent":
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"summary\":\"夜晚城市\",\"raw_tags\":[\"urban_night\"]}"}]}}]}`))
		default:
			t.Fatalf("unexpected provider path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	videoPath := filepath.Join(t.TempDir(), "fixture.mp4")
	if err := os.WriteFile(videoPath, []byte("video-fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &runtimeChannelRepo{channels: []domain.ProviderChannel{{
		ID: "gemini-channel", Capability: "video_analysis", Label: "Gemini", ProviderName: "gemini", Endpoint: server.URL + "/v1beta", Model: "gemini-test", Enabled: true,
		Members: []domain.ProviderChannelMember{{ID: "gemini-member", Label: "primary", SecretRef: "provider/gemini", Enabled: true}},
	}}}
	runtime := newProviderChannelRuntime(repo, config.Config{Providers: config.ProvidersConfig{Gemini: config.ProviderConfig{Enabled: true, Protocol: "gemini_generate_content", BaseURL: server.URL + "/v1beta", Model: "gemini-default", AuthHeader: "x-goog-api-key", AuthScheme: "raw"}}}, &runtimeSecrets{values: map[string]string{"provider/gemini": secret}}, nil, nil, nil, nil, nil, nil)
	video := runtime.video()
	preparer, ok := video.(videoproviders.VideoPreparer)
	if !ok || !video.(interface{ RequiresVideoPreparation() bool }).RequiresVideoPreparation() {
		t.Fatal("Gemini channel should advertise video preparation")
	}
	prepared, err := preparer.PrepareVideo(context.Background(), common.PrepareVideoRequest{VideoPath: videoPath, DisplayName: "fixture.mp4", MIMEType: "video/mp4"})
	if err != nil || prepared.RemoteURI == "" {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	result, _, err := video.Analyze(context.Background(), videoanalysis.Input{VideoPath: videoPath, RemoteURI: prepared.RemoteURI, MIMEType: "video/mp4"})
	if err != nil || result.Summary != "夜晚城市" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(paths) != 3 || paths[0] != "/upload/v1beta/files" || paths[1] != "/upload/finalize" || paths[2] != "/v1beta/models/gemini-test:generateContent" {
		t.Fatalf("provider paths=%v", paths)
	}
}

func TestProviderChannelRuntimeNotConfiguredIsDistinctFromProviderFailure(t *testing.T) {
	runtime := newProviderChannelRuntime(&runtimeChannelRepo{}, config.Config{}, &runtimeSecrets{values: map[string]string{}}, nil, nil, nil, nil, nil, nil)
	_, err := runtime.planner().Plan(context.Background(), domain.RepurposeBrief{Brief: "test"})
	if !errors.Is(err, ErrProviderChannelNotConfigured) {
		t.Fatalf("planner err=%v; want ErrProviderChannelNotConfigured", err)
	}
}

// TestIdentityCacheLockOrderExercisesConcurrentFastPathAndCacheWrite exercises the
// lock ordering: identity()'s fast path takes identityCacheMu then cacheMu, and
// cacheIdentity() must do the same.  The race detector (go test -race) verifies
// there is no inversion deadlock.
func TestIdentityCacheLockOrderExercisesConcurrentFastPathAndCacheWrite(t *testing.T) {
	repo := &runtimeChannelRepo{}
	secrets := &runtimeSecrets{values: map[string]string{}}
	runtime := newProviderChannelRuntime(repo, config.Config{}, secrets,
		&runtimeLegacyASR{name: "legacy-asr", model: "legacy-model"}, nil, nil, nil, nil, nil)

	var wg sync.WaitGroup
	// Fire many concurrent goroutines that call Name() (which uses identity() fast
	// path) and Model() (ditto).  Concurrent calls trigger cacheIdentity() writes
	// while the fast path reads — the lock order must be consistent or the race
	// detector flags it.
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = runtime.asr().Name() }()
		go func() { defer wg.Done(); _ = runtime.asr().Model() }()
	}
	wg.Wait()
}

// TestResolveIdentityRequiresModel verifies that resolveIdentity returns empty
// when Model is blank even if ProviderName is set, so a partially configured
// channel triggers the sentinel fallback rather than caching (name, "").
//
// This test uses the ASR capability because runtimeLegacyASR already implements
// the Name/Model interface for that path.
func TestResolveIdentityRequiresModel(t *testing.T) {
	repo := &runtimeChannelRepo{}
	// Channel with a provider name but no model: this must NOT produce a
	// cached ("stepfun", "") pair.
	repo.channels = []domain.ProviderChannel{{
		ID:           "ch-1",
		Capability:   "asr",
		ProviderName: "stepfun",
		Protocol:     "openai_chat",
		Endpoint:     "http://127.0.0.1:1/v1",
		Model:        "",
		Enabled:      true,
		RouteOrder:   1,
		Members: []domain.ProviderChannelMember{
			{ID: "m-1", ChannelID: "ch-1", SecretRef: "k1", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}}
	secrets := &runtimeSecrets{values: map[string]string{"k1": "secret"}}
	legacy := &runtimeLegacyASR{name: "fallback-asr", model: "fallback-model"}
	runtime := newProviderChannelRuntime(repo, config.Config{}, secrets,
		legacy, nil, nil, nil, nil, nil)

	name, model := runtime.asr().Name(), runtime.asr().Model()
	if name == "" || model == "" {
		t.Fatalf("Name/Model returned empty: name=%q model=%q", name, model)
	}
	// The fallback should be used since the channel has no model field.
	// (If the channel's empty model were accepted, we'd get "stepfun" / "".)
	t.Logf("resolved name=%q model=%q", name, model)
	if name == "stepfun" && model == "" {
		t.Errorf("channel with empty model was incorrectly cached as (stepfun, \"\")")
	}
	// Verify the cached identity is a complete pair (not a mixed name/empty model).
	runtime.identityCacheMu.Lock()
	for key, entry := range runtime.identityCache {
		t.Logf("cached key=%q name=%q model=%q", key, entry.name, entry.model)
		if entry.name == "" || entry.model == "" {
			t.Errorf("cached identity has empty field: key=%q name=%q model=%q", key, entry.name, entry.model)
		}
	}
	runtime.identityCacheMu.Unlock()
}

var _ providers.ASR = (*runtimeLegacyASR)(nil)
