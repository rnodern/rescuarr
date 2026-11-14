package recoveryservice

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

func (s *PreviewService) previewMissingFiles(ctx context.Context, id domain.CaseID, revision domain.Revision, proof observeservice.RecoveryQueue) (app.ImportPreview, error) {
	seriesID, ids, ok := missingFileTargets(proof)
	if !ok {
		return heldReplacement("Wait for Sonarr to identify all episodes and report that the completed download has no importable files.")
	}
	release, err := s.cases.HoldRecoveryConnection(ctx, proof.Instance)
	if err != nil {
		return app.ImportPreview{}, err
	}
	defer release()

	key, err := s.secrets.ReadSecret(proof.Instance.SecretRef)
	if err != nil {
		return heldReplacement("The connection’s API key is unavailable. Check the service in Connections.")
	}
	arr, err := observe.NewArr(observe.Config{Instance: proof.Instance, APIKey: key}, s.transport)
	if err != nil {
		return app.ImportPreview{}, err
	}
	needs, err := arr.SonarrEpisodeNeeds(ctx, seriesID, ids, s.clock.Now())
	if errors.Is(err, observe.ErrTargetVersion) {
		return heldReplacement("This Sonarr version is unavailable for replacement checks.")
	}
	if errors.Is(err, observe.ErrNotFound) || errors.Is(err, observe.ErrResponse) || errors.Is(err, observe.ErrPartial) {
		return heldReplacement("Sonarr could not confirm the current status of every episode in this download.")
	}
	if err != nil {
		return app.ImportPreview{}, err
	}
	// Recheck queue membership after reading the episodes so another download
	// already picked up by Sonarr can suppress an unnecessary search.
	current, err := s.cases.FreshRecoveryQueue(ctx, id, revision)
	if errors.Is(err, app.ErrHeld) || errors.Is(err, observe.ErrPartial) {
		return heldReplacement("Sonarr’s current queue coverage is incomplete. Run the check again before removing the download.")
	}
	if err != nil {
		return app.ImportPreview{}, err
	}
	if current.Instance != proof.Instance {
		return app.ImportPreview{}, app.ErrConflict
	}
	newSeries, newIDs, ok := missingFileTargets(current)
	if !ok || newSeries != seriesID || !slices.Equal(newIDs, ids) ||
		current.Members[0].DownloadID != proof.Members[0].DownloadID || current.Members[0].Release != proof.Members[0].Release {
		return app.ImportPreview{}, app.ErrConflict
	}
	return app.ImportPreview{Replacement: replacementPlan(current, needs)}, nil
}

func missingFileTargets(proof observeservice.RecoveryQueue) (int64, []int64, bool) {
	if len(proof.Members) == 0 || len(proof.Case.Targets) == 0 {
		return 0, nil, false
	}
	first := proof.Members[0]
	series := first.Target.SeriesID
	if first.Release == "" || first.Release != proof.Case.Display.Release {
		return 0, nil, false
	}
	mapped := make(map[domain.TargetRef]bool, len(proof.Case.Targets))
	for _, row := range proof.Members {
		if !row.Target.Valid() || row.Target.Kind != domain.TargetEpisode || row.Target.SeriesID != series ||
			!completedWithoutEligibleFiles(row) || row.Release != first.Release {
			return 0, nil, false
		}
		mapped[row.Target] = true
	}
	if len(mapped) != len(proof.Case.Targets) {
		return 0, nil, false
	}
	ids := make([]int64, 0, len(mapped))
	for _, target := range proof.Case.Targets {
		if !mapped[target.Ref] {
			return 0, nil, false
		}
		delete(mapped, target.Ref)
		ids = append(ids, target.Ref.ID)
	}
	return series, ids, true
}

func completedWithoutEligibleFiles(row observe.QueueRecord) bool {
	return row.Status == "completed" && (row.TrackedState == "importPending" || row.TrackedState == "importBlocked") &&
		slices.Contains(row.Findings, observe.FindingNoEligibleFiles)
}

func replacementPlan(proof observeservice.RecoveryQueue, needs map[int64]observe.EpisodeNeed) *app.ReplacementPreview {
	first := proof.Members[0]
	otherDownloads := make(map[domain.TargetRef]bool)
	unclear := false
	for _, row := range proof.Queue.Records {
		if row.DownloadID == first.DownloadID || row.Status == "failed" || row.TrackedState == "downloadFailed" ||
			completedWithoutEligibleFiles(row) {
			continue
		}
		if row.Target.Valid() {
			otherDownloads[row.Target] = true
		} else if row.SeriesID == 0 || row.SeriesID == first.Target.SeriesID {
			unclear = true
		}
	}
	labels := make(map[domain.TargetRef]string, len(proof.Case.Display.Media))
	for _, media := range proof.Case.Display.Media {
		label := media.Episode
		if label != "" && media.Title != "" {
			label += " · " + media.Title
		}
		labels[media.Target] = label
	}
	result := &app.ReplacementPreview{Release: first.Release}
	for _, target := range proof.Case.Targets {
		ref := target.Ref
		label := labels[ref]
		if label == "" {
			label = fmt.Sprintf("Episode #%d", ref.ID)
		}
		episode := app.ReplacementEpisode{Target: ref, Label: label}
		switch needs[ref.ID] {
		case observe.EpisodeInLibrary:
			episode.Status = app.ReplacementInLibrary
		case observe.EpisodeUnmonitored:
			episode.Status = app.ReplacementUnmonitored
		case observe.EpisodeUnaired:
			episode.Status = app.ReplacementUnaired
		case observe.EpisodeMissing:
			switch {
			case otherDownloads[ref]:
				episode.Status = app.ReplacementOtherDownload
			case !unclear:
				episode.Status = app.ReplacementSearch
				result.SearchCount++
			}
		}
		if episode.Status == "" {
			episode.Status = app.ReplacementUnclear
			result.UnclearCount++
		}
		result.Episodes = append(result.Episodes, episode)
	}
	return result
}

func heldReplacement(reason string) (app.ImportPreview, error) {
	return app.ImportPreview{Hold: reason, Replacement: &app.ReplacementPreview{}}, app.ErrHeld
}
