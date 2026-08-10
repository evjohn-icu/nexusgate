package domain

import "time"

type JobType string

const (
	JobProbe      JobType = "probe"
	JobDerive     JobType = "derive"
	JobSpeechGate JobType = "speech_gate"
	JobTranscribe JobType = "transcribe"
	JobAlign      JobType = "align"
	JobAnalyze    JobType = "analyze"
	JobNormalize  JobType = "normalize"
	JobIndex      JobType = "index"
)

type JobState string

const (
	JobPending   JobState = "pending"
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
	JobSkipped   JobState = "skipped"
)

type Job struct {
	ID           string    `json:"id"`
	AssetID      string    `json:"asset_id"`
	Type         JobType   `json:"job_type"`
	State        JobState  `json:"state"`
	Priority     int       `json:"priority"`
	AttemptCount int       `json:"attempt_count"`
	MaxAttempts  int       `json:"max_attempts"`
	RunAfter     time.Time `json:"run_after"`
	InputHash    string    `json:"input_hash"`
	LastError    string    `json:"last_error,omitempty"`
	// Terminal marks a failure the pipeline classified as permanent. Such a job
	// is never leased again regardless of how many attempts it has left, so it
	// is reported separately from a job that simply ran out of attempts.
	Terminal bool `json:"terminal"`
	// Filename is the owning asset's name (primary location, basename), for
	// operator surfaces like the progress console's "正在处理" line.
	Filename string `json:"filename,omitempty"`
	// DeferredReason is set while a job is parked on wall-clock time for a
	// reason that is not the job's fault, so a queue that is deliberately quiet
	// is not mistaken for a stuck one. RunAfter carries when it resumes. It is
	// empty for an ordinary retry backoff, which is seconds rather than hours.
	DeferredReason string `json:"deferred_reason,omitempty"`
}

// JobSummary counts the whole queue. /progress used to derive its figures by
// tallying the hundred rows it had already fetched for the table, which is a
// sample, not a count — and a biased one, since those rows are the newest by
// creation and the chain creates each job as the previous one finishes. During
// a large scan that sample is all freshly enqueued work, so the page reported a
// library with thousands of finished jobs as "0 done, 100 pending" and stayed
// there. The categories are disjoint and already have the page's meaning baked
// in, so no arithmetic happens in the browser.
type JobSummary struct {
	// Pending excludes Deferred: a job parked on an empty provider account is
	// not waiting for a turn, and counting it as backlog reads as a stall.
	Pending   int `json:"pending"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	// Failed counts failures that will be retried; Terminal counts the ones the
	// pipeline classified as permanent and will never lease again.
	Failed   int `json:"failed"`
	Terminal int `json:"terminal"`
	Deferred int `json:"deferred"`
	// Total is every row in the queue. The six categories above are disjoint and
	// currently exhaust it — JobSkipped is declared but never written — so a
	// consumer that finds them not summing to Total has found a state nothing
	// here accounts for, which is worth noticing rather than rounding away.
	Total int `json:"total"`
}

// JobFailureCategory is the stable machine-readable identity of why a job
// failed, for the issues view to aggregate on. It is decided once, at failure
// time, by the sentinel the error chain carries — never by reading the message
// text, which is prose for operators and can be reworded without changing what
// happened (the same reasoning domain.ErrPermanentFailure documents). The
// value is a Hub-assigned constant, JSON-safe, and durable in
// jobs.last_error_code — the single column the issues view reads, so a job's
// category is the code written at the moment it stopped or parked.
//
// The registry is deliberately extensible: categories with no producer yet
// (media_decode, unsupported_media, source_missing, worker_offline,
// configuration) exist so the issues view can render a complete palette from
// day one, and the pipeline maps a sentinel to them the day one exists. An
// error whose chain carries no known sentinel is Unknown — never a guess from
// its text.
type JobFailureCategory string

const (
	// JobFailureCategoryProviderQuota answers that the provider is
	// rate-limiting (HTTP 408/429): the call was fine, the account is
	// momentarily capped, and the job clears on its own.
	JobFailureCategoryProviderQuota JobFailureCategory = "provider_quota"
	// JobFailureCategoryProviderAuth answers that the credential is the
	// problem (HTTP 401/402/403): the next attempt would present the same
	// dead key to the same refusal, so an operator has to act.
	JobFailureCategoryProviderAuth JobFailureCategory = "provider_auth"
	// JobFailureCategoryProviderUnavailable answers that the provider itself
	// was not reachable or not well (HTTP 5xx, network failure, timeout):
	// transient, and the job clears on its own.
	JobFailureCategoryProviderUnavailable JobFailureCategory = "provider_unavailable"
	// JobFailureCategoryProviderRouteExhausted answers that every provider
	// key on the capability's route failed at once; see
	// JobDeferProviderRouteExhausted.
	JobFailureCategoryProviderRouteExhausted JobFailureCategory = "provider_route_exhausted"
	// JobFailureCategoryMediaDecode answers that the media could not be
	// decoded (an ffprobe/ffmpeg decode failure). No sentinel produces it
	// yet; the identity is reserved for the day one exists.
	JobFailureCategoryMediaDecode JobFailureCategory = "media_decode"
	// JobFailureCategoryUnsupportedMedia answers that the container or codec
	// cannot be decoded, or the asset has no audio where the chain needs one
	// — the permanent-ish class. No sentinel produces it yet.
	JobFailureCategoryUnsupportedMedia JobFailureCategory = "unsupported_media"
	// JobFailureCategoryDiskSpaceLow answers that the cache volume is full;
	// see JobDeferDiskSpaceLow.
	JobFailureCategoryDiskSpaceLow JobFailureCategory = "disk_space_low"
	// JobFailureCategoryBudgetExhausted answers that the configured daily or
	// monthly provider budget is spent for the period; see
	// JobDeferBudgetExhausted.
	JobFailureCategoryBudgetExhausted JobFailureCategory = "budget_exhausted"
	// JobFailureCategorySourceMissing answers that the asset's primary
	// location is gone. No sentinel produces it yet; the identity is reserved
	// for the day the asset-state check reports one.
	JobFailureCategorySourceMissing JobFailureCategory = "source_missing"
	// JobFailureCategoryWorkerOffline answers that the paired Worker the job
	// needed was not reachable. No sentinel produces it yet.
	JobFailureCategoryWorkerOffline JobFailureCategory = "worker_offline"
	// JobFailureCategoryConfiguration answers that the deployment is
	// misconfigured and only an operator can fix it. No sentinel produces it
	// yet.
	JobFailureCategoryConfiguration JobFailureCategory = "configuration"
	// JobFailureCategoryUnknown is the fallback for an error whose chain
	// carries no sentinel the classifier knows. It is not "unimportant": it
	// means the registry has not seen this failure yet, which the issues view
	// should say rather than paper over.
	JobFailureCategoryUnknown JobFailureCategory = "unknown"
)

// IsRetryable reports whether the category describes a failure that heals on
// its own — the account is uncapped, the provider comes back, the disk frees
// — so the job is worth keeping in the queue. It is false where the next
// attempt would present identical inputs to a deterministic decision (auth,
// decode, unsupported media, configuration) or where only an operator can see
// what is wrong (unknown). The verdict lives here rather than at the call site
// so the pipeline and the issues view cannot disagree about which failures
// self-recover.
func (c JobFailureCategory) IsRetryable() bool {
	switch c {
	case JobFailureCategoryProviderQuota,
		JobFailureCategoryProviderUnavailable,
		JobFailureCategoryProviderRouteExhausted,
		JobFailureCategoryDiskSpaceLow,
		JobFailureCategoryBudgetExhausted,
		JobFailureCategorySourceMissing,
		JobFailureCategoryWorkerOffline:
		return true
	}
	return false
}

// JobDeferProviderRouteExhausted is the DeferReason recorded when every
// provider key on a capability's route is failing at once. The recommended
// deployment runs on a plan with a hard monthly quota, so an exhausted account
// answers 429 on every key together; that is a fact about the account, not
// about the job, and the job's attempt budget must survive it.
//
// The value is a Hub-assigned constant rather than upstream text on purpose:
// /progress renders it to any viewer, while last_error_message — which can
// embed a truncated provider response body — stays behind the admin token.
//
// It is spelled from the category constant rather than as a literal so the
// defer reason and the issues-view category can never drift apart: DeferJob
// writes the reason into jobs.last_error_code, which is the same column the
// issues view aggregates categories from. The conversion back to an untyped
// string constant keeps existing call sites (string parameters, SQL binding)
// unchanged.
const JobDeferProviderRouteExhausted = string(JobFailureCategoryProviderRouteExhausted)

// JobDeferDiskSpaceLow is the DeferReason recorded when a job is parked on a
// full disk: an ENOSPC failure surfaced mid-job, or the pre-lease guard finding
// the cache volume below minimum_free_space_bytes. The failure is about the
// disk, not the job — the next attempt would write to the same full volume and
// fail the same way — so it is deferred on wall-clock time instead of retried,
// and the attempt this lease spent is handed back. The job auto-resumes once
// the operator frees space or run_after passes.
//
// Like JobDeferProviderRouteExhausted, it is the disk-space category constant
// spelled as an untyped string, for the same reason: the reason and the
// category must be one spelling.
const JobDeferDiskSpaceLow = string(JobFailureCategoryDiskSpaceLow)

// JobDeferBudgetExhausted is retained for persisted legacy records; advisory
// cost guides no longer produce this deferral reason.
const JobDeferBudgetExhausted = string(JobFailureCategoryBudgetExhausted)

type MediaMetadata struct {
	DurationMS         int64      `json:"duration_ms"`
	Width              int        `json:"width"`
	Height             int        `json:"height"`
	FPS                float64    `json:"fps"`
	VideoCodec         string     `json:"video_codec"`
	AudioCodec         string     `json:"audio_codec"`
	HasAudio           bool       `json:"has_audio"`
	Orientation        string     `json:"orientation"`
	CapturedAt         *time.Time `json:"captured_at,omitempty"`
	CaptureVendor      string     `json:"capture_vendor,omitempty"`
	CameraMake         string     `json:"camera_make,omitempty"`
	CameraModel        string     `json:"camera_model,omitempty"`
	CameraSerial       string     `json:"camera_serial,omitempty"`
	LensModel          string     `json:"lens_model,omitempty"`
	Reel               string     `json:"reel,omitempty"`
	Clip               string     `json:"clip,omitempty"`
	SourceTimecode     string     `json:"source_timecode,omitempty"`
	Latitude           *float64   `json:"latitude,omitempty"`
	Longitude          *float64   `json:"longitude,omitempty"`
	PixelFormat        string     `json:"pixel_format,omitempty"`
	BitDepth           int        `json:"bit_depth,omitempty"`
	ColorSpace         string     `json:"color_space,omitempty"`
	ColorTransfer      string     `json:"color_transfer,omitempty"`
	ColorPrimaries     string     `json:"color_primaries,omitempty"`
	SourceColor        string     `json:"source_color,omitempty"`
	ColorProfile       string     `json:"color_profile,omitempty"`
	RawFormat          string     `json:"raw_format,omitempty"`
	PreviewStatus      string     `json:"preview_status,omitempty"`
	PreviewRenderMode  string     `json:"preview_render_mode,omitempty"`
	PreviewAvailable   bool       `json:"preview_available"`
	PreviewRequiresLUT bool       `json:"preview_requires_lut,omitempty"`
	FFProbeRaw         string     `json:"ffprobe_raw"`
	ExifToolRaw        string     `json:"exiftool_raw"`
}

type DerivedArtifact struct {
	ID, AssetID, Type, ProfileHash, LocalPath string
	SizeBytes                                 int64
}

type SpeechClassification struct {
	Classification    string  `json:"classification"`
	SpeechProbability float64 `json:"speech_probability"`
	Reason            string  `json:"reason"`
	RawJSON           string  `json:"raw_json"`
}

type TranscriptSegment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}
type Transcript struct {
	Language    string              `json:"language"`
	Text        string              `json:"text"`
	Segments    []TranscriptSegment `json:"segments"`
	RawResponse string              `json:"-"`
}

// Timed reports whether the transcript's segments carry trustworthy
// timestamps. Some ASR providers (Qwen, Volcengine) return a single
// placeholder segment with StartMS=0 and EndMS=0 alongside the full text:
// those are not timestamps, and treating them as such would place the whole
// asset's speech into whichever window happens to start at zero. A segment
// only carries timing when it spans real media time (EndMS > StartMS);
// degenerate segments carry no placement information and are ignored.
func (t Transcript) Timed() bool {
	for _, segment := range t.Segments {
		if segment.EndMS > segment.StartMS {
			return true
		}
	}
	return false
}

// TranscriptFromAlignmentWords builds the canonical timed transcript from a
// word-level forced-alignment result. Word timestamps are the strongest
// timing evidence the pipeline has; when they exist, analysis must consume
// them rather than the ASR transcript's segments (or its untimed text).
// Degenerate words (EndMS <= StartMS) carry no placement and are dropped;
// if none survive the result is nil and the caller falls back to the ASR
// transcript.
func TranscriptFromAlignmentWords(words []AlignmentWord) *Transcript {
	segments := make([]TranscriptSegment, 0, len(words))
	text := make([]byte, 0, len(words)*8)
	for _, word := range words {
		if word.EndMS <= word.StartMS {
			continue
		}
		segments = append(segments, TranscriptSegment{StartMS: word.StartMS, EndMS: word.EndMS, Text: word.Text})
		if len(text) > 0 {
			text = append(text, ' ')
		}
		text = append(text, word.Text...)
	}
	if len(segments) == 0 {
		return nil
	}
	return &Transcript{Segments: segments, Text: string(text)}
}

type StructuredAnalysis struct {
	AssetType       string   `json:"asset_type"`
	SceneTags       []string `json:"scene_tags"`
	Subjects        []string `json:"subjects"`
	PeopleCount     int      `json:"people_count"`
	ShotSize        string   `json:"shot_size"`
	CameraMotion    string   `json:"camera_motion"`
	Lighting        string   `json:"lighting"`
	AudioType       string   `json:"audio_type"`
	HasSpeech       bool     `json:"has_speech"`
	Summary         string   `json:"summary"`
	UsableAs        []string `json:"usable_as"`
	MoodTags        []string `json:"mood_tags"`
	Quality         string   `json:"quality"`
	QualityFlags    []string `json:"quality_flags"`
	ExtraTags       []string `json:"extra_tags"`
	EditorialReason string   `json:"editorial_reason"`
}

type AssetDetail struct {
	Asset         Asset               `json:"asset"`
	Location      *AssetLocation      `json:"location,omitempty"`
	Metadata      *MediaMetadata      `json:"metadata,omitempty"`
	ThumbnailPath string              `json:"thumbnail_path,omitempty"`
	ProxyPath     string              `json:"proxy_path,omitempty"`
	Transcript    *Transcript         `json:"transcript,omitempty"`
	Analysis      *StructuredAnalysis `json:"analysis,omitempty"`
}

type ProviderFile struct {
	ID, AssetID, ArtifactType, ProfileHash, Provider string
	RemoteName, RemoteURI, MIMEType, State           string
	SizeBytes                                        int64
	CreatedAt, LastUsedAt                            time.Time
	ExpiresAt                                        *time.Time
	ErrorMessage                                     string
}

type AlignmentWord struct {
	StartMS    int64    `json:"start_ms"`
	EndMS      int64    `json:"end_ms"`
	Text       string   `json:"text"`
	Confidence *float64 `json:"confidence,omitempty"`
}

type AlignmentResult struct {
	Words       []AlignmentWord `json:"words"`
	RawResponse string          `json:"-"`
}

type AssetCard struct {
	ID               string           `json:"id"`
	Filename         string           `json:"filename"`
	State            AssetState       `json:"state"`
	ProcessingStatus ProcessingStatus `json:"processing_status,omitempty"`
	DurationMS       int64            `json:"duration_ms"`
	Orientation      string           `json:"orientation"`
	CapturedAt       *time.Time       `json:"captured_at,omitempty"`
	CameraModel      string           `json:"camera_model,omitempty"`
	RegionLabel      string           `json:"region_label,omitempty"`
	SessionID        string           `json:"session_id,omitempty"`
	SourceColor      string           `json:"source_color,omitempty"`
	ColorProfile     string           `json:"color_profile,omitempty"`
	RawFormat        string           `json:"raw_format,omitempty"`
	PreviewStatus    string           `json:"preview_status,omitempty"`
	ThumbnailURL     string           `json:"thumbnail_url,omitempty"`
	ProxyURL         string           `json:"proxy_url,omitempty"`
	Summary          string           `json:"summary,omitempty"`
	AssetType        string           `json:"asset_type,omitempty"`
	CameraMotion     string           `json:"camera_motion,omitempty"`
	Lighting         string           `json:"lighting,omitempty"`
	HasSpeech        bool             `json:"has_speech"`
	Quality          string           `json:"quality,omitempty"`
	UsableAs         []string         `json:"usable_as,omitempty"`
	MoodTags         []string         `json:"mood_tags,omitempty"`
	Transcript       string           `json:"transcript,omitempty"`
}
