package media

import "testing"

func TestClassifySpeechFromSilenceLogUsesSilentDurationInsteadOfEventCount(t *testing.T) {
	tests := []struct {
		name      string
		log       string
		duration  int64
		wantClass string
	}{
		{
			name:      "one long silent region",
			log:       "silence_start: 0\nsilence_end: 8.2 | silence_duration: 8.2",
			duration:  10000,
			wantClass: "mostly_silent",
		},
		{
			name:      "many short regions remain speech candidate",
			log:       "silence_start: 0\nsilence_end: 0.4 | silence_duration: 0.4\nsilence_start: 2\nsilence_end: 2.4 | silence_duration: 0.4\nsilence_start: 4\nsilence_end: 4.4 | silence_duration: 0.4",
			duration:  10000,
			wantClass: "speech_candidate",
		},
		{
			name:      "open silence is clamped to duration",
			log:       "silence_start: 1.5",
			duration:  10000,
			wantClass: "mostly_silent",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classifySpeechFromSilenceLog(tt.log, tt.duration)
			if err != nil {
				t.Fatal(err)
			}
			if got.Classification != tt.wantClass {
				t.Fatalf("classification=%q reason=%s raw=%s", got.Classification, got.Reason, got.RawJSON)
			}
		})
	}
}

func TestClassifySpeechFromSilenceLogRejectsUnknownDuration(t *testing.T) {
	if _, err := classifySpeechFromSilenceLog("silence_start: 0", 0); err == nil {
		t.Fatal("expected duration to be required for a duration-ratio decision")
	}
}
