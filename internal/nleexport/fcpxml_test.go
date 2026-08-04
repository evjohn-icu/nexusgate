package nleexport

import (
	"encoding/xml"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/evjohn-icu/timingdex/internal/timecode"
)

// The types below mirror the FCPXML the exporter writes. They live in the test
// file rather than reusing the marshaling structs on purpose: a wrong xml tag
// in a production struct would otherwise make an unmarshal test pass against
// the same wrong shape.

type fcpxmlTestDoc struct {
	XMLName   xml.Name          `xml:"fcpxml"`
	Version   string            `xml:"version,attr"`
	Resources fcpxmlTestRes     `xml:"resources"`
	Library   fcpxmlTestLibrary `xml:"library"`
}

type fcpxmlTestRes struct {
	Formats []fcpxmlTestFormat `xml:"format"`
	Assets  []fcpxmlTestAsset  `xml:"asset"`
}

type fcpxmlTestFormat struct {
	ID            string `xml:"id,attr"`
	FrameDuration string `xml:"frameDuration,attr"`
}

type fcpxmlTestAsset struct {
	ID   string `xml:"id,attr"`
	Name string `xml:"name,attr"`
	Src  string `xml:"src,attr"`
}

type fcpxmlTestLibrary struct {
	Event fcpxmlTestEvent `xml:"event"`
}

type fcpxmlTestEvent struct {
	Project fcpxmlTestProject `xml:"project"`
}

type fcpxmlTestProject struct {
	Sequence fcpxmlTestSequence `xml:"sequence"`
}

type fcpxmlTestSequence struct {
	Format   string          `xml:"format,attr"`
	Duration string          `xml:"duration,attr"`
	Spine    fcpxmlTestSpine `xml:"spine"`
}

type fcpxmlTestSpine struct {
	Clips []fcpxmlTestClip `xml:"asset-clip"`
}

type fcpxmlTestClip struct {
	Ref      string `xml:"ref,attr"`
	Offset   string `xml:"offset,attr"`
	Start    string `xml:"start,attr"`
	Duration string `xml:"duration,attr"`
}

// unmarshalFCPXML insists the document parses with encoding/xml before any
// assertion runs: a string comparison would not notice a malformed document
// that happened to match, and the whole point of the exporter is a document an
// NLE can actually parse.
func unmarshalFCPXML(t *testing.T, out string) fcpxmlTestDoc {
	t.Helper()
	var doc fcpxmlTestDoc
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("document does not parse as FCPXML: %v\n%s", err, out)
	}
	return doc
}

// frames is the frame count a rational N/Ds attribute stands for at the given
// rate. It is asserted as an exact division: a rational that does not land on a
// whole frame would mean the exporter drifted.
func frames(t *testing.T, rational string, fpsNum, fpsDen int64) int64 {
	t.Helper()
	n, d := rationalPair(t, rational)
	frames := n * fpsNum / (d * fpsDen)
	if frames*d*fpsDen != n*fpsNum {
		t.Fatalf("rational %q at %d/%d fps is not a whole number of frames", rational, fpsNum, fpsDen)
	}
	return frames
}

func rationalPair(t *testing.T, s string) (num, den int64) {
	t.Helper()
	if !strings.HasSuffix(s, "s") {
		t.Fatalf("time %q lacks the 's' suffix", s)
	}
	parts := strings.SplitN(strings.TrimSuffix(s, "s"), "/", 2)
	if len(parts) != 2 {
		t.Fatalf("time %q is not N/Ds", s)
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatalf("time %q: bad numerator: %v", s, err)
	}
	d, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("time %q: bad denominator: %v", s, err)
	}
	if d == 0 {
		t.Fatalf("time %q has zero denominator", s)
	}
	return n, d
}

func TestFCPXMLSingleClip(t *testing.T) {
	out, err := FCPXML(Timeline{
		Title: "My Plan",
		Clips: []Clip{{Reel: "AX001", SourceInMS: 0, SourceOutMS: 10000, FPS: 25}},
	}, map[string]Source{
		"AX001": {Path: "/tmp/AX001.mov", Name: "AX001 take 1", Duration: 12000},
	})
	if err != nil {
		t.Fatalf("FCPXML returned error: %v", err)
	}
	doc := unmarshalFCPXML(t, out)

	if doc.Version != "1.9" {
		t.Errorf("fcpxml version = %q, want 1.9", doc.Version)
	}
	if len(doc.Resources.Formats) != 1 {
		t.Fatalf("formats = %d, want 1", len(doc.Resources.Formats))
	}
	if got := doc.Resources.Formats[0].FrameDuration; got != "1/25s" {
		t.Errorf("format frameDuration = %q, want 1/25s", got)
	}
	if len(doc.Resources.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(doc.Resources.Assets))
	}
	asset := doc.Resources.Assets[0]
	if asset.Src != "file:///tmp/AX001.mov" {
		t.Errorf("asset src = %q, want file:///tmp/AX001.mov", asset.Src)
	}
	if doc.Library.Event.Project.Sequence.Format != doc.Resources.Formats[0].ID {
		t.Errorf("sequence format %q does not reference the declared format", doc.Library.Event.Project.Sequence.Format)
	}

	clips := doc.Library.Event.Project.Sequence.Spine.Clips
	if len(clips) != 1 {
		t.Fatalf("spine clips = %d, want 1", len(clips))
	}
	c := clips[0]
	if c.Ref != asset.ID {
		t.Errorf("asset-clip ref %q does not reference asset %q", c.Ref, asset.ID)
	}
	if c.Offset != "0/1s" || c.Start != "0/1s" {
		t.Errorf("asset-clip offset/start = %q/%q, want 0/1s/0/1s", c.Offset, c.Start)
	}
	// 10s at 25fps is 250 frames, exactly 10/1s when reduced.
	if c.Duration != "10/1s" {
		t.Errorf("asset-clip duration = %q, want 10/1s", c.Duration)
	}
}

// The frame duration rational is the one place a rounding error is
// unrecoverable: 1001/30000s and 0.033s both import, but only the rational
// keeps a multi-hour timeline aligned with the source.
func TestFCPXMLFrameDurationRationals(t *testing.T) {
	tests := []struct {
		name string
		fps  float64
		want string
	}{
		{"film 24", 24, "1/24s"},
		{"PAL 25", 25, "1/25s"},
		{"nominal 30", 30, "1/30s"},
		{"29.97", 30000.0 / 1001.0, "1001/30000s"},
		{"59.94", 60000.0 / 1001.0, "1001/60000s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := FCPXML(Timeline{
				Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: tt.fps}},
			}, map[string]Source{"A": {Path: "/a.mov", Duration: 10000}})
			if err != nil {
				t.Fatalf("FCPXML returned error: %v", err)
			}
			doc := unmarshalFCPXML(t, out)
			if len(doc.Resources.Formats) != 1 {
				t.Fatalf("formats = %d, want 1", len(doc.Resources.Formats))
			}
			if got := doc.Resources.Formats[0].FrameDuration; got != tt.want {
				t.Errorf("frameDuration = %q, want %q", got, tt.want)
			}
		})
	}
}

// For a straight cut the record position must continue from the previous
// clip's record end and each clip's duration must equal its source range frame
// for frame; an NLE reads any disagreement as a gap or an overlap, silently,
// since the document still parses.
func TestFCPXMLTwoClipsChain(t *testing.T) {
	const fpsNum, fpsDen = 25, 1
	out, err := FCPXML(Timeline{
		Title: "Chain",
		Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 2000, FPS: 25},
			{Reel: "B", SourceInMS: 5000, SourceOutMS: 7000, FPS: 25},
		},
	}, map[string]Source{
		"A": {Path: "/a.mov", Name: "a", Duration: 20000},
		"B": {Path: "/b.mov", Name: "b", Duration: 20000},
	})
	if err != nil {
		t.Fatalf("FCPXML returned error: %v", err)
	}
	doc := unmarshalFCPXML(t, out)

	if len(doc.Resources.Assets) != 2 {
		t.Fatalf("assets = %d, want 2", len(doc.Resources.Assets))
	}
	clips := doc.Library.Event.Project.Sequence.Spine.Clips
	if len(clips) != 2 {
		t.Fatalf("spine clips = %d, want 2", len(clips))
	}

	srcInA, _ := timecode.Frames(0, 25)
	srcOutA, _ := timecode.Frames(2000, 25)
	srcInB, _ := timecode.Frames(5000, 25)
	srcOutB, _ := timecode.Frames(7000, 25)

	if got := frames(t, clips[0].Start, fpsNum, fpsDen); got != srcInA {
		t.Errorf("clip 1 start = %d frames, want %d", got, srcInA)
	}
	if got := frames(t, clips[0].Duration, fpsNum, fpsDen); got != srcOutA-srcInA {
		t.Errorf("clip 1 duration = %d frames, want %d", got, srcOutA-srcInA)
	}
	if got := frames(t, clips[0].Offset, fpsNum, fpsDen); got != 0 {
		t.Errorf("clip 1 offset = %d frames, want 0", got)
	}

	if got := frames(t, clips[1].Start, fpsNum, fpsDen); got != srcInB {
		t.Errorf("clip 2 start = %d frames, want %d", got, srcInB)
	}
	if got := frames(t, clips[1].Duration, fpsNum, fpsDen); got != srcOutB-srcInB {
		t.Errorf("clip 2 duration = %d frames, want %d", got, srcOutB-srcInB)
	}
	// The record offset must continue from clip 1's record end.
	if got, want := frames(t, clips[1].Offset, fpsNum, fpsDen), frames(t, clips[0].Duration, fpsNum, fpsDen); got != want {
		t.Errorf("clip 2 offset = %d frames, want to continue from clip 1 duration %d", got, want)
	}
	if got, want := frames(t, doc.Library.Event.Project.Sequence.Duration, fpsNum, fpsDen), (srcOutA-srcInA)+(srcOutB-srcInB); got != want {
		t.Errorf("sequence duration = %d frames, want %d", got, want)
	}
}

// The attribute escaper and the URL escaper are both load-bearing: a path with
// &, <, >, ' or " must not be able to produce a document that parses into
// something different, and the file URL must survive the round trip byte for
// byte so an NLE relinks to the exact file the caller named.
func TestFCPXMLPathEscaping(t *testing.T) {
	path := `/Users/ev/Files/code/时序 & <raw> "quotes" 'single'.mov`
	name := `clip <A> & "B" 'C' 拍摄`
	out, err := FCPXML(Timeline{
		Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 25}},
	}, map[string]Source{"A": {Path: path, Name: name, Duration: 10000}})
	if err != nil {
		t.Fatalf("FCPXML returned error: %v", err)
	}
	doc := unmarshalFCPXML(t, out)

	if len(doc.Resources.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(doc.Resources.Assets))
	}
	asset := doc.Resources.Assets[0]
	u, err := url.Parse(asset.Src)
	if err != nil {
		t.Fatalf("asset src %q is not a parseable URL: %v", asset.Src, err)
	}
	if u.Scheme != "file" {
		t.Errorf("asset src scheme = %q, want file", u.Scheme)
	}
	if u.Path != path {
		t.Errorf("round-tripped path = %q, want exactly %q", u.Path, path)
	}
	if asset.Name != name {
		t.Errorf("round-tripped name = %q, want exactly %q", asset.Name, name)
	}
}

func TestFCPXMLMissingSource(t *testing.T) {
	_, err := FCPXML(Timeline{
		Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
			{Reel: "B", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
		},
	}, map[string]Source{"A": {Path: "/a.mov", Duration: 10000}})
	if err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
	if !strings.Contains(err.Error(), "no source supplied") {
		t.Errorf("error %q does not name the missing source condition", err)
	}
	if !strings.Contains(err.Error(), `"B"`) {
		t.Errorf("error %q does not name the missing reel B", err)
	}
}

func TestFCPXMLSourceTooShort(t *testing.T) {
	_, err := FCPXML(Timeline{
		Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 5000, FPS: 25}},
	}, map[string]Source{"A": {Path: "/a.mov", Duration: 4000}})
	if err == nil {
		t.Fatal("expected error for source shorter than out point, got nil")
	}
	if !strings.Contains(err.Error(), "shorter than clip's source out point") {
		t.Errorf("error %q does not describe the short source", err)
	}
}

func TestFCPXMLErrors(t *testing.T) {
	tests := []struct {
		name     string
		timeline Timeline
		sources  map[string]Source
		wantErr  string
	}{
		{"no clips", Timeline{Title: "x"}, nil, "no clips"},
		{"source out equals source in", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 5000, SourceOutMS: 5000, FPS: 25}}}, nil, "not after its source in point"},
		{"source out before source in", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 5000, SourceOutMS: 1000, FPS: 25}}}, nil, "not after its source in point"},
		{"negative source in", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: -1, SourceOutMS: 0, FPS: 25}}}, nil, "negative source in"},
		{"zero frame rate", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 0}}}, nil, "frame rate"},
		{"negative frame rate", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: -25}}}, nil, "frame rate"},
		{"mixed frame rates", Timeline{Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
			{Reel: "B", SourceInMS: 0, SourceOutMS: 1000, FPS: 24},
		}}, nil, "disagree on frame rate"},
		{"mixed drop and non-drop", Timeline{Clips: []Clip{
			{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 30000.0 / 1001.0},
			{Reel: "B", SourceInMS: 0, SourceOutMS: 1000, FPS: 25},
		}}, nil, "disagree on frame rate"},
		// A source long enough to pass the duration check: the failure must
		// come from the frame arithmetic, not the source bounds.
		{"range shorter than one frame", Timeline{Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 10, FPS: 25}}}, map[string]Source{"A": {Path: "/a.mov", Duration: 10000}}, "less than one frame"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FCPXML(tc.timeline, tc.sources)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// url.URL turns the first segment of a relative path into the URL's host, so
// "clips/a.mp4" would be emitted as file://clips/a.mp4 and the NLE would look
// for a machine called "clips". It imports as offline media rather than as an
// error, which is exactly the class of silent wrongness this package refuses.
func TestFCPXMLRejectsNonAbsoluteSourcePath(t *testing.T) {
	for _, path := range []string{"clips/a.mp4", "a.mp4", "./a.mp4", ""} {
		_, err := FCPXML(
			Timeline{Title: "t", Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 25}}},
			map[string]Source{"A": {Path: path, Name: "A", Duration: 60000}},
		)
		if err == nil {
			t.Errorf("path %q was accepted, want a refusal", path)
		}
	}
	if _, err := FCPXML(
		Timeline{Title: "t", Clips: []Clip{{Reel: "A", SourceInMS: 0, SourceOutMS: 1000, FPS: 25}}},
		map[string]Source{"A": {Path: "/mnt/a.mp4", Name: "A", Duration: 60000}},
	); err != nil {
		t.Fatalf("absolute path refused: %v", err)
	}
}
