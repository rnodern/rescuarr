package policy

import "github.com/rnodern/rescuarr/internal/domain"

// AssessAudio uses the original language and observed audio streams. A title
// or a release language label cannot supply an audio track or prove its absence.
func AssessAudio(prefs domain.LanguagePreferences, mode domain.Mode, original string, tracks []string, complete bool) domain.AudioAssessment {
	original = domain.AudioLanguageCode(original)
	if original == "" {
		return uncertainAudio(prefs.UnknownAudio, mode)
	}
	preferred := domain.AudioLanguageCode(prefs.PreferredLanguage)
	originalPresent, preferredPresent := false, false
	onlyOriginal := len(tracks) > 0
	for _, track := range tracks {
		code := domain.AudioLanguageCode(track)
		complete = complete && code != ""
		onlyOriginal = onlyOriginal && code == original
		originalPresent = originalPresent || code == original
		preferredPresent = preferredPresent || preferred != "" && code == preferred
	}
	if prefs.ForeignAudio == domain.ForeignAudioPreferDub && preferred != original && preferredPresent {
		return domain.AudioAssessment{Decision: domain.AudioAccept, Reason: "preferred_audio_present", Preferred: true}
	}
	if originalPresent {
		return domain.AudioAssessment{Decision: domain.AudioAccept, Reason: "original_audio_present",
			Preferred: (preferred != original && prefs.ForeignAudio != domain.ForeignAudioPreferDub) ||
				(preferred == original && (!prefs.PreferSingleAudio || complete && onlyOriginal))}
	}
	if complete && len(tracks) > 0 {
		return domain.AudioAssessment{Decision: domain.AudioReject, Reason: "original_audio_absent"}
	}
	return uncertainAudio(prefs.UnknownAudio, mode)
}

func uncertainAudio(preference domain.UnknownAudioPreference, mode domain.Mode) domain.AudioAssessment {
	decision := domain.AudioReview
	switch {
	case preference == domain.UnknownAudioAccept:
		decision = domain.AudioAccept
	case preference == domain.UnknownAudioReject || preference == domain.UnknownAudioModeDefault && mode == domain.ModeAutonomous:
		decision = domain.AudioReject
	}
	return domain.AudioAssessment{Decision: decision, Reason: "audio_unverified"}
}
