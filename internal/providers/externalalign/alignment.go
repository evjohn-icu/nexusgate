// Package externalalign implements a Provider that delegates word-level
// alignment to an external command configured by the operator.  The command
// is executed via os/exec and runs with the same OS identity (user, groups)
// as the timingdex process itself.
package externalalign

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/evjohn-icu/timingdex/internal/domain"
	"github.com/evjohn-icu/timingdex/internal/providers/common"
)

type Provider struct {
	Command   string
	Args      []string
	ModelName string
}

func (p *Provider) Name() string { return "external_command" }
func (p *Provider) Model() string {
	if p.ModelName == "" {
		return "forced-aligner"
	}
	return p.ModelName
}

func (p *Provider) Align(ctx context.Context, req common.AlignRequest) (domain.AlignmentResult, error) {
	if p.Command == "" {
		return domain.AlignmentResult{}, fmt.Errorf("alignment command is empty")
	}
	payload, err := json.Marshal(map[string]any{
		"audio_path": req.AudioPath,
		"language":   req.Language,
		"text":       req.Transcript.Text,
		"segments":   req.Transcript.Segments,
	})
	if err != nil {
		return domain.AlignmentResult{}, err
	}
	cmd := exec.CommandContext(ctx, p.Command, p.Args...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return domain.AlignmentResult{}, fmt.Errorf("aligner failed: %w: %s", err, stderr.String())
	}
	var out struct {
		Words []domain.AlignmentWord `json:"words"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return domain.AlignmentResult{}, fmt.Errorf("decode aligner output: %w", err)
	}
	if len(out.Words) == 0 {
		return domain.AlignmentResult{}, fmt.Errorf("aligner returned no words")
	}
	return domain.AlignmentResult{Words: out.Words, RawResponse: stdout.String()}, nil
}
