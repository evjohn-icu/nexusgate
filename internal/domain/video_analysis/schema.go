// Package video_analysis contains the provider-independent contract for video
// understanding. It deliberately keeps the existing StructuredAnalysis as a
// compatibility field while the rest of the pipeline moves to the richer
// scene/shot-oriented result.
package video_analysis

import (
	"github.com/evjohn-icu/timingdex/internal/domain"
)

type Input struct {
	VideoPath  string
	RemoteURI  string
	MIMEType   string
	Transcript *domain.Transcript
	Metadata   domain.MediaMetadata
	Prompt     string
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

func (r Result) ToAssetShots(assetID, sourceRunID string) []domain.AssetShot {
	objects := make([]string, 0, len(r.Objects))
	for _, object := range r.Objects {
		objects = appendUnique(objects, object.Name)
	}
	actions := make([]string, 0, len(r.Actions))
	for _, action := range r.Actions {
		actions = appendUnique(actions, action.Name)
	}
	shots := make([]domain.AssetShot, 0, len(r.Shots))
	for i, shot := range r.Shots {
		shotObjects := shot.Objects
		if len(shotObjects) == 0 {
			shotObjects = objects
		}
		shotActions := shot.Actions
		if len(shotActions) == 0 {
			shotActions = actions
		}
		shotMood := shot.Mood
		if len(shotMood) == 0 {
			shotMood = r.Mood
		}
		shots = append(shots, domain.AssetShot{
			AssetID: assetID, SourceRunID: sourceRunID, Ordinal: i,
			StartMS: shot.StartMS, EndMS: shot.EndMS, Description: shot.Description,
			Tags: shot.Tags, Objects: shotObjects, Actions: shotActions,
			Mood: shotMood, Confidence: shot.Confidence,
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
