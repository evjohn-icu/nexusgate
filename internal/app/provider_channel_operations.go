package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ev/timingdex/internal/credentials"
	"github.com/ev/timingdex/internal/domain"
	"github.com/ev/timingdex/internal/remote"
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
	Label        *string                        `json:"label,omitempty"`
	ProviderName *string                        `json:"provider_name,omitempty"`
	Protocol     *string                        `json:"protocol,omitempty"`
	Endpoint     *string                        `json:"endpoint,omitempty"`
	Model        *string                        `json:"model,omitempty"`
	Enabled      *bool                          `json:"enabled,omitempty"`
	RouteOrder   *int                           `json:"route_order,omitempty"`
	Members      *[]ProviderChannelMemberUpdate `json:"members,omitempty"`
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
	if patch.Members != nil {
		members := make([]domain.ProviderChannelMember, 0, len(*patch.Members))
		byID := make(map[string]domain.ProviderChannelMember, len(channel.Members))
		byLabel := make(map[string]domain.ProviderChannelMember, len(channel.Members))
		for _, member := range channel.Members {
			byID[member.ID] = member
			byLabel[strings.ToLower(member.Label)] = member
		}
		// keys is positional, aligned with members below: a channel allows
		// several members sharing the same label (Weight/MaxInflight exist
		// so one provider can be configured with multiple keys), so a
		// label-keyed map here would let one input silently clobber or
		// misassign another member's key.
		keys := make([]string, 0, len(*patch.Members))
		for _, input := range *patch.Members {
			member := byID[input.ID]
			if member.ID == "" {
				member = byLabel[strings.ToLower(strings.TrimSpace(input.Label))]
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
		channel.Members = members
		return s.SaveProviderChannel(ctx, channel, keys)
	}
	return s.SaveProviderChannel(ctx, channel, nil)
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

func (s *Service) TestProviderChannel(ctx context.Context, id string) (ProviderChannelTestResult, error) {
	channel, err := s.providerChannelByID(ctx, id)
	if err != nil {
		return ProviderChannelTestResult{}, err
	}
	result := ProviderChannelTestResult{ChannelID: channel.ID, ProviderName: channel.ProviderName, Status: "not_ready"}
	for _, member := range channel.Members {
		if member.Enabled && member.SecretRef != "" {
			ready, resolveErr := s.secrets.Has(member.SecretRef)
			if resolveErr != nil {
				return ProviderChannelTestResult{}, errors.New("provider channel secret store unavailable")
			}
			if ready {
				result.SecretReady = true
				break
			}
		}
	}
	if !result.SecretReady {
		result.Message = "没有可用的 Provider Key"
		return result, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(channel.Endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		result.Status = "invalid_endpoint"
		result.Message = "Provider Endpoint 无效"
		return result, nil
	}
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		result.Status = "invalid_endpoint"
		result.Message = "Provider Endpoint 无效"
		return result, nil
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Status = "unreachable"
		result.Message = "Provider Endpoint 不可达"
		return result, nil
	}
	defer response.Body.Close()
	result.Reachable = true
	result.HTTPStatus = response.StatusCode
	result.Status = "reachable"
	result.Message = "Endpoint 可达；未执行计费模型调用"
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
	value := lease.Credential.APIKey
	if strings.EqualFold(strings.TrimSpace(lease.Credential.AuthScheme), "bearer") {
		value = "Bearer " + value
	} else if scheme := strings.TrimSpace(lease.Credential.AuthScheme); scheme != "" && !strings.EqualFold(scheme, "raw") {
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
		return credentials.Lease{}, errors.New("provider proxy is not configured")
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
