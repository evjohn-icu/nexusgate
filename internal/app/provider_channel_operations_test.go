package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusslate/internal/config"
	"github.com/evjohn-icu/nexusslate/internal/credentials"
	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/remote"
	"github.com/evjohn-icu/nexusslate/internal/secretstore"
)

// fakeUpsertOnlyRepo implements Repository by embedding the interface as a nil
// zero value and overriding only UpsertProviderChannel, the single method
// SaveProviderChannel calls. Any other method would panic, which is the
// intent: it keeps the fake honest about how narrow the exercised surface is.
// It exists because the real sqlite schema enforces UNIQUE(channel_id, label)
// and so cannot express the duplicate-label shape this test pins down.
type fakeUpsertOnlyRepo struct {
	Repository
	saved domain.ProviderChannel
}

func (r *fakeUpsertOnlyRepo) UpsertProviderChannel(_ context.Context, channel domain.ProviderChannel) (domain.ProviderChannel, error) {
	r.saved = channel
	return channel, nil
}

// TestSaveProviderChannelRejectsDuplicateLabelBeforeAnySecretIsStored guards
// the create path's half of N1/N2: two members sharing a label used to reach
// UpsertProviderChannel — after SaveProviderChannel had already written a
// secret for each member — and surface a bare SQLite UNIQUE-constraint error
// with no rollback. validateDistinctProviderChannelMemberLabels now rejects
// the duplicate before anything is written, matching the update path's
// existing guarantee that a rejected write leaves the secret store untouched.
//
// This test used to assert the opposite: that a duplicate-label create
// succeeded and kept the two members' keys distinct, back when
// SaveProviderChannel performed no such validation and relied on
// UNIQUE(channel_id, label) (migration 0013) to catch it after the fact.
func TestSaveProviderChannelRejectsDuplicateLabelBeforeAnySecretIsStored(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeUpsertOnlyRepo{}
	service := &Service{repo: repo, cfg: config.Config{DataDir: dataDir}, secrets: secrets}

	channel := domain.ProviderChannel{
		ID:           "channel-create-dupe",
		Capability:   "video_analysis",
		Label:        "Duplicate label channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{ID: "member-a", Label: "same-label", Enabled: true, Weight: 1, MaxInflight: 1},
			{ID: "member-b", Label: "same-label", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}
	_, err = service.SaveProviderChannel(ctx, channel, []string{"key-for-member-a", "key-for-member-b"})
	if err == nil {
		t.Fatal("expected duplicate-label create to be rejected")
	}
	if !errors.Is(err, ErrProviderChannelValidation) {
		t.Fatalf("error should wrap ErrProviderChannelValidation, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "same-label") {
		t.Fatalf("error should name the duplicated label, got %q", err.Error())
	}
	for _, secret := range []string{"key-for-member-a", "key-for-member-b"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error must not contain secret material: %q", err.Error())
		}
	}
	if repo.saved.ID != "" {
		t.Fatalf("repository must not be written to on a rejected create: saved=%+v", repo.saved)
	}
	for _, id := range []string{"member-a", "member-b"} {
		ref := "provider-channel/channel-create-dupe/" + id
		if _, ok, err := secrets.Get(ref); err != nil || ok {
			t.Fatalf("secret must not exist for %s: ok=%v err=%v", ref, ok, err)
		}
	}
}

// TestSaveProviderChannelShortMemberKeysSliceLeavesTrailingMembersUnchanged
// documents that a memberKeys slice shorter than channel.Members must not
// panic and must be treated as "no key" for the missing tail, matching the
// same-length contract the API and patch handlers rely on.
func TestSaveProviderChannelShortMemberKeysSliceLeavesTrailingMembersUnchanged(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: &fakeUpsertOnlyRepo{}, cfg: config.Config{DataDir: dataDir}, secrets: secrets}

	channel := domain.ProviderChannel{
		Capability:   "video_analysis",
		Label:        "Short keys channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{Label: "only-one-key", Enabled: true, Weight: 1, MaxInflight: 1},
			{Label: "no-key", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}
	saved, err := service.SaveProviderChannel(ctx, channel, []string{"key-for-first"})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := secrets.Get(saved.Members[1].SecretRef); err != nil || ok {
		t.Fatalf("member[1] should have no stored secret: ok=%v err=%v", ok, err)
	}
	value, ok, err := secrets.Get(saved.Members[0].SecretRef)
	if err != nil || !ok || value != "key-for-first" {
		t.Fatalf("member[0] value=%q ok=%v err=%v", value, ok, err)
	}
}

// fakeUpsertErrorRepo implements Repository the same way fakeUpsertOnlyRepo
// does, but UpsertProviderChannel always fails, simulating a rejected save
// (e.g. a capability+label UNIQUE collision or a missing required field). It
// still records the attempted channel so the test can recover the SecretRefs
// SaveProviderChannel generated, since the caller's own channel value is
// unmodified (SaveProviderChannel takes it by value) and the error return
// discards the generated IDs.
type fakeUpsertErrorRepo struct {
	Repository
	attempted domain.ProviderChannel
}

var errFakeUpsertRejected = errors.New("fake upsert rejected")

func (r *fakeUpsertErrorRepo) UpsertProviderChannel(_ context.Context, channel domain.ProviderChannel) (domain.ProviderChannel, error) {
	r.attempted = channel
	return domain.ProviderChannel{}, errFakeUpsertRejected
}

// TestSaveProviderChannelCleansUpOrphanedSecretsOnUpsertFailure guards the
// core invariant that a provider key never persists outside a channel that
// references it. Before this fix, SaveProviderChannel wrote each member's key
// to the secret store first and only then called UpsertProviderChannel; if
// the repository rejected the channel, the keys already written had no
// channel pointing at them and stayed in the encrypted store forever.
func TestSaveProviderChannelCleansUpOrphanedSecretsOnUpsertFailure(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeUpsertErrorRepo{}
	service := &Service{repo: repo, cfg: config.Config{DataDir: dataDir}, secrets: secrets}

	channel := domain.ProviderChannel{
		Capability:   "video_analysis",
		Label:        "Rejected channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{Label: "member-a", Enabled: true, Weight: 1, MaxInflight: 1},
			{Label: "member-b", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}
	_, err = service.SaveProviderChannel(ctx, channel, []string{"key-a", "key-b"})
	if !errors.Is(err, errFakeUpsertRejected) {
		t.Fatalf("err=%v; want errFakeUpsertRejected", err)
	}

	if len(repo.attempted.Members) != 2 {
		t.Fatalf("attempted members=%+v; want 2", repo.attempted.Members)
	}
	for _, member := range repo.attempted.Members {
		if member.SecretRef == "" {
			t.Fatalf("attempted member missing generated SecretRef: %+v", member)
		}
		if _, ok, err := secrets.Get(member.SecretRef); err != nil || ok {
			t.Fatalf("ref %q should have been cleaned up: ok=%v err=%v", member.SecretRef, ok, err)
		}
	}
}

// fakeChannelRepo supports the two Repository methods UpdateProviderChannel
// exercises: ListProviderChannels (to load the channel being patched) and
// UpsertProviderChannel (to persist the result). Anything else would panic via
// the embedded nil interface, which keeps the fake honest about how narrow the
// exercised surface is.
type fakeChannelRepo struct {
	Repository
	channel   domain.ProviderChannel
	saved     domain.ProviderChannel
	upsertErr error
}

func (r *fakeChannelRepo) ListProviderChannels(_ context.Context, _ string) ([]domain.ProviderChannel, error) {
	if r.channel.ID == "" {
		return nil, nil
	}
	return []domain.ProviderChannel{r.channel}, nil
}

func (r *fakeChannelRepo) UpsertProviderChannel(_ context.Context, channel domain.ProviderChannel) (domain.ProviderChannel, error) {
	if r.upsertErr != nil {
		return domain.ProviderChannel{}, r.upsertErr
	}
	r.saved = channel
	return channel, nil
}

func testServiceWithChannel(t *testing.T, channel domain.ProviderChannel, repo *fakeChannelRepo) (*Service, *secretstore.Store) {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	if repo == nil {
		repo = &fakeChannelRepo{}
	}
	repo.channel = channel
	service := &Service{repo: repo, cfg: config.Config{DataDir: dataDir}, secrets: secrets}
	return service, secrets
}

func providerChannelFixture(id string, members []domain.ProviderChannelMember) domain.ProviderChannel {
	return domain.ProviderChannel{
		ID:           id,
		Capability:   "video_analysis",
		Label:        "Test channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members:      members,
	}
}

// TestUpdateProviderChannelSameLabelNoIDRejectsDuplicate is the server-side
// guard for the H1 bug under the schema boundary: two same-label members with
// no ids in one patch used to both resolve to the same existing member, so the
// second silently overwrote the first and its key was destroyed. The label
// fallback now consumes each matched existing member at most once, so the
// second input becomes a fresh member instead of clobbering the first — and
// because labels are unique per channel (UNIQUE(channel_id, label), migration
// 0013), that duplicate is rejected before the save with a message naming the
// label. The surviving member's key must stay intact either way.
func TestUpdateProviderChannelSameLabelNoIDRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	channelID := "channel-dupe"
	existing := domain.ProviderChannelMember{
		ID: "member-1", ChannelID: channelID, Label: "google",
		SecretRef: "provider-channel/" + channelID + "/member-1",
		Enabled:   true, Weight: 1, MaxInflight: 1,
	}
	service, secrets := testServiceWithChannel(t, providerChannelFixture(channelID, []domain.ProviderChannelMember{existing}), nil)
	if err := secrets.Put(existing.SecretRef, "original-key"); err != nil {
		t.Fatal(err)
	}

	_, err := service.UpdateProviderChannel(ctx, channelID, ProviderChannelUpdate{
		Members: &[]ProviderChannelMemberUpdate{
			{Label: "google", APIKey: "key-a"},
			{Label: "google", APIKey: "key-b"},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate-label update to be rejected")
	}
	if !strings.Contains(err.Error(), "google") {
		t.Fatalf("error should name the duplicated label, got %q", err.Error())
	}
	for _, secret := range []string{"key-a", "key-b", "original-key", existing.SecretRef} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error must not contain secret material: %q", err.Error())
		}
	}
	value, ok, err := secrets.Get(existing.SecretRef)
	if err != nil || !ok || value != "original-key" {
		t.Fatalf("existing key must survive a rejected patch: value=%q ok=%v err=%v", value, ok, err)
	}
}

// TestUpdateProviderChannelRejectsCaseVariantDuplicateLabel pins that the
// duplicate check matches the byLabel fallback's case-insensitivity: two
// members whose labels differ only in case are the same label to this code,
// so they must be rejected before the save rather than reaching the database.
func TestUpdateProviderChannelRejectsCaseVariantDuplicateLabel(t *testing.T) {
	ctx := context.Background()
	channelID := "channel-case"
	service, _ := testServiceWithChannel(t, providerChannelFixture(channelID, nil), nil)

	_, err := service.UpdateProviderChannel(ctx, channelID, ProviderChannelUpdate{
		Members: &[]ProviderChannelMemberUpdate{
			{Label: "Google", APIKey: "key-a"},
			{Label: "google", APIKey: "key-b"},
		},
	})
	if err == nil {
		t.Fatal("expected case-variant duplicate label to be rejected")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "google") {
		t.Fatalf("error should name the duplicated label, got %q", err.Error())
	}
}

// TestUpdateProviderChannelRealIDsUpdateInPlace covers the compatibility
// contract: an ordinary patch that carries real member ids must update the
// existing members in place, keep their SecretRefs, and destroy nothing.
func TestUpdateProviderChannelRealIDsUpdateInPlace(t *testing.T) {
	ctx := context.Background()
	channelID := "channel-inplace"
	members := []domain.ProviderChannelMember{
		{ID: "m1", ChannelID: channelID, Label: "key-a", SecretRef: "ref-inplace-a", Enabled: true, Weight: 1, MaxInflight: 1},
		{ID: "m2", ChannelID: channelID, Label: "key-b", SecretRef: "ref-inplace-b", Enabled: true, Weight: 1, MaxInflight: 1},
	}
	service, secrets := testServiceWithChannel(t, providerChannelFixture(channelID, members), nil)
	for _, member := range members {
		if err := secrets.Put(member.SecretRef, "original-"+member.Label); err != nil {
			t.Fatal(err)
		}
	}
	weight := 3
	saved, err := service.UpdateProviderChannel(ctx, channelID, ProviderChannelUpdate{
		Members: &[]ProviderChannelMemberUpdate{
			{ID: "m1", Label: "key-a", Weight: &weight},
			{ID: "m2", Label: "key-b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Members) != 2 {
		t.Fatalf("saved members=%+v; want 2", saved.Members)
	}
	if saved.Members[0].SecretRef != "ref-inplace-a" || saved.Members[1].SecretRef != "ref-inplace-b" {
		t.Fatalf("secret refs changed after id-based patch: %+v", saved.Members)
	}
	if saved.Members[0].Weight != 3 {
		t.Fatalf("member[0] weight=%d; want 3", saved.Members[0].Weight)
	}
	for _, member := range members {
		value, ok, err := secrets.Get(member.SecretRef)
		if err != nil || !ok || value != "original-"+member.Label {
			t.Fatalf("secret %s value=%q ok=%v err=%v; want original-%s", member.SecretRef, value, ok, err, member.Label)
		}
	}
}

// TestUpdateProviderChannelRemovesOrphanedSecretOnMemberRemoval pins H2: a
// member dropped from the patch must have its SecretRef deleted from the store,
// while the surviving members' keys stay resolvable.
func TestUpdateProviderChannelRemovesOrphanedSecretOnMemberRemoval(t *testing.T) {
	ctx := context.Background()
	channelID := "channel-rm"
	members := []domain.ProviderChannelMember{
		{ID: "m1", ChannelID: channelID, Label: "key-a", SecretRef: "ref-rm-a", Enabled: true, Weight: 1, MaxInflight: 1},
		{ID: "m2", ChannelID: channelID, Label: "key-b", SecretRef: "ref-rm-b", Enabled: true, Weight: 1, MaxInflight: 1},
		{ID: "m3", ChannelID: channelID, Label: "key-c", SecretRef: "ref-rm-c", Enabled: true, Weight: 1, MaxInflight: 1},
	}
	service, secrets := testServiceWithChannel(t, providerChannelFixture(channelID, members), nil)
	for _, member := range members {
		if err := secrets.Put(member.SecretRef, "key-"+member.Label[len(member.Label)-1:]); err != nil {
			t.Fatal(err)
		}
	}

	saved, err := service.UpdateProviderChannel(ctx, channelID, ProviderChannelUpdate{
		Members: &[]ProviderChannelMemberUpdate{
			{ID: "m1", Label: "key-a"},
			{ID: "m2", Label: "key-b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Members) != 2 {
		t.Fatalf("saved members=%+v; want 2", saved.Members)
	}
	if _, ok, err := secrets.Get("ref-rm-c"); err != nil || ok {
		t.Fatalf("removed member's secret should be gone: ok=%v err=%v", ok, err)
	}
	for _, ref := range []string{"ref-rm-a", "ref-rm-b"} {
		if _, ok, err := secrets.Get(ref); err != nil || !ok {
			t.Fatalf("surviving member's secret %s missing: ok=%v err=%v", ref, ok, err)
		}
	}
}

// TestSaveProviderChannelAssignsDistinctSecretRefsPerMember pins the property
// that makes a shared-ref channel unreachable from this layer: on a create the
// service gives every member its own SecretRef
// (provider-channel/<channelID>/<memberID>, member IDs from idgen), and
// UNIQUE(secret_ref) (migration 0013) means two members on one ref is a state
// the database cannot store — verified against real SQLite in
// TestProviderChannelMembersRejectSharedSecretRef
// (internal/repository/sqlite/provider_channel_secret_ref_test.go). The test
// that previously lived here
// (TestUpdateProviderChannelSharedSecretRefSurvivesRemoval) seeded two members
// on one ref against an in-memory fake and asserted the ref survived a member
// removal; that seed is fiction, so the surviving-reference branch of
// deleteOrphanedMemberSecrets it exercised is unreachable. The reachable shape
// of the same invariant — removing a member prunes exactly that member's ref —
// is covered by TestUpdateProviderChannelRemovesOrphanedSecretOnMemberRemoval.
func TestSaveProviderChannelAssignsDistinctSecretRefsPerMember(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: &fakeUpsertOnlyRepo{}, cfg: config.Config{DataDir: dataDir}, secrets: secrets}

	channel := domain.ProviderChannel{
		Capability:   "video_analysis",
		Label:        "Distinct refs channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{Label: "key-a", Enabled: true, Weight: 1, MaxInflight: 1},
			{Label: "key-b", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}
	saved, err := service.SaveProviderChannel(ctx, channel, []string{"key-a", "key-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Members) != 2 {
		t.Fatalf("saved members=%+v; want 2", saved.Members)
	}
	if saved.Members[0].SecretRef == "" || saved.Members[0].SecretRef == saved.Members[1].SecretRef {
		t.Fatalf("members must get distinct non-empty refs, got %q and %q", saved.Members[0].SecretRef, saved.Members[1].SecretRef)
	}
	for i, member := range saved.Members {
		if !strings.HasPrefix(member.SecretRef, "provider-channel/"+saved.ID+"/") {
			t.Fatalf("member[%d] ref %q should be scoped to channel %q", i, member.SecretRef, saved.ID)
		}
		value, ok, err := secrets.Get(member.SecretRef)
		if err != nil || !ok || value != []string{"key-a", "key-b"}[i] {
			t.Fatalf("member[%d] key=%q ok=%v err=%v; want %q", i, value, ok, err, []string{"key-a", "key-b"}[i])
		}
	}
}

// TestUpdateProviderChannelSaveFailureLeavesEverySecretIntact pins the H2
// ordering requirement: orphaned-secret deletion happens strictly after the
// save succeeds. When the save is rejected, the channel still references every
// pre-existing ref, so none of them may be deleted.
func TestUpdateProviderChannelSaveFailureLeavesEverySecretIntact(t *testing.T) {
	ctx := context.Background()
	channelID := "channel-fail"
	members := []domain.ProviderChannelMember{
		{ID: "m1", ChannelID: channelID, Label: "key-a", SecretRef: "ref-fail-a", Enabled: true, Weight: 1, MaxInflight: 1},
		{ID: "m2", ChannelID: channelID, Label: "key-b", SecretRef: "ref-fail-b", Enabled: true, Weight: 1, MaxInflight: 1},
		{ID: "m3", ChannelID: channelID, Label: "key-c", SecretRef: "ref-fail-c", Enabled: true, Weight: 1, MaxInflight: 1},
	}
	repo := &fakeChannelRepo{upsertErr: errFakeUpsertRejected}
	service, secrets := testServiceWithChannel(t, providerChannelFixture(channelID, members), repo)
	for _, member := range members {
		if err := secrets.Put(member.SecretRef, "key-"+member.Label[len(member.Label)-1:]); err != nil {
			t.Fatal(err)
		}
	}

	// The patch drops m3; had the save succeeded its ref would be pruned.
	_, err := service.UpdateProviderChannel(ctx, channelID, ProviderChannelUpdate{
		Members: &[]ProviderChannelMemberUpdate{
			{ID: "m1", Label: "key-a"},
			{ID: "m2", Label: "key-b"},
		},
	})
	if !errors.Is(err, errFakeUpsertRejected) {
		t.Fatalf("err=%v; want errFakeUpsertRejected", err)
	}
	for _, ref := range []string{"ref-fail-a", "ref-fail-b", "ref-fail-c"} {
		if _, ok, err := secrets.Get(ref); err != nil || !ok {
			t.Fatalf("secret %s should be intact after a failed save: ok=%v err=%v", ref, ok, err)
		}
	}
}

// TestWorkerProviderCredentialErrorsDistinguishChannelOnlyFromNotConfigured
// pins the refusal IssueWorkerCredential and issueProviderCredentialForProxy
// give a Worker apart for two failures that used to look identical: a Hub
// with a provider channel configured for a capability and nothing in
// providers.*, versus a Hub with nothing configured for that capability by
// either means. Both paths build their credentials.Broker from
// s.cfg.Providers only -- CLAUDE.md's Worker trust boundary keeps Worker
// provider access legacy-config-only on purpose, so channel-scoped keys,
// member pools and health state never leave the Hub -- but before
// classifyWorkerProviderBrokerErr existed, that meant a channel-configured
// capability failed with the exact same "not enabled"/"has no endpoint" (or,
// on the proxy path, a flat "provider proxy is not configured" that
// discarded even that) as a capability nobody had touched, telling the
// operator their configuration was wrong when it was only in the wrong
// place for this path. This drives both Service methods directly, not the
// broker: reusing fakeChannelRepo (ListProviderChannels only, everything
// else panics via the embedded nil Repository) is what proves the
// distinction is read from the channel repository Service already holds,
// not invented by calling credentials.Broker a second time with different
// input.
func TestWorkerProviderCredentialErrorsDistinguishChannelOnlyFromNotConfigured(t *testing.T) {
	ctx := context.Background()
	worker := remote.Worker{ID: "worker-1", Capabilities: remote.WorkerCapabilities{ProviderOperations: []string{"video_analysis"}}}
	cfg := config.Config{}
	cfg.HubSecurity.AllowWorkerProviderCredentials = true

	t.Run("channel configured, no legacy config", func(t *testing.T) {
		repo := &fakeChannelRepo{channel: providerChannelFixture("channel-video", nil)}
		service := &Service{repo: repo, cfg: cfg}

		if _, err := service.IssueWorkerCredential(ctx, worker, "job-1", credentials.OperationVideoAnalysis); !errors.Is(err, ErrWorkerProviderConfiguredAsChannelOnly) {
			t.Fatalf("IssueWorkerCredential err=%v; want ErrWorkerProviderConfiguredAsChannelOnly", err)
		} else if errors.Is(err, ErrWorkerProviderNotConfigured) {
			t.Fatalf("IssueWorkerCredential err=%v; must not also match ErrWorkerProviderNotConfigured", err)
		}

		if _, err := service.issueProviderCredentialForProxy(ctx, worker, "job-1", credentials.OperationVideoAnalysis); !errors.Is(err, ErrWorkerProviderConfiguredAsChannelOnly) {
			t.Fatalf("issueProviderCredentialForProxy err=%v; want ErrWorkerProviderConfiguredAsChannelOnly", err)
		} else if errors.Is(err, ErrWorkerProviderNotConfigured) {
			t.Fatalf("issueProviderCredentialForProxy err=%v; must not also match ErrWorkerProviderNotConfigured", err)
		}
	})

	t.Run("nothing configured anywhere", func(t *testing.T) {
		repo := &fakeChannelRepo{}
		service := &Service{repo: repo, cfg: cfg}

		if _, err := service.IssueWorkerCredential(ctx, worker, "job-1", credentials.OperationVideoAnalysis); !errors.Is(err, ErrWorkerProviderNotConfigured) {
			t.Fatalf("IssueWorkerCredential err=%v; want ErrWorkerProviderNotConfigured", err)
		} else if errors.Is(err, ErrWorkerProviderConfiguredAsChannelOnly) {
			t.Fatalf("IssueWorkerCredential err=%v; must not match ErrWorkerProviderConfiguredAsChannelOnly", err)
		}

		if _, err := service.issueProviderCredentialForProxy(ctx, worker, "job-1", credentials.OperationVideoAnalysis); !errors.Is(err, ErrWorkerProviderNotConfigured) {
			t.Fatalf("issueProviderCredentialForProxy err=%v; want ErrWorkerProviderNotConfigured", err)
		} else if errors.Is(err, ErrWorkerProviderConfiguredAsChannelOnly) {
			t.Fatalf("issueProviderCredentialForProxy err=%v; must not match ErrWorkerProviderConfiguredAsChannelOnly", err)
		}
	})
}

// probeTestChannel builds the channel shape the TestProviderChannel probe
// tests share: one enabled member whose secret lives in the store, an
// OpenAI-compatible provider name (so the /models probe applies), and an
// endpoint pointing at the given fixture server.
func probeTestChannel(t *testing.T, server *httptest.Server) (*Service, string, string) {
	t.Helper()
	channelID := "channel-probe"
	member := domain.ProviderChannelMember{
		ID: "member-1", ChannelID: channelID, Label: "probe",
		SecretRef: "provider-channel/" + channelID + "/member-1",
		Enabled:   true, Weight: 1, MaxInflight: 1,
	}
	channel := providerChannelFixture(channelID, []domain.ProviderChannelMember{member})
	channel.ProviderName = "openai_chat"
	channel.Endpoint = server.URL
	service, secrets := testServiceWithChannel(t, channel, nil)
	if err := secrets.Put(member.SecretRef, "probe-secret-key"); err != nil {
		t.Fatal(err)
	}
	return service, channelID, "probe-secret-key"
}

// TestProviderChannelTestProbesModelsEndpoint pins the C2 upgrade: after the
// reachability GET, an OpenAI-compatible channel gets one non-billed GET
// {endpoint}/models carrying its resolved key, and a non-empty listing
// reports the first model id. The key must ride in the Authorization header
// and nowhere in the result.
func TestProviderChannelTestProbesModelsEndpoint(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"probe-model-a"},{"id":"probe-model-b"}]}`))
	}))
	defer server.Close()

	service, channelID, key := probeTestChannel(t, server)
	result, err := service.TestProviderChannel(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.SecretReady || !result.Reachable {
		t.Fatalf("secret_ready=%v reachable=%v; want both true: %+v", result.SecretReady, result.Reachable, result)
	}
	if !result.ModelResponded || !result.SchemaOK {
		t.Fatalf("model_responded=%v schema_ok=%v; want both true: %+v", result.ModelResponded, result.SchemaOK, result)
	}
	if result.ModelName != "probe-model-a" {
		t.Fatalf("model_name=%q; want probe-model-a", result.ModelName)
	}
	if result.Status != "reachable" {
		t.Fatalf("status=%q; want reachable", result.Status)
	}
	if gotAuth != "Bearer "+key {
		t.Fatalf("probe Authorization=%q; want Bearer %s", gotAuth, key)
	}
	assertTestResultHasNoKey(t, result, key)
}

// TestProviderChannelTestReportsRejectedKey pins the 401/403 verdict: the
// endpoint is reachable, but the stored key is rejected, and the message says
// so without ever echoing the key or the upstream body.
func TestProviderChannelTestReportsRejectedKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	service, channelID, key := probeTestChannel(t, server)
	result, err := service.TestProviderChannel(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reachable {
		t.Fatalf("reachable=%v; want true: %+v", result.Reachable, result)
	}
	if result.ModelResponded || result.SchemaOK {
		t.Fatalf("model_responded=%v schema_ok=%v; want both false: %+v", result.ModelResponded, result.SchemaOK, result)
	}
	if !strings.Contains(result.Message, "API Key 无效") {
		t.Fatalf("message=%q; want key-rejection wording", result.Message)
	}
	if !strings.Contains(result.Message, "401") {
		t.Fatalf("message=%q; want the HTTP status named", result.Message)
	}
	assertTestResultHasNoKey(t, result, key)
}

// TestProviderChannelTestReportsSchemaUnknownOn404 pins that a missing /models
// endpoint is a schema note, not a failure: the channel is still marked
// reachable and the message says no billed call was made.
func TestProviderChannelTestReportsSchemaUnknownOn404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	service, channelID, key := probeTestChannel(t, server)
	result, err := service.TestProviderChannel(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reachable {
		t.Fatalf("reachable=%v; want true: %+v", result.Reachable, result)
	}
	if result.ModelResponded || result.SchemaOK {
		t.Fatalf("model_responded=%v schema_ok=%v; want both false: %+v", result.ModelResponded, result.SchemaOK, result)
	}
	if !strings.Contains(result.Message, "schema 未知") || !strings.Contains(result.Message, "未执行计费模型调用") {
		t.Fatalf("message=%q; want schema-unknown + no-billed-call wording", result.Message)
	}
	assertTestResultHasNoKey(t, result, key)
}

// TestProviderChannelTestSkipsModelsProbeForGemini pins the no-probe
// providers: a Gemini channel must never hit /models — its key rides in a
// query parameter and its model list lives under a different shape — and the
// result says no billed call was made.
func TestProviderChannelTestSkipsModelsProbeForGemini(t *testing.T) {
	modelsHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			modelsHits++
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	channelID := "channel-gemini"
	member := domain.ProviderChannelMember{
		ID: "member-1", ChannelID: channelID, Label: "probe",
		SecretRef: "provider-channel/" + channelID + "/member-1",
		Enabled:   true, Weight: 1, MaxInflight: 1,
	}
	channel := providerChannelFixture(channelID, []domain.ProviderChannelMember{member})
	channel.ProviderName = "gemini"
	channel.Endpoint = server.URL
	service, secrets := testServiceWithChannel(t, channel, nil)
	if err := secrets.Put(member.SecretRef, "gemini-secret-key"); err != nil {
		t.Fatal(err)
	}

	result, err := service.TestProviderChannel(context.Background(), channelID)
	if err != nil {
		t.Fatal(err)
	}
	if modelsHits != 0 {
		t.Fatalf("gemini channel hit /models %d times; want 0", modelsHits)
	}
	if !result.Reachable || result.ModelResponded || result.SchemaOK {
		t.Fatalf("reachable=%v model_responded=%v schema_ok=%v; want reachable only: %+v", result.Reachable, result.ModelResponded, result.SchemaOK, result)
	}
	if !strings.Contains(result.Message, "未执行计费模型调用") {
		t.Fatalf("message=%q; want no-billed-call wording", result.Message)
	}
	assertTestResultHasNoKey(t, result, "gemini-secret-key")
}

func assertTestResultHasNoKey(t *testing.T, result ProviderChannelTestResult, key string) {
	t.Helper()
	if strings.Contains(result.Message, key) || strings.Contains(result.ModelName, key) {
		t.Fatalf("test result leaked the key: %+v", result)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("marshaled test result leaked the key: %s", raw)
	}
}
