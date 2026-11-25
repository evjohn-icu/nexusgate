package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/evjohn-icu/nexusgate/internal/config"
	"github.com/evjohn-icu/nexusgate/internal/domain"
	"github.com/evjohn-icu/nexusgate/internal/secretstore"
)

// fakeChannelStateRepo is the narrow Repository double for the provider
// channel patch/delete tests: in-memory channel rows with their member
// references, an injectable upsert failure and an injectable soft-delete
// failure. Everything else panics through the embedded nil interface, which
// keeps the exercised surface honest.
type fakeChannelStateRepo struct {
	Repository
	channels  []domain.ProviderChannel
	upserts   int
	upsertErr error
	softErr   error
	softened  []string
}

func (r *fakeChannelStateRepo) ListProviderChannels(_ context.Context, _ string) ([]domain.ProviderChannel, error) {
	out := make([]domain.ProviderChannel, len(r.channels))
	for i := range r.channels {
		out[i] = r.channels[i]
		out[i].Members = append([]domain.ProviderChannelMember(nil), r.channels[i].Members...)
	}
	return out, nil
}

func (r *fakeChannelStateRepo) UpsertProviderChannel(_ context.Context, channel domain.ProviderChannel) (domain.ProviderChannel, error) {
	if r.upsertErr != nil {
		return domain.ProviderChannel{}, r.upsertErr
	}
	r.upserts++
	stored := channel
	stored.Members = append([]domain.ProviderChannelMember(nil), channel.Members...)
	for i := range r.channels {
		if r.channels[i].ID == channel.ID {
			r.channels[i] = stored
			return channel, nil
		}
	}
	r.channels = append(r.channels, stored)
	return channel, nil
}

func (r *fakeChannelStateRepo) SoftDeleteProviderChannel(_ context.Context, id string) error {
	if r.softErr != nil {
		return r.softErr
	}
	r.softened = append(r.softened, id)
	return nil
}

func newProviderChannelStateService(t *testing.T) (*Service, *fakeChannelStateRepo, string) {
	t.Helper()
	dataDir := t.TempDir()
	secrets, err := secretstore.Open(dataDir, "test-admin-token")
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeChannelStateRepo{}
	service := &Service{repo: repo, cfg: config.Config{DataDir: dataDir}, secrets: secrets}
	return service, repo, dataDir
}

// seedChannelState plants a three-member channel whose members carry stored
// versioned references, and writes a synthetic key behind each reference.
// The keys are fixtures, never real credentials.
func seedChannelState(t *testing.T, service *Service, repo *fakeChannelStateRepo) domain.ProviderChannel {
	t.Helper()
	channel := domain.ProviderChannel{
		ID:           "channel-origin",
		Capability:   "video_analysis",
		Label:        "Origin channel",
		ProviderName: "gemini",
		Endpoint:     "https://old.example.invalid/v1",
		Model:        "gemini-flash",
		Enabled:      true,
		Members: []domain.ProviderChannelMember{
			{ID: "member-a", ChannelID: "channel-origin", Label: "primary", Enabled: true, Weight: 1, MaxInflight: 1, SecretRef: "provider-channel/channel-origin/member-a/ref-a"},
			{ID: "member-b", ChannelID: "channel-origin", Label: "backup", Enabled: true, Weight: 2, MaxInflight: 3, SecretRef: "provider-channel/channel-origin/member-b/ref-b"},
			{ID: "member-c", ChannelID: "channel-origin", Label: "spare", Enabled: true, Weight: 1, MaxInflight: 1, SecretRef: "provider-channel/channel-origin/member-c/ref-c"},
		},
	}
	repo.channels = []domain.ProviderChannel{channel}
	for i, member := range channel.Members {
		if err := service.secrets.Put(member.SecretRef, "synthetic-key-"+string(rune('A'+i))); err != nil {
			t.Fatal(err)
		}
	}
	return channel
}

// providerSecretFiles lists the secret store's on-disk file names so a
// rejected write can be proven not to have staged anything.
func providerSecretFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, "provider-secrets"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

func memberUpdate(id, label string, weight, maxInflight int, apiKey string) ProviderChannelMemberUpdate {
	enabled := true
	return ProviderChannelMemberUpdate{ID: id, Label: label, APIKey: apiKey, Enabled: &enabled, Weight: &weight, MaxInflight: &maxInflight}
}

func secretValue(t *testing.T, service *Service, ref string) (string, bool) {
	t.Helper()
	value, ok, err := service.secrets.Get(ref)
	if err != nil {
		t.Fatalf("secret get %q: %v", ref, err)
	}
	return value, ok
}

// A cross-origin endpoint edit without replacement keys must be rejected
// before a single secret is staged: no repository write, no new store file,
// and the stored channel keeps its old endpoint and every key.
func TestUpdateProviderChannelCrossOriginEditWithoutReplacementKeysIsRejected(t *testing.T) {
	ctx := context.Background()
	service, repo, dataDir := newProviderChannelStateService(t)
	channel := seedChannelState(t, service, repo)
	before := providerSecretFiles(t, dataDir)

	patch := ProviderChannelUpdate{
		Endpoint: ptrString("https://other.example.invalid/v1"),
		Members: &[]ProviderChannelMemberUpdate{
			memberUpdate("member-a", "primary", 1, 1, ""),
			memberUpdate("member-b", "backup", 2, 3, ""),
			memberUpdate("member-c", "spare", 1, 1, ""),
		},
	}
	_, err := service.UpdateProviderChannel(ctx, channel.ID, patch)
	if !errors.Is(err, ErrProviderChannelValidation) {
		t.Fatalf("cross-origin edit without replacement keys must wrap ErrProviderChannelValidation, got %v", err)
	}
	if repo.upserts != 0 {
		t.Fatalf("rejected edit must not reach the repository (upserts=%d)", repo.upserts)
	}
	if after := providerSecretFiles(t, dataDir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("rejected edit must not stage or delete any secret: before=%v after=%v", before, after)
	}
	saved, err := service.providerChannelByID(ctx, channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Endpoint != channel.Endpoint || len(saved.Members) != 3 {
		t.Fatalf("stored channel must be untouched: endpoint=%q members=%d", saved.Endpoint, len(saved.Members))
	}
	for _, member := range saved.Members {
		if value, ok := secretValue(t, service, member.SecretRef); !ok || !strings.HasPrefix(value, "synthetic-key-") {
			t.Fatalf("member %q kept key must survive: ref=%q ok=%v", member.Label, member.SecretRef, ok)
		}
	}
}

// A cross-origin edit that does supply a replacement key for every retained
// member succeeds, retires the old references and resolves every surviving
// member's new versioned reference to the new value.
func TestUpdateProviderChannelCrossOriginEditWithReplacementKeysSucceeds(t *testing.T) {
	ctx := context.Background()
	service, repo, _ := newProviderChannelStateService(t)
	channel := seedChannelState(t, service, repo)
	oldRefs := make([]string, 0, len(channel.Members))
	for _, member := range channel.Members {
		oldRefs = append(oldRefs, member.SecretRef)
	}

	patch := ProviderChannelUpdate{
		Endpoint: ptrString("https://other.example.invalid/v1"),
		Members: &[]ProviderChannelMemberUpdate{
			memberUpdate("member-a", "primary", 1, 1, "synthetic-replacement-a"),
			memberUpdate("member-b", "backup", 2, 3, "synthetic-replacement-b"),
			memberUpdate("member-c", "spare", 1, 1, "synthetic-replacement-c"),
		},
	}
	saved, err := service.UpdateProviderChannel(ctx, channel.ID, patch)
	if err != nil {
		t.Fatalf("cross-origin edit with replacement keys must succeed: %v", err)
	}
	if saved.Endpoint != "https://other.example.invalid/v1" {
		t.Fatalf("endpoint = %q", saved.Endpoint)
	}
	for i, member := range saved.Members {
		if !strings.HasPrefix(member.SecretRef, "provider-channel/channel-origin/"+member.ID+"/") {
			t.Fatalf("member %q ref %q is not a versioned channel reference", member.ID, member.SecretRef)
		}
		if value, ok := secretValue(t, service, member.SecretRef); !ok || value != "synthetic-replacement-"+string(rune('a'+i)) {
			t.Fatalf("member %q must resolve to its replacement key: ok=%v", member.ID, ok)
		}
	}
	for _, ref := range oldRefs {
		if _, ok := secretValue(t, service, ref); ok {
			t.Fatalf("retired reference %q must be deleted after the origin change", ref)
		}
	}
}

// A same-origin endpoint edit — different path, differently spelled default
// port — keeps every stored reference and every stored value, and still
// applies the member label and option changes carried by the same patch.
func TestUpdateProviderChannelSameOriginEditKeepsStoredRefsAndOptions(t *testing.T) {
	ctx := context.Background()
	service, repo, _ := newProviderChannelStateService(t)
	channel := seedChannelState(t, service, repo)

	patch := ProviderChannelUpdate{
		Endpoint: ptrString("https://old.example.invalid:443/v2"),
		Members: &[]ProviderChannelMemberUpdate{
			memberUpdate("member-a", "primary-relabeled", 4, 5, ""),
			memberUpdate("member-b", "backup", 2, 3, ""),
		},
	}
	saved, err := service.UpdateProviderChannel(ctx, channel.ID, patch)
	if err != nil {
		t.Fatalf("same-origin edit must succeed: %v", err)
	}
	if len(saved.Members) != 2 {
		t.Fatalf("member-c must be removed by the patch, got %d members", len(saved.Members))
	}
	refs := map[string]string{}
	for _, member := range saved.Members {
		refs[member.ID] = member.SecretRef
	}
	if refs["member-a"] != "provider-channel/channel-origin/member-a/ref-a" || refs["member-b"] != "provider-channel/channel-origin/member-b/ref-b" {
		t.Fatalf("same-origin edit must preserve stored references, got %v", refs)
	}
	if value, ok := secretValue(t, service, refs["member-a"]); !ok || value != "synthetic-key-A" {
		t.Fatalf("kept member must still resolve to its stored key: ok=%v", ok)
	}
	if _, ok := secretValue(t, service, "provider-channel/channel-origin/member-c/ref-c"); ok {
		t.Fatal("removed member's reference must be retired")
	}
	if saved.Members[0].Label != "primary-relabeled" || saved.Members[0].Weight != 4 || saved.Members[0].MaxInflight != 5 {
		t.Fatalf("label and options must apply: %+v", saved.Members[0])
	}
}

// Rows must stay bound to their own keys across removal and reordering:
// resending [c, a] with no keys retires b's reference and leaves c's and a's
// references exactly as they were, regardless of row order.
func TestUpdateProviderChannelRemovalAndReorderKeepKeysBoundToSurvivingRows(t *testing.T) {
	ctx := context.Background()
	service, repo, _ := newProviderChannelStateService(t)
	channel := seedChannelState(t, service, repo)

	patch := ProviderChannelUpdate{
		Members: &[]ProviderChannelMemberUpdate{
			memberUpdate("member-c", "spare", 1, 1, ""),
			memberUpdate("member-a", "primary", 1, 1, ""),
		},
	}
	saved, err := service.UpdateProviderChannel(ctx, channel.ID, patch)
	if err != nil {
		t.Fatalf("reorder+removal patch must succeed: %v", err)
	}
	if len(saved.Members) != 2 || saved.Members[0].ID != "member-c" || saved.Members[1].ID != "member-a" {
		t.Fatalf("saved order must follow the patch: %+v", saved.Members)
	}
	if saved.Members[0].SecretRef != "provider-channel/channel-origin/member-c/ref-c" || saved.Members[1].SecretRef != "provider-channel/channel-origin/member-a/ref-a" {
		t.Fatalf("surviving rows must keep their own references: %+v", saved.Members)
	}
	if _, ok := secretValue(t, service, "provider-channel/channel-origin/member-b/ref-b"); ok {
		t.Fatal("removed member-b's reference must be retired")
	}
	for _, member := range saved.Members {
		if _, ok := secretValue(t, service, member.SecretRef); !ok {
			t.Fatalf("surviving reference %q must still resolve", member.SecretRef)
		}
	}
}

// Saving an empty member set is the existing server behavior for an empty
// channel: the channel row survives with zero members and every key is
// retired.
func TestUpdateProviderChannelEmptyMemberSetSavesEmptyChannel(t *testing.T) {
	ctx := context.Background()
	service, repo, _ := newProviderChannelStateService(t)
	channel := seedChannelState(t, service, repo)

	patch := ProviderChannelUpdate{Members: &[]ProviderChannelMemberUpdate{}}
	saved, err := service.UpdateProviderChannel(ctx, channel.ID, patch)
	if err != nil {
		t.Fatalf("empty member set must save: %v", err)
	}
	if len(saved.Members) != 0 {
		t.Fatalf("channel must have no members, got %+v", saved.Members)
	}
	for _, member := range channel.Members {
		if _, ok := secretValue(t, service, member.SecretRef); ok {
			t.Fatalf("reference %q must be retired once no member references it", member.SecretRef)
		}
	}
	if stored := repo.channels[0]; len(stored.Members) != 0 {
		t.Fatalf("repository must persist the empty member set, got %+v", stored.Members)
	}
}

// The tombstone must be written before any key is retired: when the
// soft-delete fails, the delete fails with every key still in place, and the
// channel is still resolvable. A successful delete retires the keys only
// after the tombstone is recorded.
func TestDeleteProviderChannelWritesTombstoneBeforeRetiringKeys(t *testing.T) {
	ctx := context.Background()
	service, repo, _ := newProviderChannelStateService(t)
	channel := seedChannelState(t, service, repo)

	repo.softErr = errors.New("synthetic soft-delete failure")
	if err := service.DeleteProviderChannel(ctx, channel.ID); err == nil {
		t.Fatal("a failed tombstone write must fail the delete")
	}
	for _, member := range channel.Members {
		if value, ok := secretValue(t, service, member.SecretRef); !ok || !strings.HasPrefix(value, "synthetic-key-") {
			t.Fatalf("a failed tombstone write must retain every key: ref=%q ok=%v", member.SecretRef, ok)
		}
	}
	if _, err := service.providerChannelByID(ctx, channel.ID); err != nil {
		t.Fatalf("a failed tombstone write must leave the channel in place: %v", err)
	}
	if len(repo.softened) != 0 {
		t.Fatalf("no tombstone must be recorded on failure: %v", repo.softened)
	}

	repo.softErr = nil
	if err := service.DeleteProviderChannel(ctx, channel.ID); err != nil {
		t.Fatalf("successful delete: %v", err)
	}
	if len(repo.softened) != 1 || repo.softened[0] != channel.ID {
		t.Fatalf("tombstone must be recorded for the channel: %v", repo.softened)
	}
	for _, member := range channel.Members {
		if _, ok := secretValue(t, service, member.SecretRef); ok {
			t.Fatalf("reference %q must be retired after a successful tombstone", member.SecretRef)
		}
	}
}

// Direct unit coverage of the origin rule the two save paths share: same
// origin under different spellings passes, an endpoint is validated by being
// same-origin with itself (HTTP(S), sane host and effective port), and no
// error ever quotes the raw endpoint text (a URL can carry userinfo).
func TestValidateProviderChannelOriginChangeRule(t *testing.T) {
	members := []domain.ProviderChannelMember{
		{ID: "m", Label: "kept", SecretRef: "provider-channel/c/m/r"},
	}
	keys := []string{""}

	for _, same := range [][2]string{
		{"https://api.example.invalid/v1", "https://api.example.invalid/v9"},
		{"https://api.example.invalid/v1", "https://api.example.invalid:443"},
		{"https://api.example.invalid:443/x", "HTTPS://API.EXAMPLE.INVALID/"},
		{"http://api.example.invalid", "http://api.example.invalid:80/"},
	} {
		if err := validateProviderChannelOriginChange(same[0], same[1], members, keys); err != nil {
			t.Fatalf("endpoints %q and %q are the same origin, got %v", same[0], same[1], err)
		}
	}

	if err := validateProviderChannelOriginChange("https://a.example.invalid", "https://b.example.invalid", members, keys); err == nil {
		t.Fatal("a different host is a different origin and requires a replacement key")
	} else {
		if !errors.Is(err, ErrProviderChannelValidation) {
			t.Fatalf("origin mismatch must wrap ErrProviderChannelValidation, got %v", err)
		}
		// The mismatch error names the member, never an endpoint: the URL
		// text can carry embedded userinfo.
		if strings.Contains(err.Error(), "a.example.invalid") || strings.Contains(err.Error(), "b.example.invalid") {
			t.Fatalf("origin-mismatch error must not quote endpoint text: %q", err.Error())
		}
	}
	// A member with nothing stored needs no replacement key.
	if err := validateProviderChannelOriginChange("https://a.example.invalid", "https://b.example.invalid", []domain.ProviderChannelMember{{ID: "n", Label: "new"}}, nil); err != nil {
		t.Fatalf("a member without a stored reference must pass: %v", err)
	}
	// A replacement key satisfies the requirement.
	if err := validateProviderChannelOriginChange("https://a.example.invalid", "https://b.example.invalid", members, []string{"synthetic-replacement"}); err != nil {
		t.Fatalf("a supplied replacement key must pass: %v", err)
	}
	// Untouched endpoints never trip the rule, whatever their shape.
	if err := validateProviderChannelOriginChange("not a url", "not a url", members, keys); err != nil {
		t.Fatalf("an untouched endpoint must not be re-parsed: %v", err)
	}
	if err := validateProviderChannelOriginChange("wss://openspeech.example.invalid/api", "wss://openspeech.example.invalid/api", members, keys); err != nil {
		t.Fatalf("an untouched wss endpoint must not be re-parsed: %v", err)
	}
	// A CHANGED endpoint that cannot prove an HTTP(S) origin with a sane
	// hostname and effective port — relative text, a non-HTTP(S) scheme —
	// fails as an invalid request, and the message names the side only.
	for _, invalid := range [][2]string{
		{"https://a.example.invalid", "not a url"},
		{"ftp://a.example.invalid", "https://b.example.invalid"},
		{"wss://openspeech.example.invalid/api", "wss://openspeech.example.invalid/api/v9"},
	} {
		err := validateProviderChannelOriginChange(invalid[0], invalid[1], members, keys)
		if !errors.Is(err, ErrProviderChannelValidation) {
			t.Fatalf("invalid origin %q/%q must fail as invalid request, got %v", invalid[0], invalid[1], err)
		}
		if strings.Contains(err.Error(), ".invalid") {
			t.Fatalf("invalid-origin error must not quote endpoint text: %q", err.Error())
		}
	}
}

func ptrString(v string) *string { return &v }
