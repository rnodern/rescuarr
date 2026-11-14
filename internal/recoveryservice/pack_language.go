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
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
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

func reviewPackLanguage(ctx context.Context, arr *observe.Arr, proof observeservice.RecoveryQueue,
	plan *app.ReplacementPreview, now time.Time) *app.PackLanguageReview {
	if proof.Instance.UpstreamVersion != arrwire.SonarrIDOnlyImportVersion || len(proof.Members) == 0 || len(plan.Episodes) < 2 {
		return nil
	}
	first := proof.Members[0]
	names := packTitleLanguages(first.Release)
	if len(names) != 1 || strings.EqualFold(names[0], "English") || strings.EqualFold(names[0], "Multi") || names[0] == "DUAL" {
		return nil
	}
	view := &app.PackLanguageReview{FilenameLanguages: names}
	history, err := arr.History(ctx, first.DownloadID, proof.Queue.Evidence.SnapshotGeneration, now)
	if err != nil {
		return view
	}
	matched := false
	view.Agreement = true
	for _, row := range history.Records {
		if row.Event != "grabbed" {
			continue
		}
		if row.SourceTitle != first.Release || row.Target.SeriesID != first.Target.SeriesID {
			view.Agreement = false
			continue
		}
		matched = true
		if len(row.Languages) != 1 || !strings.EqualFold(row.Languages[0].Name, names[0]) {
			view.Agreement = false
		}
		for _, lang := range row.Languages {
			if !slices.Contains(view.SonarrLanguages, lang.Name) {
				view.SonarrLanguages = append(view.SonarrLanguages, lang.Name)
			}
		}
	}
	view.Agreement = matched && view.Agreement
	slices.Sort(view.SonarrLanguages)
	if !view.Agreement {
		return view
	}
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
			case observe.PackFileDifferentDownload:
				result.Origin = "other_release"
			}
			result.Filename = file.Filename
			result.AudioLanguages = file.AudioLanguages
			result.AudioLanguagesComplete = file.AudioLanguagesComplete
		}
		view.Files = append(view.Files, result)
	}
	return view
}
