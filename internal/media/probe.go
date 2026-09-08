package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// ErrProbeRejected reports that ffprobe ran to completion and judged the file
// unreadable — a truncated MOV with no moov atom, an unsupported container.
// Only *exec.ExitError wraps it: a missing binary (*exec.Error) or a cancelled
// context says nothing about the file and must keep its retry meaning, so
// callers branch on the error chain rather than on the message text.
var ErrProbeRejected = errors.New("ffprobe rejected the file")

// probeStderrLimit bounds the ffprobe diagnostics embedded in the error. That
// text is persisted in jobs.last_error_message and rendered on the /progress
// page, so an unbounded copy would bloat the row and let one multi-line stderr
// break the table.
const probeStderrLimit = 512

type FFProbeResult struct {
	Format struct {
		Filename   string            `json:"filename"`
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		Size       string            `json:"size"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []FFProbeStream `json:"streams"`
}

type FFProbeStream struct {
	Index            int               `json:"index"`
	CodecName        string            `json:"codec_name"`
	CodecType        string            `json:"codec_type"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	AvgFrameRate     string            `json:"avg_frame_rate"`
	PixFmt           string            `json:"pix_fmt"`
	Profile          string            `json:"profile"`
	ColorSpace       string            `json:"color_space"`
	ColorTransfer    string            `json:"color_transfer"`
	ColorPrimaries   string            `json:"color_primaries"`
	BitsPerRawSample string            `json:"bits_per_raw_sample"`
	CodecTagString   string            `json:"codec_tag_string"`
	Tags             map[string]string `json:"tags"`
	SideDataList     []map[string]any  `json:"side_data_list"`
}

func Probe(ctx context.Context, path string) (FFProbeResult, error) {
	command := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_format", "-show_streams", "-of", "json", path)
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return FFProbeResult{}, fmt.Errorf("ffprobe: %w: %w%s", ErrProbeRejected, err, probeStderrSuffix(exit.Stderr))
		}
		return FFProbeResult{}, fmt.Errorf("ffprobe: %w", err)
	}
	var result FFProbeResult
	if err := json.Unmarshal(output, &result); err != nil {
		return FFProbeResult{}, fmt.Errorf("decode ffprobe result: %w", err)
	}
	return result, nil
}

// probeStderrSuffix renders ffprobe's own diagnostics as an optional ": ..."
// suffix for the error message. It stays one bounded line because the message
// is persisted in jobs.last_error_message and printed on the /progress page,
// where a multi-line or oversized value would corrupt the row.
func probeStderrSuffix(stderr []byte) string {
	message := strings.Join(strings.Fields(string(stderr)), " ")
	if message == "" {
		return ""
	}
	if len(message) > probeStderrLimit {
		message = message[:probeStderrLimit]
		// A byte cut can land inside a multi-byte rune, and this text is stored
		// as UTF-8, so drop the partial rune rather than persisting invalid bytes.
		for len(message) > 0 && !utf8.RuneStart(message[len(message)-1]) {
			message = message[:len(message)-1]
		}
		message += "…(truncated)"
	}
	return ": " + message
}
