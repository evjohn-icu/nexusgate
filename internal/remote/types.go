// Package remote contains the Hub <-> Worker contract.  It deliberately has
// no repository or HTTP dependency so a Worker binary never opens the Hub DB.
package remote

import (
	"time"

	"github.com/ev/timingdex/internal/domain"
)

// WorkerJob contains only a portable source locator. Hub absolute paths never
// leave the NAS; Workers resolve RootID using their own mounted library map.
type WorkerJob struct {
	JobID             string         `json:"job_id"`
	AssetID           string         `json:"asset_id"`
	RootID            string         `json:"root_id"`
	RelativePath      string         `json:"relative_path"`
	ModifiedNS        int64          `json:"modified_ns"`
	JobType           domain.JobType `json:"job_type"`
	AttemptCount      int            `json:"attempt_count"`
	MaxAttempts       int            `json:"max_attempts"`
	CurrentStage      string         `json:"current_stage,omitempty"`
	Progress          float64        `json:"progress"`
	PreferredWorkerID string         `json:"preferred_worker_id,omitempty"`
	AssignedWorkerID  string         `json:"assigned_worker_id,omitempty"`
	// SourceBytes is the Hub's recorded size of the source. It travels with the
	// lease so the Worker does not have to stat a NAS path to learn it.
	SourceBytes int64 `json:"source_bytes,omitempty"`
	// ReadRate caps FFmpeg's input read speed as a multiple of realtime; zero
	// means unlimited. The Hub decides it, because the throttle is an operator
	// setting and a Worker must not be able to opt itself out of protecting the
	// shared disk it is reading from.
	ReadRate float64 `json:"read_rate,omitempty"`
}

// WorkerAssignmentMode controls the safety semantics of a manual Worker
// selection. Preferred is a soft hint; Required is a hard lease constraint.
type WorkerAssignmentMode string

const (
	WorkerAssignmentAny       WorkerAssignmentMode = "any"
	WorkerAssignmentPreferred WorkerAssignmentMode = "preferred"
	WorkerAssignmentRequired  WorkerAssignmentMode = "required"
)

type WorkerJobEvent struct {
	ID        string    `json:"id"`
	JobID     string    `json:"job_id"`
	WorkerID  string    `json:"worker_id,omitempty"`
	EventType string    `json:"event_type"`
	Stage     string    `json:"stage,omitempty"`
	Progress  float64   `json:"progress"`
	ErrorCode string    `json:"error_code,omitempty"`
	Retryable bool      `json:"retryable,omitempty"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type WorkerJobStatus struct {
	JobID               string           `json:"job_id"`
	AssetID             string           `json:"asset_id"`
	JobType             domain.JobType   `json:"job_type"`
	State               domain.JobState  `json:"state"`
	Priority            int              `json:"priority"`
	AttemptCount        int              `json:"attempt_count"`
	MaxAttempts         int              `json:"max_attempts"`
	RunAfter            time.Time        `json:"run_after"`
	LeaseOwner          string           `json:"lease_owner,omitempty"`
	LeaseExpiresAt      time.Time        `json:"lease_expires_at,omitempty"`
	CurrentStage        string           `json:"current_stage,omitempty"`
	Progress            float64          `json:"progress"`
	LastErrorCode       string           `json:"last_error_code,omitempty"`
	LastErrorMessage    string           `json:"last_error_message,omitempty"`
	LastFailureAt       time.Time        `json:"last_failure_at,omitempty"`
	LastFailureWorkerID string           `json:"last_failure_worker_id,omitempty"`
	LastFailureStage    string           `json:"last_failure_stage,omitempty"`
	PreferredWorkerID   string           `json:"preferred_worker_id,omitempty"`
	AssignedWorkerID    string           `json:"assigned_worker_id,omitempty"`
	Events              []WorkerJobEvent `json:"events,omitempty"`
}

type WorkerStatus string

const (
	WorkerOnline  WorkerStatus = "online"
	WorkerOffline WorkerStatus = "offline"
	WorkerRevoked WorkerStatus = "revoked"
)

type WorkerCapabilities struct {
	Proxy                bool     `json:"proxy"`
	Thumbnail            bool     `json:"thumbnail"`
	AudioExtract         bool     `json:"audio_extract"`
	MaxParallelProxyJobs int      `json:"max_parallel_proxy_jobs"`
	MaxProxyHeight       int      `json:"max_proxy_height"`
	SpeedClass           string   `json:"speed_class"`
	Hardware             []string `json:"hardware,omitempty"`
	LibraryRoots         []string `json:"library_roots,omitempty"`
	ProviderOperations   []string `json:"provider_operations,omitempty"`
}

type WorkerRegistration struct {
	Name         string             `json:"name"`
	Platform     string             `json:"platform"`
	Version      string             `json:"version,omitempty"`
	Capabilities WorkerCapabilities `json:"capabilities"`
}

type Worker struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Platform     string             `json:"platform"`
	Version      string             `json:"version,omitempty"`
	Status       WorkerStatus       `json:"status"`
	Capabilities WorkerCapabilities `json:"capabilities"`
	LastSeenAt   time.Time          `json:"last_seen_at"`
	CreatedAt    time.Time          `json:"created_at"`
}

type PairingToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}
