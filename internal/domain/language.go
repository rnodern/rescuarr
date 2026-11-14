package domain

type ForeignAudioPreference string
type UnknownAudioPreference string

const (
	ForeignAudioOriginal  ForeignAudioPreference = "original"
	ForeignAudioPreferDub ForeignAudioPreference = "prefer_dub"

	UnknownAudioModeDefault UnknownAudioPreference = "mode_default"
	UnknownAudioReview      UnknownAudioPreference = "review"
	UnknownAudioAccept      UnknownAudioPreference = "accept"
	UnknownAudioReject      UnknownAudioPreference = "reject"
)

type LanguagePreferences struct {
	PreferredLanguage string
	ForeignAudio      ForeignAudioPreference
	UnknownAudio      UnknownAudioPreference
	PreferSingleAudio bool
	Revision          Revision
}

type AudioDecision string

const (
	AudioAccept AudioDecision = "accept"
	AudioReview AudioDecision = "review"
	AudioReject AudioDecision = "reject"
)

type AudioAssessment struct {
	Decision  AudioDecision
	Reason    ReasonCode
	Preferred bool
}
