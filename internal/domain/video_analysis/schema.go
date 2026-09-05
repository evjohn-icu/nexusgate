// Package video_analysis contains the provider-independent contract for video
// understanding. It deliberately keeps the existing StructuredAnalysis as a
// compatibility field while the rest of the pipeline moves to the richer
// scene/shot-oriented result.
package video_analysis

import (
	"github.com/evjohn-icu/nexusgate/internal/domain"
)

// Frame is one still image extracted from an asset's proxy at a known point
// on the asset timeline. Providers that understand video whole (Gemini,
// openai_video) ignore Frames; the multiframe protocol is fed exclusively by
// them — NexusGate decides the timeline, the model only describes pixels.
type Frame struct {
	Path        string `json:"path"`
	TimestampMS int64  `json:"timestamp_ms"`
}

type Input struct {
	VideoPath  string
	RemoteURI  string
	MIMEType   string
	Transcript *domain.Transcript
	Metadata   domain.MediaMetadata
	Prompt     string
	// Frames carries pre-extracted stills for providers that cannot consume
	// video directly (openai_multiframe). Each frame's timestamp is relative
	// to the asset timeline, not to any window the asset was cut into.
	Frames []Frame
}

type Scene struct {
	Description string   `json:"description,omitempty"`
	StartMS     int64    `json:"start_ms,omitempty"`
	EndMS       int64    `json:"end_ms,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

type Object struct {
	Name       string  `json:"name"`
	Count      int     `json:"count,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

type Action struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence,omitempty"`
}

type Shot struct {
	StartMS     int64    `json:"start_ms"`
	EndMS       int64    `json:"end_ms"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Objects     []string `json:"objects,omitempty"`
	Actions     []string `json:"actions,omitempty"`
	Mood        []string `json:"mood,omitempty"`
	Confidence  float64  `json:"confidence,omitempty"`
}

type Result struct {
	Summary    string                    `json:"summary"`
	Scenes     []Scene                   `json:"scenes,omitempty"`
	Objects    []Object                  `json:"objects,omitempty"`
	Actions    []Action                  `json:"actions,omitempty"`
	Mood       []string                  `json:"mood,omitempty"`
	RawTags    []string                  `json:"raw_tags,omitempty"`
	Shots      []Shot                    `json:"shots,omitempty"`
	Confidence float64                   `json:"confidence,omitempty"`
	Analysis   domain.StructuredAnalysis `json:"analysis,omitempty"`
}

func (r Result) ToStructuredAnalysis() domain.StructuredAnalysis {
	a := r.Analysis
	if a.Summary == "" {
		a.Summary = r.Summary
	}
	for _, scene := range r.Scenes {
		a.SceneTags = appendUnique(a.SceneTags, scene.Tags...)
	}
	a.SceneTags = appendUnique(a.SceneTags, r.RawTags...)
	for _, object := range r.Objects {
		a.Subjects = appendUnique(a.Subjects, object.Name)
		if object.Count > a.PeopleCount && object.Name == "person" {
			a.PeopleCount = object.Count
		}
	}
	for _, action := range r.Actions {
		a.ExtraTags = appendUnique(a.ExtraTags, action.Name)
	}
	a.MoodTags = appendUnique(a.MoodTags, r.Mood...)
	return a
}

// ToAssetShots maps the model's per-shot observations onto the canonical shot
// table. Shot metadata must come from the shot's own evidence: an object that
// was only seen at 08:20 of a ten-minute asset belongs to the shots the model
// put it in, not to every shot that happened to be missing a list. The
// asset-global lists stay on the asset (ToStructuredAnalysis) where they
// describe the whole file without masquerading as per-shot evidence.
func (r Result) ToAssetShots(assetID, sourceRunID string) []domain.AssetShot {
	shots := make([]domain.AssetShot, 0, len(r.Shots))
	for i, shot := range r.Shots {
		shots = append(shots, domain.AssetShot{
			AssetID: assetID, SourceRunID: sourceRunID, Ordinal: i,
			StartMS: shot.StartMS, EndMS: shot.EndMS, Description: shot.Description,
			Tags: shot.Tags, Objects: shot.Objects, Actions: shot.Actions,
			Mood: shot.Mood, Confidence: shot.Confidence,
		})
	}
	return shots
}

func appendUnique(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, value := range dst {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		dst = append(dst, value)
	}
	return dst
}
