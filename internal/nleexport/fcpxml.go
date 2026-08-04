// The EDL in this package renders a timeline as flat text; FCPXML renders the
// same timeline as a structured interchange document. The two share the Clip
// and Timeline types and the same frame-domain arithmetic, but FCPXML is a
// different beast for a different import path: it carries media locations and
// exact rational durations instead of SMPTE labels, and it must stay
// well-formed XML because an NLE that cannot parse the document fails the whole
// import rather than one edit. Everything in this file exists to keep that
// document parseable and lossless.
package nleexport

import (
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/timecode"
)

// Source describes where a clip's media actually lives. It is supplied by the
// caller, never discovered here: this package performs no I/O and cannot
// relink a clip to the wrong file, so a missing or wrong path can only be a
// caller bug and is reported as such rather than guessed at.
type Source struct {
	Path     string // absolute path to the original media
	Name     string // display name for the asset
	Duration int64  // full duration of the source file in milliseconds
}

// FCPXML renders the timeline as an FCPXML 1.9 document an NLE can import.
// sources must contain one entry for every distinct Clip.Reel in the timeline;
// a missing entry is an error naming the reel.
func FCPXML(t Timeline, sources map[string]Source) (string, error) {
	// These checks are duplicated from EDL deliberately: edl.go is out of
	// scope for this file, so instead of refactoring the shared validation out
	// of it, each check is repeated here so a timeline that EDL rejects is
	// rejected identically by FCPXML. Keep the two in agreement.
	if len(t.Clips) == 0 {
		return "", fmt.Errorf("%w: timeline has no clips", ErrInvalidTimeline)
	}

	fps := t.Clips[0].FPS
	for i := range t.Clips {
		c := &t.Clips[i]
		if c.FPS <= 0 {
			return "", fmt.Errorf("%w: clip %d has frame rate %v, which is not positive", ErrInvalidTimeline, i+1, c.FPS)
		}
		// FCPXML carries one <format> for the whole sequence, so every clip
		// must agree on the rate: a mixed-rate sequence imports with a single
		// frame duration and silently mis-times every clip but the first.
		if c.FPS != fps {
			return "", fmt.Errorf("%w: clips disagree on frame rate: clip 1 is %v fps, clip %d is %v fps", ErrInvalidTimeline, fps, i+1, c.FPS)
		}
		if c.SourceInMS < 0 {
			return "", fmt.Errorf("%w: clip %d has negative source in point %d", ErrInvalidTimeline, i+1, c.SourceInMS)
		}
		// An out point at or before the in point has no duration. Writing it
		// anyway yields a plausible-looking document with a zero-length clip,
		// which is harder to notice than the error.
		if c.SourceOutMS <= c.SourceInMS {
			return "", fmt.Errorf("%w: clip %d source out point %d is not after its source in point %d", ErrInvalidTimeline, i+1, c.SourceOutMS, c.SourceInMS)
		}
	}

	// Every distinct reel must resolve to a source. A missing entry is a
	// caller bug that would otherwise import as an asset-clip with no media,
	// which an NLE silently leaves black.
	reels := make(map[string]Source)
	for i := range t.Clips {
		c := &t.Clips[i]
		src, ok := sources[c.Reel]
		if !ok {
			return "", fmt.Errorf("%w: no source supplied for reel %q", ErrInvalidTimeline, c.Reel)
		}
		// A source shorter than the clip's out point means the caller is
		// asking to use footage past the end of the file; an NLE imports that
		// region as a silent black gap rather than an error, so refuse it
		// here.
		if src.Duration < c.SourceOutMS {
			return "", fmt.Errorf("%w: source for reel %q has duration %dms, shorter than clip's source out point %dms", ErrInvalidTimeline, c.Reel, src.Duration, c.SourceOutMS)
		}
		// A relative path cannot survive the trip through a file URL: the first
		// segment becomes the URL's HOST, so "clips/a.mp4" emits
		// file://clips/a.mp4 and the NLE looks for a machine named "clips".
		// That fails as offline media rather than as an import error, which is
		// the failure mode this whole package refuses to produce.
		if !filepath.IsAbs(src.Path) {
			return "", fmt.Errorf("%w: source for reel %q has path %q, which is not absolute; a relative path becomes a URL host and imports as offline media", ErrInvalidTimeline, c.Reel, src.Path)
		}
		reels[c.Reel] = src
	}

	// FCPXML expresses every duration as a rational number of seconds. The
	// frame rate is recovered as an exact rational N/D so that one frame at
	// 29.97 emits as 1001/30000s rather than a rounded 0.033s: a rounded
	// decimal drifts the whole timeline after import while a rational stays
	// exact.
	fpsNum, fpsDen := frameRateRational(fps)
	frameDuration := fmt.Sprintf("%d/%ds", fpsDen, fpsNum)

	// Distinct sources are collected in first-use order so a given source
	// keeps the same resource id across re-exports of the same timeline.
	type distinct struct {
		reel string
		src  Source
	}
	var order []distinct
	seen := make(map[string]struct{})
	for i := range t.Clips {
		c := &t.Clips[i]
		if _, ok := seen[c.Reel]; ok {
			continue
		}
		seen[c.Reel] = struct{}{}
		order = append(order, distinct{c.Reel, reels[c.Reel]})
	}

	formatID := "r1"
	ids := make(map[string]string, len(order))
	assets := make([]asset, 0, len(order))
	for i, d := range order {
		id := fmt.Sprintf("r%d", i+2)
		ids[d.reel] = id
		assets = append(assets, asset{
			ID:   id,
			Name: d.src.Name,
			Src:  fileURL(d.src.Path),
		})
	}

	// All position arithmetic is frame arithmetic, not millisecond
	// arithmetic, for the same reason edl.go documents: converting each
	// millisecond position independently lets each one round by its own
	// sub-frame phase, so the same clip can occupy a different number of
	// frames on the source side and the timeline side. Convert at the edges
	// with timecode.Frames and derive the record position by adding frame
	// counts, which makes the two sides equal by construction.
	var recFrames int64
	clips := make([]assetClip, 0, len(t.Clips))
	for i := range t.Clips {
		c := &t.Clips[i]
		srcInFrames, err := timecode.Frames(c.SourceInMS, fps)
		if err != nil {
			return "", fmt.Errorf("%w: clip %d source in: %w", ErrInvalidTimeline, i+1, err)
		}
		srcOutFrames, err := timecode.Frames(c.SourceOutMS, fps)
		if err != nil {
			return "", fmt.Errorf("%w: clip %d source out: %w", ErrInvalidTimeline, i+1, err)
		}
		// A range shorter than one frame survives the millisecond check above
		// but has nothing to cut. Refusing beats emitting a zero-length clip.
		if srcOutFrames == srcInFrames {
			return "", fmt.Errorf("%w: clip %d spans %dms, which is less than one frame at %v fps", ErrInvalidTimeline, i+1, c.SourceOutMS-c.SourceInMS, fps)
		}
		durationFrames := srcOutFrames - srcInFrames
		clips = append(clips, assetClip{
			Ref:      ids[c.Reel],
			Offset:   rationalSeconds(recFrames, fpsNum, fpsDen),
			Start:    rationalSeconds(srcInFrames, fpsNum, fpsDen),
			Duration: rationalSeconds(durationFrames, fpsNum, fpsDen),
			Name:     reels[c.Reel].Name,
			TCFormat: tcFormat(fps),
		})
		recFrames += durationFrames
	}

	doc := fcpxmlDoc{
		Version: "1.9",
		Resources: resources{
			Formats: []format{{ID: formatID, Name: formatName(fpsNum, fpsDen), FrameDuration: frameDuration}},
			Assets:  assets,
		},
		Library: library{
			Event: event{
				Name: t.Title,
				Project: project{
					Name: t.Title,
					Sequence: sequence{
						Format:   formatID,
						TCStart:  "0/1s",
						Duration: rationalSeconds(recFrames, fpsNum, fpsDen),
						TCFormat: tcFormat(fps),
						Spine:    spine{Clips: clips},
					},
				},
			},
		},
	}

	data, err := xml.MarshalIndent(doc, "", "\t")
	if err != nil {
		return "", fmt.Errorf("%w: marshal fcpxml: %w", ErrInvalidTimeline, err)
	}
	// encoding/xml neither emits the declaration nor knows about the DOCTYPE,
	// so both are prepended as fixed text; the marshaled document below is
	// escaped by the library, which is what keeps source paths and names from
	// breaking the parse.
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE fcpxml PUBLIC \"-//Apple//DTD FCPXML 1.9//EN\" \"http://developer.apple.com/fcpxml/documentation/FCPXML-1.9.dtd\">\n")
	b.Write(data)
	b.WriteByte('\n')
	return b.String(), nil
}

// frameRateRational recovers the exact frames-per-second rational N/D that the
// caller's float describes. A rounded decimal frame duration would drift the
// whole timeline after import, so the rational must be exact, not
// approximate.
func frameRateRational(fps float64) (num, den int64) {
	// The NTSC drop rates arrive pre-divided (30000.0/1001.0), so comparing
	// the float against the exact rational is the only reliable test; the
	// tolerance matches timecode.DropFrame so the two export paths agree on
	// what counts as an NTSC rate.
	if math.Abs(fps-30000.0/1001.0) < 0.01 {
		return 30000, 1001
	}
	if math.Abs(fps-60000.0/1001.0) < 0.01 {
		return 60000, 1001
	}
	// Continued-fraction expansion recovers the small-denominator rational a
	// float was divided from, which is the only sane source for an exact
	// frame duration. The denominator cap keeps a pathological rate from
	// growing without bound.
	n0, d0 := int64(0), int64(1)
	n1, d1 := int64(1), int64(0)
	x := fps
	for i := 0; i < 64; i++ {
		a := int64(x)
		n2 := a*n1 + n0
		d2 := a*d1 + d0
		if d2 > 1<<20 {
			break
		}
		n0, d0, n1, d1 = n1, d1, n2, d2
		frac := x - float64(a)
		if frac < 1e-9 {
			break
		}
		x = 1 / frac
	}
	return n1, d1
}

// rationalSeconds renders a frame count as a reduced rational number of
// seconds: F frames at N/D frames per second is F*D/N seconds. Reduction keeps
// the numbers small (250 frames at 25fps emits 10/1s rather than 250/25s),
// which NLEs accept either way but is easier to audit.
func rationalSeconds(frames, fpsNum, fpsDen int64) string {
	n := frames * fpsDen
	d := fpsNum
	g := gcd(n, d)
	n /= g
	d /= g
	return fmt.Sprintf("%d/%ds", n, d)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// fileURL turns an absolute path into the file:// URL an <asset> src expects.
// net/url percent-encodes spaces and non-ASCII bytes so the round trip through
// an NLE's parser is lossless; hand-rolling the escaping would only introduce
// a second, competing idea of what is special in a path.
func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func tcFormat(fps float64) string {
	if timecode.DropFrame(fps) {
		return "DF"
	}
	return "NDF"
}

func formatName(num, den int64) string {
	if den == 1 {
		return fmt.Sprintf("FFVideoFormatRate%dfps", num)
	}
	return fmt.Sprintf("FFVideoFormatRate%dOver%dfps", num, den)
}

// The structs below mirror the subset of FCPXML 1.9 the exporter writes. The
// document is built from structs precisely so encoding/xml escapes every
// attribute: source paths and names come from a user's filesystem and can
// contain &, <, >, ' and ", and an unescaped one must never be able to produce
// a document that parses into something different.
type fcpxmlDoc struct {
	XMLName   xml.Name  `xml:"fcpxml"`
	Version   string    `xml:"version,attr"`
	Resources resources `xml:"resources"`
	Library   library   `xml:"library"`
}

type resources struct {
	Formats []format `xml:"format"`
	Assets  []asset  `xml:"asset"`
}

type format struct {
	ID            string `xml:"id,attr"`
	Name          string `xml:"name,attr"`
	FrameDuration string `xml:"frameDuration,attr"`
}

type asset struct {
	ID   string `xml:"id,attr"`
	Name string `xml:"name,attr"`
	Src  string `xml:"src,attr"`
}

type library struct {
	Event event `xml:"event"`
}

type event struct {
	Name    string  `xml:"name,attr"`
	Project project `xml:"project"`
}

type project struct {
	Name     string   `xml:"name,attr"`
	Sequence sequence `xml:"sequence"`
}

type sequence struct {
	Format   string `xml:"format,attr"`
	TCStart  string `xml:"tcStart,attr"`
	Duration string `xml:"duration,attr"`
	TCFormat string `xml:"tcFormat,attr"`
	Spine    spine  `xml:"spine"`
}

type spine struct {
	Clips []assetClip `xml:"asset-clip"`
}

type assetClip struct {
	Ref      string `xml:"ref,attr"`
	Offset   string `xml:"offset,attr"`
	Start    string `xml:"start,attr"`
	Duration string `xml:"duration,attr"`
	Name     string `xml:"name,attr"`
	TCFormat string `xml:"tcFormat,attr"`
}
