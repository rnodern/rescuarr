package recoveryservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/store"
)

// ManualImportEffects is the qualified Sonarr Copy route. Readback is independent of acknowledgement.
type ManualImportEffects interface {
	DispatchManualImport(context.Context, domain.Operation, domain.PreparedAction, func(context.Context) error) (domain.Acknowledgement, error)
	Reconcile(context.Context, domain.Attempt) (domain.Outcome, error)
	AdapterRevision() string
}

type ManualImportQualification func(context.Context, domain.Instance, importflow.SonarrV5Source) (ManualImportEffects, error)

// SetManualImportQualification registers the Copy route during application startup.
func (s *ManualReviews) SetManualImportQualification(qualify ManualImportQualification) {
	s.importQualified = qualify
}

func (s *ManualReviews) manualImportView(ctx context.Context, slot manualSlot, view app.ManualReviewView) app.ManualReviewView {
	item := view.Case
	view.Diagnosis = "Sonarr identified the series from the grab history. The episode needs review before import."
	view.Hold = "Approval requires a verified Sonarr import connection."
	if s.importQualified != nil {
		plan, err := prepareManualImport(ctx, s.cases, s.local, s.clock, s.transport, item.ID, item.Revision)
		if plan.preview.Episode != nil || len(plan.preview.Audio) > 0 {
			view.Preview = plan.preview
		}
		if plan.preview.Hold != "" {
			view.Hold = plan.preview.Hold
			return view
		}
		if err == nil {
			if importAudioRejected(plan.preview) {
				view.Hold = "The audio policy rejected this file. Review the language evidence before importing."
				return view
			}
			if _, err := s.qualifyImportApproval(ctx, plan); err == nil {
				view.Hold, view.CanApprove, view.ImportAction = "", true, true
				view.ApprovalToken = s.approvalToken(slot.token, plan.scopeHash)
			}
			return view
		}
		view.Hold = "The current download, episode or file could not be confirmed. Refresh to check again or decline to move on."
	}
	if view.Preview.Episode == nil {
		if preview, err := s.preview.ImportPreview(ctx, item.ID, item.Revision); err == nil || errors.Is(err, app.ErrHeld) {
			view.Preview = preview
		}
	}
	if view.Preview.Episode == nil && item.Display.EpisodeCheck != nil {
		view.Preview.Episode = &item.Display.EpisodeCheck.EpisodeNameReview
	}
	return view
}

func importAudioRejected(preview app.ImportPreview) bool {
	for _, audio := range preview.Audio {
		if audio.Decision == domain.AudioReject {
			return true
		}
	}
	return false
}

func (s *ManualReviews) qualifyImportApproval(ctx context.Context, plan manualImportPlan) (ManualImportEffects, error) {
	if s.importQualified == nil || plan.preview.Episode == nil || !plan.preview.Episode.Matched ||
		len(plan.preview.Audio) != 1 || importAudioRejected(plan.preview) {
		return nil, app.ErrHeld
	}
	if err := s.manualApprovalAllowed(ctx); err != nil {
		return nil, err
	}
	effects, err := s.importQualified(ctx, plan.instance, plan.source)
	if err == nil && effects == nil {
		err = arradapter.ErrQualification
	}
	return effects, err
}

func (s *ManualReviews) manualImportProcessing(ctx context.Context, slot manualSlot, view app.ManualReviewView) (app.ManualReviewView, error) {
	view.ImportAction = true
	view.Hold = "Waiting for Sonarr to confirm the approved Copy import."
	record, err := s.loadManualImport(ctx, slot)
	if err == nil && record.stage == "uncertain" {
		view.Hold = "The Copy import outcome is uncertain. Check Sonarr’s Activity and library before retrying."
		view.CanCloseUncertain = true
	}
	return view, err
}

// RunManualImportRecovery processes only the currently approved Manual Review.
// A committed send reservation can be read back after a restart, never sent again.
func (s *ManualReviews) RunManualImportRecovery(ctx context.Context, onError func(error)) error {
	if s.importQualified == nil {
		return arradapter.ErrQualification
	}
	return runRecoveryPasses(ctx, s.manualImportStep, onError)
}

type manualImportApproval struct {
	manualSlot
	revision   domain.Revision
	scope      string
	approvedAt time.Time
}

func (s *ManualReviews) manualImportStep(ctx context.Context) error {
	var approval manualImportApproval
	var updated string
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT m.case_id, m.token, m.case_revision, m.scope_hash, m.updated_at
			FROM manual_reviews AS m JOIN app_state AS s ON s.singleton = 1
			JOIN action_pause AS p ON p.singleton = 1
			WHERE m.state = 'approved' AND m.action = 'import' AND (
				EXISTS (SELECT 1 FROM manual_import_attempts AS a WHERE a.token = m.token AND a.stage IN ('sending', 'uncertain'))
				OR (s.mode = 'assist' AND NOT s.reconcile_required AND NOT p.paused))`).
			Scan(&approval.id, &approval.token, &approval.revision, &approval.scope, &updated)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	record, err := s.loadManualImport(ctx, approval.manualSlot)
	if err != nil {
		return err
	}
	if record.stage != "" {
		if record.stage == "sending" && s.clock.Now().Sub(record.Attempt.SentAt) >= 2*time.Minute {
			return s.recordManualImport(ctx, approval.manualSlot, record, "uncertain", "recovery_manual_import_uncertain")
		}
		return s.reconcileManualImport(ctx, approval.manualSlot, record)
	}
	approval.approvedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil || s.clock.Now().Sub(approval.approvedAt) >= manualApprovalWindow {
		return resetManualReview(ctx, s.local, approval.id, approval.token, s.clock.Now())
	}
	err = s.sendManualImport(ctx, approval)
	if errors.Is(err, app.ErrConflict) || errors.Is(err, app.ErrHeld) || errors.Is(err, app.ErrNotFound) ||
		errors.Is(err, arradapter.ErrQualification) || errors.Is(err, importflow.ErrSonarrV5Qualification) ||
		errors.Is(err, importflow.ErrSonarrV5Candidate) || errors.Is(err, importflow.ErrStale) || errors.Is(err, importflow.ErrInvalid) {
		return resetManualReview(ctx, s.local, approval.id, approval.token, s.clock.Now())
	}
	return err
}

func (s *ManualReviews) sendManualImport(ctx context.Context, approval manualImportApproval) error {
	plan, err := prepareManualImport(ctx, s.cases, s.local, s.clock, s.transport, approval.id, approval.revision)
	if err != nil {
		return err
	}
	if plan.scopeHash != approval.scope {
		return app.ErrConflict
	}
	release, err := s.cases.HoldRecoveryConnection(ctx, plan.instance)
	if err != nil {
		return err
	}
	defer release()
	effects, err := s.qualifyImportApproval(ctx, plan)
	if err != nil {
		return err
	}
	key, err := s.local.ReadSecret(plan.instance.SecretRef)
	if err != nil {
		return err
	}
	client, err := importflow.New(observe.Config{Instance: plan.instance, APIKey: key}, s.transport)
	if err != nil {
		return err
	}
	prepared, err := client.ReprocessSonarrV5(ctx, plan.job, s.clock.Now(), func(_ context.Context, source importflow.SonarrV5Source) error {
		if source != plan.source {
			return app.ErrConflict
		}
		return nil
	})
	if err != nil {
		return err
	}
	current, err := prepareManualImport(ctx, s.cases, s.local, s.clock, s.transport, approval.id, approval.revision)
	if err != nil {
		return err
	}
	if current.scopeHash != approval.scope || current.source != plan.source {
		return app.ErrConflict
	}
	if s.clock.Now().Sub(approval.approvedAt) >= manualApprovalWindow {
		return app.ErrConflict
	}
	record := newManualImportCheckpoint(current, prepared, effects.AdapterRevision(), s.clock.Now())
	if err := s.reserveManualImport(ctx, approval, current, record); err != nil {
		return err
	}
	ack, err := effects.DispatchManualImport(ctx, record.Attempt.Operation, record.Prepared, func(ctx context.Context) error {
		return s.allowManualImportSend(ctx, approval, current, record.Attempt.ID)
	})
	record.Attempt.Acknowledgement = ack
	switch {
	case errors.Is(err, arradapter.ErrQualification) || errors.Is(err, arradapter.ErrConfiguration):
		return s.recordManualImport(ctx, approval.manualSlot, record, "failed", "recovery_manual_import_failed")
	case err != nil || ack.State != domain.DispatchAcknowledged:
		return s.recordManualImport(ctx, approval.manualSlot, record, "uncertain", "recovery_manual_import_uncertain")
	default:
		return s.recordManualImport(ctx, approval.manualSlot, record, "sending", "")
	}
}

// The private payload survives a process failure between dispatch and readback.
// Paths and downloader IDs are never projected into Activity or approval forms.
type manualImportCheckpoint struct {
	stage      string
	Attempt    domain.Attempt
	Prepared   domain.PreparedAction
	Outcome    *domain.Outcome
	Client     string
	OutputPath string
	Size       int64
}

func newManualImportCheckpoint(plan manualImportPlan, prepared domain.PreparedImport, adapter string, now time.Time) manualImportCheckpoint {
	target := plan.source.Target
	operation := domain.Operation{
		Key: domain.OperationKey(store.NewID()), Job: plan.source.Job, Targets: []domain.TargetRef{target},
		Action: domain.ActionImport, Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}},
	}
	return manualImportCheckpoint{stage: "sending",
		Attempt: domain.Attempt{ID: domain.AttemptID(store.NewID()), Operation: operation, SentAt: now},
		Prepared: domain.PreparedAction{Product: plan.instance.Product, Version: plan.instance.UpstreamVersion,
			ConnectionRevision: plan.instance.ConnectionRevision, AdapterRevision: adapter, OperationKey: operation.Key, Import: &prepared},
		Client: plan.source.Client, OutputPath: plan.source.OutputPath, Size: plan.source.Size}
}

func (s *ManualReviews) loadManualImport(ctx context.Context, slot manualSlot) (manualImportCheckpoint, error) {
	var record manualImportCheckpoint
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		var stage, payload string
		err := tx.QueryRowContext(ctx, `SELECT stage, payload FROM manual_import_attempts WHERE case_id = ? AND token = ?`,
			slot.id, slot.token).Scan(&stage, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return err
		}
		record.stage = stage
		return nil
	})
	return record, err
}

func (s *ManualReviews) reserveManualImport(ctx context.Context, approval manualImportApproval, plan manualImportPlan, record manualImportCheckpoint) error {
	if !record.Prepared.Binds(record.Attempt.Operation) || !plan.job.Queue.Evidence.Fresh(s.clock.Now()) {
		return app.ErrConflict
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `INSERT INTO manual_import_attempts (case_id, token, stage, payload)
			SELECT m.case_id, m.token, 'sending', ?
			FROM manual_reviews AS m JOIN observe_cases AS c ON c.id = m.case_id AND c.revision = m.case_revision
			JOIN observe_instances AS i ON i.id = c.instance_id AND i.epoch = c.epoch
			JOIN app_state AS s ON s.singleton = 1 JOIN action_pause AS p ON p.singleton = 1
			WHERE m.case_id = ? AND m.token = ? AND m.state = 'approved' AND m.action = 'import' AND m.scope_hash = ?
			AND c.state = 'held' AND c.reason = 'series_matched_by_id' AND NOT c.observation_only AND c.missing_complete_scans = 0
			AND i.id = ? AND i.epoch = ? AND i.connection_revision = ? AND i.health = 'available'
			AND s.mode = 'assist' AND NOT s.reconcile_required AND NOT p.paused
			AND COALESCE((SELECT revision FROM language_preferences WHERE singleton = 1), 0) = ?
			ON CONFLICT(case_id) DO NOTHING`, string(raw), approval.id, approval.token, approval.scope,
			plan.instance.Ref.ID, plan.instance.Ref.Epoch, plan.instance.ConnectionRevision, plan.languageRevision)
		if err := requireManualImportRow(result, err); err != nil {
			return err
		}
		return manualImportEvent(ctx, tx, approval.id, "recovery_manual_import_sending", record.Attempt.SentAt)
	})
}

func (s *ManualReviews) allowManualImportSend(ctx context.Context, approval manualImportApproval, plan manualImportPlan, attempt domain.AttemptID) error {
	if s.clock.Now().Sub(approval.approvedAt) >= manualApprovalWindow {
		return app.ErrHeld
	}
	var allowed bool
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM manual_import_attempts AS a JOIN manual_reviews AS m ON m.token = a.token AND m.case_id = a.case_id
			JOIN observe_cases AS c ON c.id = m.case_id AND c.revision = m.case_revision
			JOIN observe_instances AS i ON i.id = c.instance_id AND i.epoch = c.epoch
			JOIN app_state AS s ON s.singleton = 1 JOIN action_pause AS p ON p.singleton = 1
			WHERE m.case_id = ? AND m.token = ? AND m.state = 'approved' AND m.action = 'import' AND m.scope_hash = ?
			AND a.stage = 'sending' AND json_extract(a.payload, '$.Attempt.ID') = ?
			AND c.state = 'held' AND c.reason = 'series_matched_by_id' AND NOT c.observation_only AND c.missing_complete_scans = 0
			AND i.id = ? AND i.epoch = ? AND i.connection_revision = ? AND i.health = 'available'
			AND s.mode = 'assist' AND NOT s.reconcile_required AND NOT p.paused
			AND COALESCE((SELECT revision FROM language_preferences WHERE singleton = 1), 0) = ?)`,
			approval.id, approval.token, approval.scope, attempt, plan.instance.Ref.ID,
			plan.instance.Ref.Epoch, plan.instance.ConnectionRevision, plan.languageRevision).Scan(&allowed)
	})
	if err == nil && !allowed {
		return app.ErrHeld
	}
	return err
}

func (s *ManualReviews) recordManualImport(ctx context.Context, slot manualSlot, record manualImportCheckpoint, stage string, event domain.EventKind) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE manual_import_attempts SET stage = ?, payload = ?
			WHERE case_id = ? AND token = ? AND stage = ?`, stage, string(raw), slot.id, slot.token, record.stage)
		if err := requireManualImportRow(result, err); err != nil {
			return err
		}
		if stage == "verified" || stage == "failed" {
			result, err := tx.ExecContext(ctx, `UPDATE manual_reviews SET state = 'completed', updated_at = ?
				WHERE case_id = ? AND token = ? AND state = 'approved' AND action = 'import'`, emptyPackStamp(s.clock.Now()), slot.id, slot.token)
			if err := requireManualImportRow(result, err); err != nil {
				return err
			}
		}
		if event == "" {
			return nil
		}
		var evidence []domain.EvidenceID
		if record.Outcome != nil {
			for _, effect := range record.Outcome.Effects {
				for _, id := range effect.Evidence {
					if !slices.Contains(evidence, id) {
						evidence = append(evidence, id)
					}
				}
			}
		}
		return manualImportEvent(ctx, tx, slot.id, event, s.clock.Now(), evidence...)
	})
}

func requireManualImportRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return app.ErrConflict
	}
	return err
}

func manualImportEvent(ctx context.Context, tx *sql.Tx, id domain.CaseID, kind domain.EventKind, now time.Time, evidence ...domain.EvidenceID) error {
	return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: kind,
		CaseID: id, Actor: "manual-import-worker", OccurredAt: now, Evidence: evidence})
}

func (s *ManualReviews) closeUncertainManualImport(ctx context.Context, choice app.ManualDecision) error {
	now := s.clock.Now()
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		var id domain.CaseID
		err := tx.QueryRowContext(ctx, `UPDATE manual_import_attempts SET stage = 'unconfirmed'
			WHERE token = ? AND stage = 'uncertain' AND EXISTS (SELECT 1 FROM manual_reviews AS m
				JOIN app_state AS s ON s.singleton = 1 WHERE m.token = ? AND m.state = 'approved'
				AND m.action = 'import' AND s.mode = 'assist') RETURNING case_id`, choice.Token, choice.Token).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return app.ErrConflict
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE manual_reviews SET state = 'completed', updated_at = ? WHERE case_id = ? AND token = ?`,
			emptyPackStamp(now), id, choice.Token)
		if err := requireManualImportRow(result, err); err != nil {
			return err
		}
		return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: "recovery_manual_import_unconfirmed",
			CaseID: id, Actor: choice.Actor, OccurredAt: now})
	})
}

func (s *ManualReviews) reconcileManualImport(ctx context.Context, slot manualSlot, record manualImportCheckpoint) error {
	if record.stage != "sending" && record.stage != "uncertain" {
		return app.ErrConflict
	}
	if !record.Prepared.Binds(record.Attempt.Operation) || len(record.Prepared.Import.Files) != 1 {
		return app.ErrConflict
	}
	instance, err := s.cases.RecoveryInstance(ctx, record.Attempt.Operation.Job.Instance, record.Prepared.ConnectionRevision)
	if err != nil {
		return err
	}
	release, err := s.cases.HoldRecoveryConnection(ctx, instance)
	if err != nil {
		return err
	}
	defer release()
	source := importflow.SonarrV5Source{Instance: instance, Job: record.Attempt.Operation.Job, Target: record.Attempt.Operation.Targets[0],
		DownloadID: record.Prepared.Import.DownloadID, Path: record.Prepared.Import.Files[0].Path,
		Client: record.Client, OutputPath: record.OutputPath, Size: record.Size}
	effects, err := s.importQualified(ctx, instance, source)
	if err != nil {
		return err
	}
	if effects == nil || effects.AdapterRevision() != record.Prepared.AdapterRevision {
		return arradapter.ErrQualification
	}
	attempt := record.Attempt
	attempt.Prepared = &record.Prepared
	outcome, err := effects.Reconcile(ctx, attempt)
	if err != nil {
		return err
	}
	if outcome.AttemptID != attempt.ID {
		return arradapter.ErrReadback
	}
	if verifiedManualImport(outcome, attempt) {
		record.Outcome = &outcome
		return s.recordManualImport(ctx, slot, record, "verified", "recovery_manual_import_verified")
	}
	if outcome.Verification == domain.VerificationNoEffect {
		record.Outcome = &outcome
		return s.recordManualImport(ctx, slot, record, "failed", "recovery_manual_import_failed")
	}
	return nil
}

func verifiedManualImport(outcome domain.Outcome, attempt domain.Attempt) bool {
	if outcome.Verification != domain.VerificationEffect || len(outcome.Effects) != len(attempt.Operation.Effects) ||
		outcome.ObservedAt.IsZero() || outcome.ObservedAt.Before(attempt.SentAt) {
		return false
	}
	for _, expected := range attempt.Operation.Effects {
		verified := false
		for _, result := range outcome.Effects {
			if result.Effect == expected && result.Verification == domain.VerificationEffect && len(result.Evidence) > 0 {
				verified = true
			}
		}
		if !verified {
			return false
		}
	}
	return true
}
