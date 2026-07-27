package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

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
		return FFProbeResult{}, fmt.Errorf("ffprobe: %w", err)
	}
	var result FFProbeResult
	if err := json.Unmarshal(output, &result); err != nil {
		return FFProbeResult{}, fmt.Errorf("decode ffprobe result: %w", err)
	}
	return result, nil
}
