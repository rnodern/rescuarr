package recoveryservice

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/thcolin/oleoo/packages/go/v3"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/policy"
)

var hebrewDub = regexp.MustCompile(`(?i)(^|[ ._\-])hebdub([ ._\-]|$)`)

// packTitleLanguages treats a release name as supporting evidence. Sonarr's
// language metadata and the identity of each current file are separate reads.
func packTitleLanguages(title string) []string {
	if title == "" || len(title) > 300 || strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return nil
	}
	// Oleoo recognises Hebrew. Normalise this release spelling for parsing
	// while keeping the original title for the Sonarr history comparison.
	normalised := hebrewDub.ReplaceAllString(title, `${1}Hebrew${2}`)
	release, err := oleoo.Parse(normalised, oleoo.Strict(true))
	if err != nil {
		return nil
	}
	if slices.Contains(release.Flags, "DUAL") && !slices.Contains(release.Languages, "MULTi") {
		release.Languages = append(release.Languages, "DUAL")
	}
	return release.Languages
}

func filenameLanguageHints(names ...string) []string {
	var labels []string
	for _, name := range names {
		basename := name[strings.LastIndexAny(name, "/\\")+1:]
		for _, label := range packTitleLanguages(basename) {
			if !slices.Contains(labels, label) {
				labels = append(labels, label)
			}
		}
	}
	return labels
}

func reviewPackLanguage(ctx context.Context, arr *observe.Arr, proof observeservice.RecoveryQueue,
	plan *app.ReplacementPreview, now time.Time, prefs app.LanguagePreferences, mode domain.Mode) *app.PackLanguageReview {
	if proof.Instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion || len(proof.Members) == 0 || len(plan.Episodes) < 2 {
		return nil
	}
	first := proof.Members[0]
	names := packTitleLanguages(first.Release)
	native, matchedGrab := readGrabLanguages(ctx, arr, proof, now)
	agreed := len(names) == 1 && len(native) == 1 && strings.EqualFold(names[0], native[0])
	view := &app.PackLanguageReview{FilenameLanguages: names, SonarrLanguages: native, Agreement: agreed}
	original, _ := arr.OriginalAudioLanguage(ctx, first.Target)
	view.OriginalLanguage = original.Name
	hintMismatch := languageHintMismatch(prefs, original.Name, names) || languageHintMismatch(prefs, original.Name, native)
	if !matchedGrab {
		if len(names) == 0 && len(native) == 0 {
			return nil
		}
		view.NeedsReview = hintMismatch
		return view
	}
	unverified := false
	for _, episode := range plan.Episodes {
		if episode.Status != app.ReplacementInLibrary {
			continue
		}
		file, err := arr.SonarrPackEpisodeFile(ctx, episode.Target, first.DownloadID)
		result := app.PackLanguageFile{Episode: episode.Label, Origin: "unverified"}
		if err == nil {
			switch file.State {
			case observe.PackFileLinked:
				result.Origin = "same_release"
				audio := reviewAudio(prefs, mode, original.Name, "Library file from this download", filenameLanguageHints(first.Release, file.Filename), native,
					file.AudioLanguages, file.AudioLanguagesComplete)
				result.Audio = &audio
				view.NeedsReview = view.NeedsReview || audio.Decision != domain.AudioAccept
			case observe.PackFileDifferentDownload:
				result.Origin = "other_release"
			}
			result.Filename = file.Filename
			result.AudioLanguages = file.AudioLanguages
			result.AudioLanguagesComplete = file.AudioLanguagesComplete
		}
		unverified = unverified || result.Origin == "unverified"
		view.Files = append(view.Files, result)
	}
	view.NeedsReview = view.NeedsReview || hintMismatch && (unverified || len(view.Files) == 0)
	if len(view.Files) == 0 && len(names) == 0 && len(native) == 0 {
		return nil
	}
	return view
}

func grabLanguageLabels(ctx context.Context, arr *observe.Arr, proof observeservice.RecoveryQueue, now time.Time) []string {
	labels, _ := readGrabLanguages(ctx, arr, proof, now)
	return labels
}

func readGrabLanguages(ctx context.Context, arr *observe.Arr, proof observeservice.RecoveryQueue, now time.Time) ([]string, bool) {
	first := proof.Members[0]
	history, err := arr.History(ctx, first.DownloadID, proof.Queue.Evidence.SnapshotGeneration, now)
	if err != nil {
		return nil, false
	}
	var labels []string
	matched := false
	for _, row := range history.Records {
		if row.Event != "grabbed" {
			continue
		}
		if row.SourceTitle != first.Release || row.Target.Kind != first.Target.Kind ||
			(first.Target.Kind == domain.TargetEpisode && row.Target.SeriesID != first.Target.SeriesID) ||
			(first.Target.Kind == domain.TargetMovie && row.Target.ID != first.Target.ID) {
			return nil, false
		}
		matched = true
		for _, language := range row.Languages {
			if !slices.Contains(labels, language.Name) {
				labels = append(labels, language.Name)
			}
		}
	}
	slices.Sort(labels)
	return labels, matched
}

func languageHintMismatch(prefs app.LanguagePreferences, original string, names []string) bool {
	if len(names) != 1 {
		return false
	}
	claimed := domain.AudioLanguageCode(names[0])
	if claimed == "" {
		return false
	}
	originalCode := domain.AudioLanguageCode(original)
	if originalCode == "" {
		return claimed != prefs.PreferredLanguage
	}
	return claimed != originalCode && (prefs.ForeignAudio != domain.ForeignAudioPreferDub || claimed != prefs.PreferredLanguage)
}

func reviewAudio(prefs app.LanguagePreferences, mode domain.Mode, original, label string, filename, release, tracks []string,
	complete bool) app.AudioLanguageReview {
	labels := slices.Clone(tracks)
	for index, tag := range labels {
		if name := domain.AudioLanguageName(domain.AudioLanguageCode(tag)); name != "" {
			labels[index] = name
		}
	}
	return app.AudioLanguageReview{Label: label, OriginalLanguage: original, PreferredLanguage: domain.AudioLanguageName(prefs.PreferredLanguage),
		FilenameLanguages: filename, ReleaseLanguages: release, TrackLanguages: labels, TrackLanguagesComplete: complete,
		AudioAssessment: policy.AssessAudio(prefs, mode, original, tracks, complete)}
}
