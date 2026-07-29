package normalize

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ev/timingdex/internal/domain"
)

func TestValidateAndNormalize_EnumFallback(t *testing.T) {
	a := domain.StructuredAnalysis{
		Summary:      "ok",
		AssetType:    "garbage_type",
		CameraMotion: "quantum_zoom",
		ShotSize:     "ultra_macro",
		AudioType:    "dolphin_speech",
		Quality:      "perfect_but_not_allowed",
	}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AssetType != "other" {
		t.Errorf("AssetType = %q, want %q", got.AssetType, "other")
	}
	if got.CameraMotion != "unknown" {
		t.Errorf("CameraMotion = %q", got.CameraMotion)
	}
	if got.ShotSize != "unknown" {
		t.Errorf("ShotSize = %q", got.ShotSize)
	}
	if got.AudioType != "unknown" {
		t.Errorf("AudioType = %q", got.AudioType)
	}
	if got.Quality != "unknown" {
		t.Errorf("Quality = %q", got.Quality)
	}
}

func TestValidateAndNormalize_ValidEnumPassthrough(t *testing.T) {
	a := domain.StructuredAnalysis{
		Summary:      "valid clip",
		AssetType:    "b_roll",
		CameraMotion: "tracking",
		ShotSize:     "close_up",
		AudioType:    "speech_and_music",
		Quality:      "usable",
	}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AssetType != "b_roll" {
		t.Errorf("AssetType = %q", got.AssetType)
	}
	if got.CameraMotion != "tracking" {
		t.Errorf("CameraMotion = %q", got.CameraMotion)
	}
	if got.ShotSize != "close_up" {
		t.Errorf("ShotSize = %q", got.ShotSize)
	}
	if got.AudioType != "speech_and_music" {
		t.Errorf("AudioType = %q", got.AudioType)
	}
	if got.Quality != "usable" {
		t.Errorf("Quality = %q", got.Quality)
	}
}

func TestValidateAndNormalize_CaseAndWhitespace(t *testing.T) {
	a := domain.StructuredAnalysis{
		Summary:   "ok",
		AssetType: "  B_Roll ",
	}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.AssetType != "b_roll" {
		t.Errorf("AssetType = %q, want %q", got.AssetType, "b_roll")
	}
}

func TestValidateAndNormalize_NegativePeopleCount(t *testing.T) {
	a := domain.StructuredAnalysis{
		Summary:     "ok",
		PeopleCount: -5,
	}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.PeopleCount != 0 {
		t.Errorf("PeopleCount = %d, want 0", got.PeopleCount)
	}
}

func TestValidateAndNormalize_EmptySummary(t *testing.T) {
	tests := []struct {
		name string
		sum  string
	}{
		{"empty", ""},
		{"whitespace", "  \t  "},
	}
	for _, tt := range tests {
		a := domain.StructuredAnalysis{Summary: tt.sum}
		_, err := ValidateAndNormalize(a)
		if err == nil {
			t.Errorf("%s: expected error", tt.name)
		}
	}
}

func TestValidateAndNormalize_SummaryTruncated(t *testing.T) {
	long := strings.Repeat("x", 2500)
	a := domain.StructuredAnalysis{Summary: long}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len([]rune(got.Summary)) != MaxSummaryLength {
		t.Errorf("Summary length = %d runes, want %d", len([]rune(got.Summary)), MaxSummaryLength)
	}
}

func TestValidateAndNormalize_EditorialReasonTruncated(t *testing.T) {
	long := strings.Repeat("y", 3000)
	a := domain.StructuredAnalysis{Summary: "ok", EditorialReason: long}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len([]rune(got.EditorialReason)) != MaxSummaryLength {
		t.Errorf("EditorialReason length = %d runes, want %d", len([]rune(got.EditorialReason)), MaxSummaryLength)
	}
}

func TestValidateAndNormalize_SummaryChineseRuneBoundary(t *testing.T) {
	cjk := "你好世界" // 4 runes, 12 bytes
	repeat := strings.Repeat(cjk, 600)
	if len([]rune(repeat)) < MaxSummaryLength+100 {
		t.Fatal("test string too short")
	}
	a := domain.StructuredAnalysis{Summary: repeat}
	got, err := ValidateAndNormalize(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rs := []rune(got.Summary)
	if len(rs) != MaxSummaryLength {
		t.Errorf("Summary rune count = %d, want %d", len(rs), MaxSummaryLength)
	}
	// must be valid UTF-8
	if !utf8Valid(got.Summary) {
		t.Error("Summary is not valid UTF-8")
	}
}

func TestClean(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"dedup", []string{"a", "b", "a", "c"}, []string{"a", "b", "c"}},
		{"empty removal", []string{"a", "", "b", "  "}, []string{"a", "b"}},
		{"lowercase", []string{"A", "B"}, []string{"a", "b"}},
		{"all empty", []string{"", "", ""}, nil},
		{"nil input", nil, nil},
	}
	for _, tt := range tests {
		got := clean(tt.in)
		if !stringSliceEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestClean_TagLengthCapped(t *testing.T) {
	ok := strings.Repeat("a", MaxTagLength)
	long := strings.Repeat("b", MaxTagLength+50)
	in := []string{ok, long}
	got := clean(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if len([]rune(got[0])) != MaxTagLength {
		t.Errorf("short tag rune count = %d", len([]rune(got[0])))
	}
	if len([]rune(got[1])) != MaxTagLength {
		t.Errorf("truncated tag rune count = %d, want %d", len([]rune(got[1])), MaxTagLength)
	}
}

func TestClean_ChineseTagRuneBoundary(t *testing.T) {
	cjk := "文" // 1 rune, 3 bytes
	long := strings.Repeat(cjk, MaxTagLength+10)
	got := clean([]string{long})
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	rs := []rune(got[0])
	if len(rs) != MaxTagLength {
		t.Errorf("rune count = %d, want %d", len(rs), MaxTagLength)
	}
	if !utf8Valid(got[0]) {
		t.Error("tag is not valid UTF-8")
	}
}

func TestClean_MaxTagsPerList(t *testing.T) {
	in := make([]string, 500)
	for i := range in {
		in[i] = fmt.Sprintf("tag_%d", i)
	}
	got := clean(in)
	if len(got) != MaxTagsPerList {
		t.Errorf("got %d tags, want %d", len(got), MaxTagsPerList)
	}
}

func TestClean_DedupAfterTruncation(t *testing.T) {
	prefix := strings.Repeat("a", MaxTagLength)
	suffix1 := "xxx"
	suffix2 := "yyy"
	got := clean([]string{prefix + suffix1, prefix + suffix2})
	if len(got) != 1 {
		t.Errorf("expected dedup (both truncated to same %d runes), got %d: %v", MaxTagLength, len(got), got)
	}
}

func TestClean_DifferentAfterTruncation(t *testing.T) {
	a := strings.Repeat("a", MaxTagLength)
	b := strings.Repeat("b", MaxTagLength)
	got := clean([]string{a, b})
	if len(got) != 2 {
		t.Errorf("expected 2 distinct entries after truncation, got %d: %v", len(got), got)
	}
}

func TestNormList(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"all valid", []string{"hook", "opening"}, []string{"hook", "opening"}},
		{"filters unknown", []string{"hook", "nonsense"}, []string{"hook"}},
		{"dedup", []string{"hook", "hook"}, []string{"hook"}},
		{"case insensitive", []string{"HOOK"}, []string{"hook"}},
		{"whitespace", []string{"  hook  "}, []string{"hook"}},
		{"all bad", []string{"bad1", "bad2"}, nil},
		{"nil input", nil, nil},
	}
	set := allowedUse
	for _, tt := range tests {
		got := normList(tt.in, set)
		if !stringSliceEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNormList_MaxTagsPerList(t *testing.T) {
	// build a custom set with MaxTagsPerList+1 unique entries
	bigSet := map[string]bool{}
	in := make([]string, 0, MaxTagsPerList+1)
	for i := 0; i <= MaxTagsPerList; i++ {
		k := fmt.Sprintf("v_%d", i)
		bigSet[k] = true
		in = append(in, k)
	}
	got := normList(in, bigSet)
	if len(got) > MaxTagsPerList {
		t.Errorf("got %d, want <= %d", len(got), MaxTagsPerList)
	}
	if len(got) != MaxTagsPerList {
		t.Errorf("got %d, want exactly %d", len(got), MaxTagsPerList)
	}
}

func TestNormList_EmptySet(t *testing.T) {
	set := map[string]bool{}
	got := normList([]string{"x", "y"}, set)
	if len(got) != 0 {
		t.Errorf("expected 0, got %d", len(got))
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func utf8Valid(s string) bool {
	return utf8.ValidString(s)
}
