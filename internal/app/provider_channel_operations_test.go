package app

import (
	"context"
	"errors"
	"testing"

	"github.com/ev/timingdex/internal/config"
	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/secretstore"
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

// TestSaveProviderChannelKeepsDuplicateLabelKeysDistinct guards the positional
// contract between memberKeys and channel.Members.
//
// Keys used to be indexed by member.Label, which silently collapsed two
// same-label members onto one key. Today provider_channel_members carries
// UNIQUE(channel_id,label), so the database rejects that shape before it can
// do damage — this is a deliberately defensive test, not a reproduction of a
// reachable bug. The point is that key assignment must be correct on its own
// terms rather than borrowing its correctness from a schema constraint that a
// future migration could relax.
func TestSaveProviderChannelKeepsDuplicateLabelKeysDistinct(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{repo: &fakeUpsertOnlyRepo{}, cfg: config.Config{DataDir: dataDir}, secrets: secrets}

	channel := domain.ProviderChannel{
		Capability:   "video_analysis",
		Label:        "Duplicate label channel",
		ProviderName: "gemini",
		Endpoint:     "https://example.invalid",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{Label: "same-label", Enabled: true, Weight: 1, MaxInflight: 1},
			{Label: "same-label", Enabled: true, Weight: 1, MaxInflight: 1},
		},
	}
	saved, err := service.SaveProviderChannel(ctx, channel, []string{"key-for-member-a", "key-for-member-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Members) != 2 {
		t.Fatalf("saved members=%+v; want 2", saved.Members)
	}
	if saved.Members[0].SecretRef == "" || saved.Members[1].SecretRef == "" || saved.Members[0].SecretRef == saved.Members[1].SecretRef {
		t.Fatalf("expected two distinct non-empty secret refs, got %q and %q", saved.Members[0].SecretRef, saved.Members[1].SecretRef)
	}

	valueA, ok, err := secrets.Get(saved.Members[0].SecretRef)
	if err != nil || !ok {
		t.Fatalf("member[0] secret missing: ok=%v err=%v", ok, err)
	}
	valueB, ok, err := secrets.Get(saved.Members[1].SecretRef)
	if err != nil || !ok {
		t.Fatalf("member[1] secret missing: ok=%v err=%v", ok, err)
	}
	if valueA != "key-for-member-a" {
		t.Fatalf("member[0] key=%q; want key-for-member-a", valueA)
	}
	if valueB != "key-for-member-b" {
		t.Fatalf("member[1] key=%q; want key-for-member-b", valueB)
	}
}

// TestSaveProviderChannelShortMemberKeysSliceLeavesTrailingMembersUnchanged
// documents that a memberKeys slice shorter than channel.Members must not
// panic and must be treated as "no key" for the missing tail, matching the
// same-length contract the API and patch handlers rely on.
func TestSaveProviderChannelShortMemberKeysSliceLeavesTrailingMembersUnchanged(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
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
	dataDir := t.TempDir()
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
