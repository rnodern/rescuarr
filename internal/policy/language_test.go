package policy

import (
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
)

func TestAudioLanguagePolicy(t *testing.T) {
	for _, tt := range []struct {
		name, original string
		tracks         []string
		complete       bool
		mode           domain.Mode
		foreign        domain.ForeignAudioPreference
		unknown        domain.UnknownAudioPreference
		decision       domain.AudioDecision
		preferred      bool
	}{
		{name: "Korean original", original: "Korean", tracks: []string{"kor"}, complete: true, decision: domain.AudioAccept, preferred: true},
		{name: "Korean with English", original: "Korean", tracks: []string{"kor", "eng"}, complete: true, decision: domain.AudioAccept, preferred: true},
		{name: "English dub only", original: "Korean", tracks: []string{"eng"}, complete: true, decision: domain.AudioReject},
		{name: "Hebrew dub of English original", original: "English", tracks: []string{"heb"}, complete: true, decision: domain.AudioReject},
		{name: "English only", original: "English", tracks: []string{"eng", "en-GB"}, complete: true, decision: domain.AudioAccept, preferred: true},
		{name: "English MULTi allowed", original: "English", tracks: []string{"eng", "heb"}, complete: true, decision: domain.AudioAccept},
		{name: "positive partial evidence", original: "Korean", tracks: []string{"kor", "und"}, decision: domain.AudioAccept, preferred: true},
		{name: "unknown companion track", original: "English", tracks: []string{"heb", "und"}, complete: true, decision: domain.AudioReview},
		{name: "aggregate audio tag in Automatic", original: "Korean", tracks: []string{"mul"}, complete: true, mode: domain.ModeAutonomous, decision: domain.AudioReject},
		{name: "unknown original in Manual", original: "", tracks: []string{"eng"}, complete: true, mode: domain.ModeAssist, decision: domain.AudioReview},
		{name: "force unknown through", original: "Korean", unknown: domain.UnknownAudioAccept, decision: domain.AudioAccept},
		{name: "explicit reject in Manual", original: "Korean", unknown: domain.UnknownAudioReject, mode: domain.ModeAssist, decision: domain.AudioReject},
		{name: "review override in Automatic", original: "Korean", unknown: domain.UnknownAudioReview, mode: domain.ModeAutonomous, decision: domain.AudioReview},
		{name: "opt in to dub", original: "Korean", tracks: []string{"eng"}, complete: true, foreign: domain.ForeignAudioPreferDub, decision: domain.AudioAccept, preferred: true},
		{name: "dub preference keeps original fallback", original: "Korean", tracks: []string{"kor"}, complete: true, foreign: domain.ForeignAudioPreferDub, decision: domain.AudioAccept},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prefs := domain.LanguagePreferences{PreferredLanguage: "en", PreferSingleAudio: true, ForeignAudio: domain.ForeignAudioOriginal,
				UnknownAudio: domain.UnknownAudioModeDefault}
			if tt.foreign != "" {
				prefs.ForeignAudio = tt.foreign
			}
			if tt.unknown != "" {
				prefs.UnknownAudio = tt.unknown
			}
			got := AssessAudio(prefs, tt.mode, tt.original, tt.tracks, tt.complete)
			if got.Decision != tt.decision || got.Preferred != tt.preferred || got.Reason == "" {
				t.Fatalf("audio assessment = %+v", got)
			}
		})
	}
}
