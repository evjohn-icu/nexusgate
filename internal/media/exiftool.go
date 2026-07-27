package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

func ReadExif(ctx context.Context, path string) (map[string]any, error) {
	command := exec.CommandContext(ctx, "exiftool", "-json", "-n", path)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("exiftool: %w", err)
	}
	var values []map[string]any
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, fmt.Errorf("decode exiftool result: %w", err)
	}
	if len(values) == 0 {
		return map[string]any{}, nil
	}
	return values[0], nil
}
