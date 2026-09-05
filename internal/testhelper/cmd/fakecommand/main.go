// Command fakecommand provides deterministic external-process fixtures for
// tests. Its behavior is selected by the executable basename, which keeps the
// fixture usable through os/exec on Unix and Windows without a shell.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const ffprobeJSON = `{"format":{"duration":"1.0","filename":"clip.mp4"},"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,"avg_frame_rate":"30/1","pix_fmt":"yuv420p"}]}`

func main() {
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), filepath.Ext(os.Args[0]))
	switch name {
	case "ffprobe":
		output := ffprobeJSON
		if transfer := os.Getenv("NEXUSGATE_FAKE_FFPROBE_COLOR_TRANSFER"); transfer != "" {
			output = strings.Replace(output, `"pix_fmt":"yuv420p"`, `"pix_fmt":"yuv420p","color_transfer":"`+transfer+`"`, 1)
		}
		fmt.Print(output)
	case "ffmpeg":
		fakeFFmpeg()
	case "shotdetect-success":
		fmt.Print(`{"shots":[{"start_ms":0,"end_ms":4300},{"start_ms":4300,"end_ms":9000}]}`)
	case "shotdetect-invalid":
		fmt.Print("not json")
	case "shotdetect-empty":
		fmt.Print(`{"shots":[]}`)
	case "align-success":
		fmt.Print(`{"words":[{"start_ms":0,"end_ms":500,"text":"hello"},{"start_ms":600,"end_ms":900,"text":"world"}]}`)
	case "align-invalid":
		fmt.Print("not json at all")
	case "align-empty":
		fmt.Print(`{"words":[]}`)
	default:
		fmt.Fprintf(os.Stderr, "unknown nexusgate test command %q", name)
		os.Exit(2)
	}
}

func fakeFFmpeg() {
	failHardware := os.Getenv("NEXUSGATE_FAKE_FFMPEG_FAIL_HARDWARE") == "1"
	for i, arg := range os.Args[1:] {
		if failHardware && (arg == "-hwaccel" || arg == "h264_nvenc" || strings.Contains(arg, "h264_nvenc")) {
			os.Exit(1)
		}
		if arg == "-vf" && i+2 <= len(os.Args[1:]) {
			if logPath := os.Getenv("NEXUSGATE_FILTER_LOG"); logPath != "" {
				if err := os.WriteFile(logPath, []byte(os.Args[1:][i+1]), 0o600); err != nil {
					fmt.Fprintf(os.Stderr, "write fake ffmpeg filter log: %v", err)
					os.Exit(1)
				}
			}
		}
	}
	args := os.Args[1:]
	if len(args) == 0 || args[len(args)-1] == "-" {
		_, _ = os.Stdout.Write([]byte("fixture-pcm"))
		return
	}
	output := args[len(args)-1]
	if err := os.WriteFile(output, []byte("derived"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write fake ffmpeg output: %v", err)
		os.Exit(1)
	}
}
