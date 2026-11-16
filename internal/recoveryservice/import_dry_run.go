package recoveryservice

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const (
	importDryRunPageSize    = 5
	importDryRunCaseTimeout = 8 * time.Second
)

var _ app.ImportDryRunReader = (*ManualReviews)(nil)

// ImportDryRun evaluates held cases without scheduling or recording actions.
func (s *ManualReviews) ImportDryRun(ctx context.Context, after string) (app.ImportDryRunPage, error) {
	select {
	case s.dryRunSlot <- struct{}{}:
		defer func() { <-s.dryRunSlot }()
	default:
		return app.ImportDryRunPage{}, app.ErrConflict
	}
	page, err := s.cases.Cases(ctx, app.CaseQuery{State: domain.CaseHeld, Limit: importDryRunPageSize, After: after})
	if err != nil {
		return app.ImportDryRunPage{}, err
	}
	result := app.ImportDryRunPage{Next: page.After}
	for _, item := range page.Cases {
		if ctx.Err() != nil {
			return app.ImportDryRunPage{}, ctx.Err()
		}
		check, cancel := context.WithTimeout(ctx, importDryRunCaseTimeout)
		review, err := s.checkDryRunCase(check, item)
		cancel()
		if err != nil {
			return app.ImportDryRunPage{}, err
		}
		result.Cases = append(result.Cases, review)
	}
	return result, nil
}

func (s *ManualReviews) checkDryRunCase(ctx context.Context, item domain.Case) (app.ImportDryRunCase, error) {
	result := app.ImportDryRunCase{Case: item, Assessment: "Needs review"}
	if item.Display.Product != domain.ProductSonarr || item.Display.Version != arrwire.SonarrIDOnlyImportVersion {
		result.Assessment, result.Reason = "Outside scope", "This check covers the tested Sonarr 5.0.0.2941 import."
		return result, nil
	}
	if item.Reason != domain.ReasonCode(observe.FindingSeriesIDOnly) {
		result.Assessment, result.Reason = "Outside scope", "This hold needs another recovery path."
		return result, nil
	}
	switch {
	case len(item.Incidents) > 0:
		result.Reason = "An active incident is affecting this download."
		return result, nil
	case item.Job.Shared:
		result.Reason = "Sonarr flagged this download as shared."
		return result, nil
	case len(item.Targets) != 1:
		result.Reason = "This download needs one identified episode for automatic import."
		return result, nil
	case item.Job.Protocol != domain.ProtocolUsenet:
		result.Assessment, result.Reason = "Outside scope", "The tested import applies to Usenet downloads."
		return result, nil
	}

	var reviewState string
	var attempted bool
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE((SELECT state FROM manual_reviews WHERE case_id = ?), ''),
				EXISTS (SELECT 1 FROM manual_import_attempts WHERE case_id = ?)
				OR EXISTS (SELECT 1 FROM action_intents
					WHERE json_extract(proposal, '$.CaseID') = ? AND state <> 'cancelled')`, item.ID, item.ID, item.ID).
			Scan(&reviewState, &attempted)
	})
	if err != nil {
		return result, err
	}
	switch {
	case reviewState == "declined":
		result.Reason = "You declined this download in Manual Review."
		return result, nil
	case attempted || reviewState == "approved" || reviewState == "completed":
		result.Reason = "This case already has a recovery decision or attempt."
		return result, nil
	}

	plan, err := prepareManualImport(ctx, s.cases, s.local, s.clock, s.transport, item.ID, item.Revision, s.importAudio)
	result.Reason = s.automaticEvidenceHold(ctx, item, plan, err)
	if result.Reason != "" {
		return result, nil
	}

	if s.importQualified == nil {
		result.Reason = "Configure the tested Sonarr import profile for this connection."
		return result, nil
	}
	unlock, err := s.cases.HoldRecoveryConnection(ctx, plan.instance)
	if err != nil {
		result.Reason = "Sonarr’s connection changed during the check. Run the dry run again."
		return result, nil
	}
	defer unlock()
	effects, err := s.importQualified(ctx, plan.instance, plan.source)
	switch {
	case errors.Is(err, arradapter.ErrQualification), err == nil && effects == nil:
		result.Reason = "Rescuarr could not verify the pinned Sonarr import profile for this connection."
		return result, nil
	case err != nil:
		result.Reason = "Rescuarr could not read the current Sonarr import profile."
		return result, nil
	}
	switch effects.AdapterRevision() {
	case arrwire.SonarrV5CleanupCopyRevision:
		result.Reason = "The file, audio and available library space passed. Sonarr manages source cleanup after import."
	case arradapter.ManualSonarrV5CopyRevision:
		result.Reason = "The file, audio and available library space passed with the source-preserving profile."
	default:
		result.Reason = "This connection did not return a tested Sonarr Copy profile."
		return result, nil
	}
	result.Candidate, result.Assessment = true, "Candidate"
	return result, nil
}

func (s *ManualReviews) automaticEvidenceHold(ctx context.Context, item domain.Case, plan manualImportPlan, err error) string {
	if episode := plan.preview.Episode; episode != nil {
		if !episode.Matched && episode.Reason != "" {
			return episode.Reason
		}
		if episode.Matched && !episode.TitleInNames {
			order, orderErr := s.cases.EpisodeOrder(ctx, item)
			if orderErr != nil || !order.Available {
				return "Rescuarr could not confirm the episode order setting for this series."
			}
			if order.RequireTitle {
				return "This series requires a matching episode title. Confirm which episode order the release uses."
			}
		}
	}
	if len(plan.preview.Audio) == 1 {
		audio := plan.preview.Audio[0]
		switch audio.Reason {
		case "original_audio_absent":
			return "The tagged tracks do not include the original audio required by your policy."
		case "audio_unverified":
			return "Audio unverified. Listen to the file before approving it in Manual Review."
		}
	}
	if plan.preview.Hold != "" {
		return plan.preview.Hold
	}
	if errors.Is(err, app.ErrConflict) {
		return "The queue or episode changed during the check. Run the dry run again."
	}
	if errors.Is(err, app.ErrHeld) {
		return "A recent complete queue is required. Check the Sonarr connection, then run the dry run again."
	}
	if err != nil || plan.preview.Episode == nil || !plan.preview.Episode.Matched || len(plan.preview.Audio) != 1 ||
		!plan.preview.Audio[0].TrackLanguagesComplete || plan.preview.Audio[0].Decision != domain.AudioAccept {
		return "Rescuarr could not confirm the current download and episode in Sonarr."
	}
	root := plan.libraryRoot
	switch {
	case root.Accessible != nil && !*root.Accessible:
		return "Sonarr reports that the destination library folder is unavailable."
	case root.Accessible == nil || root.FreeSpace == nil || *root.FreeSpace < 0:
		return "Sonarr did not report available space for the destination library folder."
	case *root.FreeSpace < plan.source.Size:
		return "The destination library folder has less free space than the selected file needs."
	}
	return ""
}
