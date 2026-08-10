package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/evjohn-icu/timingdex/internal/credentials"
	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/remote"
)

// ProviderChannelMemberUpdate is the write-only shape used by the admin API.
// APIKey is accepted only on writes and is never copied into a response.
type ProviderChannelMemberUpdate struct {
	ID          string `json:"id,omitempty"`
	Label       string `json:"label,omitempty"`
	APIKey      string `json:"api_key,omitempty"`
	Enabled     *bool  `json:"enabled,omitempty"`
	Weight      *int   `json:"weight,omitempty"`
	MaxInflight *int   `json:"max_inflight,omitempty"`
}

type ProviderChannelUpdate struct {
	Label              *string                        `json:"label,omitempty"`
	ProviderName       *string                        `json:"provider_name,omitempty"`
	Protocol           *string                        `json:"protocol,omitempty"`
	Endpoint           *string                        `json:"endpoint,omitempty"`
	Model              *string                        `json:"model,omitempty"`
	Enabled            *bool                          `json:"enabled,omitempty"`
	RouteOrder         *int                           `json:"route_order,omitempty"`
	CostPerRequest     *float64                       `json:"cost_per_request,omitempty"`
	CostPerVideoMinute *float64                       `json:"cost_per_video_minute,omitempty"`
	CostPerAudioMinute *float64                       `json:"cost_per_audio_minute,omitempty"`
	Members            *[]ProviderChannelMemberUpdate `json:"members,omitempty"`
}

// ErrProviderChannelValidation is wrapped by a provider-channel write failure
// that is about what the operator submitted — today, two members sharing a
// label — as opposed to a failure in a layer downstream of validation
// (SaveProviderChannel also calls UpsertProviderChannel and the secret
// store). The API layer uses errors.Is against this sentinel to decide
// whether an error's text is safe to put in an HTTP response body: only text
// wrapping this sentinel was written to be shown to an operator. This
// follows the same shape as ErrPlanNotApproved/ErrPlanNotExportable in
// export.go rather than inventing a new one.
var ErrProviderChannelValidation = errors.New("provider channel validation failed")

// validateDistinctProviderChannelMemberLabels rejects a member list where two
// members share a label, case-insensitively on the trimmed value. Labels are
// unique per channel (UNIQUE(channel_id, label), migration 0013), so a
// duplicate would otherwise reach SQLite as a bare constraint error naming no
// label. Both SaveProviderChannel (create) and UpdateProviderChannel (patch)
// call this before writing anything — a rejected write must not create or
// overwrite a secret either — so the two paths cannot drift out of sync on
// what "duplicate" means.
func validateDistinctProviderChannelMemberLabels(members []domain.ProviderChannelMember) error {
	seenLabels := make(map[string]struct{}, len(members))
	for _, member := range members {
		labelKey := strings.ToLower(strings.TrimSpace(member.Label))
		if _, dup := seenLabels[labelKey]; dup {
			return fmt.Errorf("%w: provider channel member label %q is duplicated; labels are unique per channel", ErrProviderChannelValidation, strings.TrimSpace(member.Label))
		}
		seenLabels[labelKey] = struct{}{}
	}
	return nil
}

// validateProviderChannelCosts rejects a channel whose optional cost metadata
// is negative (or NaN, which JSON cannot carry but a direct caller could).
// The unit is unspecified and the estimate is a guide, so there is no upper
// bound — only the sign can be a mistake. It wraps ErrProviderChannelValidation
// so the API layer shows the message to the operator, like the duplicate-label
// check.
func validateProviderChannelCosts(channel domain.ProviderChannel) error {
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"cost_per_request", channel.CostPerRequest},
		{"cost_per_video_minute", channel.CostPerVideoMinute},
		{"cost_per_audio_minute", channel.CostPerAudioMinute},
	} {
		if field.value < 0 || math.IsNaN(field.value) {
			return fmt.Errorf("%w: provider channel %s must not be negative", ErrProviderChannelValidation, field.name)
		}
	}
	return nil
}

type ProviderChannelTestResult struct {
	ChannelID    string `json:"channel_id"`
	ProviderName string `json:"provider_name"`
	Status       string `json:"status"`
	SecretReady  bool   `json:"secret_ready"`
	Reachable    bool   `json:"reachable"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	LatencyMS    int64  `json:"latency_ms,omitempty"`
	Message      string `json:"message,omitempty"`
	// ModelResponded and SchemaOK are the capability-probe verdicts: whether
	// the endpoint answered a non-billed model-listing request and whether
	// that listing had the expected shape. SchemaOK=false covers both "no
	// /models here" (404) and "the response was not a model list"; it is a
	// schema note, not a hard failure — the channel may still work for its
	// capability.
	ModelResponded bool   `json:"model_responded"`
	ModelName      string `json:"model_name,omitempty"`
	SchemaOK       bool   `json:"schema_ok"`
}

// UpdateProviderChannel merges an admin patch with the persisted channel.
// Existing SecretRefs are retained when a member update omits api_key.
func (s *Service) UpdateProviderChannel(ctx context.Context, id string, patch ProviderChannelUpdate) (domain.ProviderChannel, error) {
	channel, err := s.providerChannelByID(ctx, id)
	if err != nil {
		return domain.ProviderChannel{}, err
	}
	if patch.Label != nil {
		channel.Label = strings.TrimSpace(*patch.Label)
	}
	if patch.ProviderName != nil {
		channel.ProviderName = strings.TrimSpace(*patch.ProviderName)
	}
	if patch.Protocol != nil {
		channel.Protocol = strings.TrimSpace(*patch.Protocol)
	}
	if patch.Endpoint != nil {
		channel.Endpoint = strings.TrimSpace(*patch.Endpoint)
	}
	if patch.Model != nil {
		channel.Model = strings.TrimSpace(*patch.Model)
	}
	if patch.Enabled != nil {
		channel.Enabled = *patch.Enabled
	}
	if patch.RouteOrder != nil {
		channel.RouteOrder = *patch.RouteOrder
	}
	if patch.CostPerRequest != nil {
		channel.CostPerRequest = *patch.CostPerRequest
	}
	if patch.CostPerVideoMinute != nil {
		channel.CostPerVideoMinute = *patch.CostPerVideoMinute
	}
	if patch.CostPerAudioMinute != nil {
		channel.CostPerAudioMinute = *patch.CostPerAudioMinute
	}
	if patch.Members != nil {
		members := make([]domain.ProviderChannelMember, 0, len(*patch.Members))
		byID := make(map[string]domain.ProviderChannelMember, len(channel.Members))
		byLabel := make(map[string]domain.ProviderChannelMember, len(channel.Members))
		for _, member := range channel.Members {
			byID[member.ID] = member
			byLabel[strings.ToLower(member.Label)] = member
		}
		// keys is positional, aligned with members below. Labels are unique
		// per channel (UNIQUE(channel_id, label), migration 0013), so the
		// slice cannot be keyed by label: the input list is positional, and
		// an id-less input is matched to an existing member by label. The
		// delete(byLabel, ...) calls consume that member so each existing
		// member is matched at most once — a label-keyed map would let one
		// input silently rebind to a member another input already claimed.
		keys := make([]string, 0, len(*patch.Members))
		for _, input := range *patch.Members {
			member := byID[input.ID]
			if member.ID != "" {
				// The member bound by id is consumed: a later input with the
				// same label and no id must not be able to rebind to it, or a
				// second same-label key would clobber this member's key
				// instead of becoming a new member.
				delete(byLabel, strings.ToLower(member.Label))
			} else {
				labelKey := strings.ToLower(strings.TrimSpace(input.Label))
				member = byLabel[labelKey]
				if member.ID != "" {
					// Consume each matched existing member at most once. Two
					// same-label inputs with no ids both used to resolve to
					// the same existing member, so the second silently
					// overwrote the first: one member row vanished and its
					// key was gone. The second input now falls through to
					// new-member creation with its own secret instead.
					delete(byLabel, labelKey)
				}
			}
			if member.ID == "" {
				member.ID = input.ID
				member.Label = strings.TrimSpace(input.Label)
				member.ChannelID = channel.ID
				member.Enabled = true
				member.Weight = 1
				member.MaxInflight = 1
			}
			if strings.TrimSpace(input.Label) != "" {
				member.Label = strings.TrimSpace(input.Label)
			}
			if input.Enabled != nil {
				member.Enabled = *input.Enabled
			}
			if input.Weight != nil {
				member.Weight = *input.Weight
			}
			if input.MaxInflight != nil {
				member.MaxInflight = *input.MaxInflight
			}
			members = append(members, member)
			keys = append(keys, strings.TrimSpace(input.APIKey))
		}
		// A patch whose resulting list repeats a label must be rejected
		// before the save, or the database would surface a bare constraint
		// error that names no label, and a reject must not touch the secret
		// store: this runs before SaveProviderChannel writes anything.
		// validateDistinctProviderChannelMemberLabels is the same check
		// SaveProviderChannel runs on create, so the two paths cannot drift.
		if err := validateDistinctProviderChannelMemberLabels(members); err != nil {
			return domain.ProviderChannel{}, err
		}
		// Capture the pre-patch SecretRefs before the list is replaced: a
		// member absent from the patch is dropped, and its ref must not
		// linger as an orphaned encrypted file. Deleting a secret is not
		// reversible, so the deletion happens only after the save succeeds
		// (a failed save must leave every ref resolvable) and only for refs
		// no surviving member still references.
		oldSecretRefs := make(map[string]struct{}, len(channel.Members))
		for _, member := range channel.Members {
			if ref := strings.TrimSpace(member.SecretRef); ref != "" {
				oldSecretRefs[ref] = struct{}{}
			}
		}
		channel.Members = members
		saved, err := s.SaveProviderChannel(ctx, channel, keys)
		if err != nil {
			return domain.ProviderChannel{}, err
		}
		s.deleteOrphanedMemberSecrets(saved.ID, oldSecretRefs, saved.Members)
		return saved, nil
	}
	return s.SaveProviderChannel(ctx, channel, nil)
}

// deleteOrphanedMemberSecrets removes secret-store entries whose SecretRef no
// longer appears on any member of a just-saved channel. The check runs against
// the saved list rather than the patch input because refs exist only on saved
// members: ProviderChannelMemberUpdate carries an APIKey, never a SecretRef, so
// the patch cannot say which key a retained member still points at. Comparing
// against it would prune the refs of members it simply did not mention. The
// set is keyed by ref rather than by member so that a ref two members somehow
// shared would survive — the schema forbids that (UNIQUE(secret_ref), migration
// 0013) and SaveProviderChannel mints one ref per member, so it is defence
// against a shape that cannot currently occur, not a case being handled. It
// runs strictly after a successful save: on a
// failed save the channel still references every ref and the keys must remain.
// A failed delete is logged and ignored because the channel is already saved
// correctly at that point; letting it fail the update would turn a hygiene step
// into data loss.
func (s *Service) deleteOrphanedMemberSecrets(channelID string, oldRefs map[string]struct{}, members []domain.ProviderChannelMember) {
	stillReferenced := make(map[string]struct{}, len(members))
	for _, member := range members {
		if ref := strings.TrimSpace(member.SecretRef); ref != "" {
			stillReferenced[ref] = struct{}{}
		}
	}
	for ref := range oldRefs {
		if _, kept := stillReferenced[ref]; kept {
			continue
		}
		if err := s.secrets.Delete(ref); err != nil {
			slog.Warn("provider channel: failed to delete orphaned member secret", "channel_id", channelID, "error", err)
		}
	}
}

func (s *Service) SetProviderChannelEnabled(ctx context.Context, id string, enabled bool) (domain.ProviderChannel, error) {
	value := enabled
	return s.UpdateProviderChannel(ctx, id, ProviderChannelUpdate{Enabled: &value})
}

// DeleteProviderChannel is a soft delete that persists the tombstone in
// SQLite via the deleted_at column so the channel stays hidden across Hub
// restarts. Hub secrets are removed synchronously. The UNIQUE(capability,label)
// constraint on provider_channels prevents reuse of the same label until a
// future hard-delete migration removes the row.
func (s *Service) DeleteProviderChannel(ctx context.Context, id string) error {
	channel, err := s.providerChannelByID(ctx, id)
	if err != nil {
		return err
	}
	for _, member := range channel.Members {
		if strings.TrimSpace(member.SecretRef) != "" {
			if err := s.secrets.Delete(member.SecretRef); err != nil {
				return err
			}
		}
	}
	return s.repo.SoftDeleteProviderChannel(ctx, channel.ID)
}

func (s *Service) providerChannelByID(ctx context.Context, id string) (domain.ProviderChannel, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return domain.ProviderChannel{}, errors.New("provider channel id is required")
	}
	channels, err := s.repo.ListProviderChannels(ctx, "")
	if err != nil {
		return domain.ProviderChannel{}, err
	}
	for _, channel := range channels {
		if channel.ID == id {
			return channel, nil
		}
	}
	return domain.ProviderChannel{}, fmt.Errorf("provider channel not found")
}

// providerChannelTestBudget bounds the whole endpoint test. The reachability
// GET and the capability probe share one deadline, so a slow first hop can
// never stretch the total past the operator-facing promise of one click.
const providerChannelTestBudget = 8 * time.Second

// channelProbeableModels reports whether the channel's provider speaks an
// OpenAI-compatible wire protocol with a cheap, non-billed GET /models
// listing. The Gemini family and Volcengine ASR do not: Gemini's key rides in
// a query parameter and its model list lives under a different shape, and ASR
// endpoints expose no model list at all. For those the test keeps the
// reachability verdict and says explicitly that no billed call was made.
func channelProbeableModels(channel domain.ProviderChannel) bool {
	switch strings.TrimSpace(channel.ProviderName) {
	case "gemini", "gemini_embed_content", "volcengine_asr":
		return false
	}
	return true
}

func providerChannelModelsURL(endpoint string) string {
	return strings.TrimRight(strings.TrimSpace(endpoint), "/") + "/models"
}

// maxProbeModelNameBytes bounds the model id copied into the result. A model
// listing is upstream-controlled text and the result reaches the browser
// page, so it gets the same truncation discipline common.ReadError applies to
// error bodies.
const maxProbeModelNameBytes = 128

// probeProviderModels is the non-billed capability probe for OpenAI-
// compatible channels: GET {endpoint}/models with the channel key. It never
// logs or returns the key, and it never copies an upstream body into the
// message — a relay that echoes the request back must not be able to persist
// a key through the result. 404 is a schema mismatch (the endpoint is
// reachable, it just exposes no model list), not a failure; 401/403 name the
// key as the problem.
func probeProviderModels(ctx context.Context, client *http.Client, endpoint, key string) (modelResponded bool, modelName string, schemaOK bool, message string, httpStatus int, latencyMS int64) {
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, providerChannelModelsURL(endpoint), nil)
	if err != nil {
		return false, "", false, "Endpoint 可达；模型探测请求无效", 0, time.Since(started).Milliseconds()
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := client.Do(request)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return false, "", false, "Endpoint 可达；模型探测失败", 0, latency
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, "", false, fmt.Sprintf("Endpoint 可达；API Key 无效或已被拒绝 (HTTP %d)", response.StatusCode), response.StatusCode, latency
	case http.StatusNotFound:
		return false, "", false, "Endpoint 可达；未发现模型列表端点（schema 未知），未执行计费模型调用", response.StatusCode, latency
	}
	if response.StatusCode != http.StatusOK {
		return false, "", false, fmt.Sprintf("Endpoint 可达；模型列表请求失败 (HTTP %d)", response.StatusCode), response.StatusCode, latency
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&listing); err != nil {
		return false, "", false, "Endpoint 可达；模型列表返回无法解析（schema 未知）", response.StatusCode, latency
	}
	for _, m := range listing.Data {
		name := redactString(strings.TrimSpace(m.ID), key)
		if name == "" {
			continue
		}
		return true, clipProbeText(name, maxProbeModelNameBytes), true, "模型已响应", response.StatusCode, latency
	}
	return false, "", true, "Endpoint 可达；模型列表为空", response.StatusCode, latency
}

// clipProbeText truncates s to at most n bytes without splitting a UTF-8
// rune, marking the cut like the API envelope's clipText. Model ids are
// normally short ASCII, but the bound exists for the endpoint that is not.
func clipProbeText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func (s *Service) TestProviderChannel(ctx context.Context, id string) (ProviderChannelTestResult, error) {
	channel, err := s.providerChannelByID(ctx, id)
	if err != nil {
		return ProviderChannelTestResult{}, err
	}
	result := ProviderChannelTestResult{ChannelID: channel.ID, ProviderName: channel.ProviderName, Status: "not_ready"}
	secretRef := ""
	for _, member := range channel.Members {
		if member.Enabled && member.SecretRef != "" {
			ready, resolveErr := s.secrets.Has(ctx, member.SecretRef)
			if resolveErr != nil {
				return ProviderChannelTestResult{}, errors.New("provider channel secret store unavailable")
			}
			if ready {
				result.SecretReady = true
				secretRef = member.SecretRef
				break
			}
		}
	}
	if !result.SecretReady {
		result.Message = "没有可用的 Provider Key"
		return result, nil
	}
	key, keyOK, resolveErr := s.secrets.Resolve(secretRef)
	if resolveErr != nil {
		return ProviderChannelTestResult{}, errors.New("provider channel secret store unavailable")
	}
	if !keyOK {
		result.Message = "没有可用的 Provider Key"
		return result, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(channel.Endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		result.Status = "invalid_endpoint"
		result.Message = "Provider Endpoint 无效"
		return result, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, providerChannelTestBudget)
	defer cancel()
	client := &http.Client{Timeout: providerChannelTestBudget}
	started := time.Now()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		result.Status = "invalid_endpoint"
		result.Message = "Provider Endpoint 无效"
		return result, nil
	}
	response, err := client.Do(request)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Status = "unreachable"
		result.Message = "Provider Endpoint 不可达"
		return result, nil
	}
	response.Body.Close()
	result.Reachable = true
	result.HTTPStatus = response.StatusCode
	result.Status = "reachable"
	if !channelProbeableModels(channel) {
		result.Message = "Endpoint 可达；未执行计费模型调用"
		return result, nil
	}
	modelResponded, modelName, schemaOK, message, probeStatus, probeLatency := probeProviderModels(probeCtx, client, channel.Endpoint, key)
	result.ModelResponded, result.ModelName, result.SchemaOK, result.Message = modelResponded, modelName, schemaOK, message
	// A transport failure inside the model probe returns HTTP 0 — it never
	// reached the endpoint — so it must not erase the reachability probe's
	// real status above: the page would show "可达" with HTTP 0. Only a
	// completed model-probe request carries a status worth reporting.
	if probeStatus != 0 {
		result.HTTPStatus = probeStatus
		result.LatencyMS = probeLatency
	}
	return result, nil
}

type ProviderProxyResult struct {
	Provider   string
	StatusCode int
	Body       []byte
}

var ErrProviderProxyRequest = errors.New("provider proxy request failed")

const maxProviderProxyBytes int64 = 2 << 20

// MaxProviderProxyBodyBytes is the shared API guardrail for JSON-only worker
// proxy requests. It is exported so the HTTP layer cannot drift from the
// Service contract.
func MaxProviderProxyBodyBytes() int64 { return maxProviderProxyBytes }

// ProxyWorkerProviderJSON verifies the active job lease, resolves the server
// side provider credential, and forwards one bounded JSON request. It never
// returns the credential and redacts it from the bounded JSON response.
func (s *Service) ProxyWorkerProviderJSON(ctx context.Context, worker remote.Worker, jobID string, operation credentials.Operation, body []byte) (ProviderProxyResult, error) {
	lease, err := s.issueProviderCredentialForProxy(ctx, worker, jobID, operation)
	if err != nil {
		return ProviderProxyResult{}, err
	}
	if int64(len(body)) > maxProviderProxyBytes || !jsonBody(body) {
		return ProviderProxyResult{}, errors.New("provider proxy accepts only valid JSON up to 2 MiB")
	}
	endpoint, err := providerEndpoint(lease.Credential.BaseURL, lease.Credential.Path)
	if err != nil {
		return ProviderProxyResult{}, ErrProviderProxyRequest
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ProviderProxyResult{}, ErrProviderProxyRequest
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	header := strings.TrimSpace(lease.Credential.AuthHeader)
	if header == "" {
		header = "Authorization"
	}
	// An empty scheme means ordinary bearer, which is what
	// common.Endpoint.NewRequest already assumes for the in-process request
	// path. This proxy used to disagree: an empty scheme matched neither
	// branch below and the key went out unprefixed, so every provider whose
	// config omits auth_scheme — the documented house style for plain bearer,
	// used by the StepFun, Qwen, QwenVideo, VolcVideo, LocalVLM, TagCurator,
	// Embedding and Repurpose defaults — authenticated in-process and 401'd
	// through a Worker. Providers that genuinely want the bare key say so
	// explicitly with "raw" (Gemini does), so defaulting here takes nothing
	// away from them.
	scheme := strings.TrimSpace(lease.Credential.AuthScheme)
	if scheme == "" {
		scheme = "Bearer"
	}
	value := lease.Credential.APIKey
	if strings.EqualFold(scheme, "bearer") {
		// Normalised rather than passed through so a config saying "bearer"
		// still sends the canonical capitalisation.
		value = "Bearer " + value
	} else if !strings.EqualFold(scheme, "raw") {
		value = scheme + " " + value
	}
	request.Header.Set(header, value)
	for key, extra := range lease.Credential.ExtraHeaders {
		if strings.TrimSpace(key) != "" {
			request.Header.Set(key, extra)
		}
	}
	timeout := time.Duration(lease.Credential.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return ProviderProxyResult{}, ErrProviderProxyRequest
	}
	defer response.Body.Close()
	contentType := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Type")))
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return ProviderProxyResult{}, ErrProviderProxyRequest
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxProviderProxyBytes+1))
	if err != nil || int64(len(responseBody)) > maxProviderProxyBytes || !jsonBody(responseBody) {
		return ProviderProxyResult{}, ErrProviderProxyRequest
	}
	responseBody = bytes.ReplaceAll(responseBody, []byte(lease.Credential.APIKey), []byte("[REDACTED]"))
	return ProviderProxyResult{Provider: lease.Provider, StatusCode: response.StatusCode, Body: responseBody}, nil
}

func (s *Service) issueProviderCredentialForProxy(ctx context.Context, worker remote.Worker, jobID string, operation credentials.Operation) (credentials.Lease, error) {
	if !workerSupportsOperation(worker.Capabilities, operation) {
		return credentials.Lease{}, errors.New("worker does not declare provider operation")
	}
	lease, err := credentials.NewBroker(s.cfg.Providers, nil).Issue(jobID, worker.ID, operation, 5*time.Minute)
	if err != nil {
		return credentials.Lease{}, s.classifyWorkerProviderBrokerErr(ctx, operation, err)
	}
	expiresAt, err := time.Parse(time.RFC3339, lease.ExpiresAt)
	if err != nil {
		return credentials.Lease{}, errors.New("provider proxy lease invalid")
	}
	if err := s.repo.RecordProviderCredentialLease(ctx, jobID, worker.ID, lease.Provider, string(operation), expiresAt); err != nil {
		return credentials.Lease{}, err
	}
	return lease, nil
}

// classifyWorkerProviderBrokerErr turns a credentials.Broker resolution
// failure into one of two sentinels an operator can act on, instead of the
// single flat refusal both Worker provider-access paths used to collapse
// every failure into (see ErrWorkerProviderConfiguredAsChannelOnly's doc
// comment for why the broker is legacy-config-only on purpose, and why that
// makes this distinction worth making). It only asks whether a provider
// channel row exists for the operation's capability -- never anything about
// the channel's members, secrets or health -- so this stays a yes/no read
// through the same ListProviderChannels the /providers admin page already
// uses, not a second route into channel internals.
//
// brokerErr is folded into the %w-wrapped ErrWorkerProviderNotConfigured
// case rather than discarded: credentials.Broker's own error text is always
// just a provider name and operation (see its resolve/Issue implementations),
// never a key, endpoint or anything from secretstore, so carrying it forward
// loses no safety and keeps the detail the flat string used to throw away.
func (s *Service) classifyWorkerProviderBrokerErr(ctx context.Context, operation credentials.Operation, brokerErr error) error {
	if s.workerProviderChannelConfigured(ctx, operation) {
		return fmt.Errorf("%w: %s", ErrWorkerProviderConfiguredAsChannelOnly, operation)
	}
	return fmt.Errorf("%w: %s", ErrWorkerProviderNotConfigured, brokerErr)
}

// workerProviderChannelConfigured reports whether any (enabled or not)
// provider channel row exists for the operation's capability. A disabled or
// still-being-set-up channel still proves the operator used the channels UI
// for this capability rather than providers.* config, which is exactly the
// fact classifyWorkerProviderBrokerErr needs -- it is not asking whether the
// channel would currently serve a request, only where the operator put their
// configuration. A repository error here is treated as "no channel found"
// rather than propagated: this call exists only to sharpen an already-failed
// broker resolution's error message, and must never turn a message-quality
// improvement into a new way for that resolution to fail.
func (s *Service) workerProviderChannelConfigured(ctx context.Context, operation credentials.Operation) bool {
	channels, err := s.repo.ListProviderChannels(ctx, string(operation))
	if err != nil {
		return false
	}
	return len(channels) > 0
}

func providerEndpoint(base, path string) (string, error) {
	base = strings.TrimSpace(base)
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", ErrProviderProxyRequest
	}
	if strings.TrimSpace(path) != "" {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	}
	return parsed.String(), nil
}

func jsonBody(body []byte) bool {
	return len(body) > 0 && json.Valid(body)
}
