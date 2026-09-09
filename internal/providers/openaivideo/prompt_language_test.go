package openaivideo

import (
	"strings"
	"testing"

	videoanalysis "github.com/evjohn-icu/nexusgate/internal/domain/video_analysis"
)

// This is the hop where the configured language stops being a value being
// passed around and starts being an instruction. Everything upstream of it can
// be wired correctly and the model still answers in English if the prompt never
// mentions the language — and that failure is invisible to every other test,
// because a prompt is a string and a string always builds.
func TestUnifiedPromptCarriesTheConfiguredLanguage(t *testing.T) {
	with := unifiedPrompt(videoanalysis.Input{Language: "zh-CN"})
	if !strings.Contains(with, "简体中文") {
		t.Fatalf("prompt does not ask for Chinese even though Input.Language was zh-CN:\n%s", with)
	}

	without := unifiedPrompt(videoanalysis.Input{})
	if strings.Contains(without, "简体中文") || strings.Contains(without, "Write the free-text fields") {
		t.Fatalf("prompt asks for a language nobody configured:\n%s", without)
	}
	// The unconfigured prompt must otherwise be the prompt it always was: the
	// language clause is an addition, not a rewrite, so an install that sets
	// nothing gets byte-identical behaviour to before this existed.
	if !strings.Contains(without, "Describe only observable footage.") {
		t.Fatalf("the language change altered the base prompt:\n%s", without)
	}
}

// The vocabulary rule and the language rule are adjacent in the prompt and pull
// in opposite directions. Order matters: the language clause has to come after
// the value lists it exempts, or it reads as an instruction to translate them.
func TestLanguageClauseFollowsTheVocabularyRule(t *testing.T) {
	prompt := unifiedPrompt(videoanalysis.Input{Language: "zh-CN"})
	vocab := strings.Index(prompt, "spelled exactly as shown")
	lang := strings.Index(prompt, "Write the free-text fields")
	if vocab < 0 || lang < 0 {
		t.Fatalf("prompt is missing one of the two rules (vocab=%d lang=%d)", vocab, lang)
	}
	if lang < vocab {
		t.Fatal("the language clause precedes the controlled-vocabulary values it exempts; " +
			"read in that order it tells the model to translate them, and a translated enum is a permanent failure")
	}
}
