// Package externalalign implements a Provider that delegates word-level
// alignment to an external command configured by the operator.  The command
// is executed via os/exec and runs with the same OS identity (user, groups)
// as the nexusslate process itself.
package externalalign

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/evjohn-icu/nexusslate/internal/domain"
	"github.com/evjohn-icu/nexusslate/internal/providers/common"
)

type Provider struct {
	Command   string
	Args      []string
	ModelName string
}

func (p *Provider) Name() string { return "external_command" }
func (p *Provider) Model() string {
	if p.ModelName != "" {
		return p.ModelName
	}
	// The aligner command line IS the model identity: two different binaries
	// (or argument sets) produce two different word timelines, and the align
	// job's input hash keys on Name()+Model(). A constant here would mean an
	// operator swapping the aligner keeps the old alignment forever — the job
	// never re-enqueues and GetAlignmentWords keeps serving the stale words.
	h := sha256.Sum256([]byte(p.Command + "\x00" + strings.Join(p.Args, "\x00")))
	return "forced-aligner-" + hex.EncodeToString(h[:8])
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
