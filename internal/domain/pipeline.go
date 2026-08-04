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

// JobDeferProviderRouteExhausted is the DeferReason recorded when every
// provider key on a capability's route is failing at once. The recommended
// deployment runs on a plan with a hard monthly quota, so an exhausted account
// answers 429 on every key together; that is a fact about the account, not
// about the job, and the job's attempt budget must survive it.
//
// The value is a Hub-assigned constant rather than upstream text on purpose:
// /progress renders it to any viewer, while last_error_message — which can
// embed a truncated provider response body — stays behind the admin token.
const JobDeferProviderRouteExhausted = "provider_route_exhausted"

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
