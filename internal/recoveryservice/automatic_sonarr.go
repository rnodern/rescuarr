package recoveryservice

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

const automaticSonarrActor = "automatic-import-worker"

func (s *ManualReviews) AutomaticImportsConfigured() bool {
	return s.importQualified != nil && s.importAudio != nil
}

func (s *ManualReviews) SetAutomaticImportMode(ctx context.Context, enabled bool, actor domain.ActorID) error {
	if !emptyPackToken(string(actor), 128) {
		return app.ErrConflict
	}
	if enabled && !s.AutomaticImportsConfigured() {
		return app.ErrHeld
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		var mode domain.Mode
		var blocked bool
		if err := tx.QueryRowContext(ctx, `SELECT mode, paused OR reconcile_required OR
			EXISTS (SELECT 1 FROM manual_reviews WHERE state = 'approved')
			OR EXISTS (SELECT 1 FROM action_intents WHERE state NOT IN ('completed', 'cancelled'))
			OR EXISTS (SELECT 1 FROM empty_pack_recovery WHERE stage <> 'done')
			FROM app_state JOIN action_pause USING (singleton) WHERE singleton = 1`).Scan(&mode, &blocked); err != nil {
			return err
		}
		next, event := domain.ModeAssist, domain.EventKind("recovery_automatic_import_disabled")
		if enabled {
			if mode == domain.ModeAutonomous {
				return nil
			}
			if blocked {
				return app.ErrHeld
			}
			next, event = domain.ModeAutonomous, "recovery_automatic_import_enabled"
			// Discard an unapproved review so it cannot occupy the import slot.
			if _, err := tx.ExecContext(ctx, `DELETE FROM manual_reviews WHERE state = 'pending'
				AND NOT EXISTS (SELECT 1 FROM manual_import_attempts WHERE case_id = manual_reviews.case_id)
				AND NOT EXISTS (SELECT 1 FROM empty_pack_recovery WHERE case_id = manual_reviews.case_id)`); err != nil {
				return err
			}
		} else {
			if mode != domain.ModeAutonomous {
				return nil
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM manual_reviews
				WHERE state = 'approved' AND actor = ? AND action = 'import'
				AND NOT EXISTS (SELECT 1 FROM manual_import_attempts WHERE case_id = manual_reviews.case_id)`, automaticSonarrActor); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_state SET mode = ? WHERE singleton = 1`, next); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE observe_revision SET revision = revision + 1 WHERE singleton = 1`); err != nil {
			return err
		}
		return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Kind: event, Actor: actor, OccurredAt: s.clock.Now()})
	})
}

func (s *ManualReviews) queueAutomaticSonarr(ctx context.Context, after *domain.CaseID) error {
	if !s.AutomaticImportsConfigured() {
		return nil
	}
	const ready = `s.mode = 'autonomous' AND NOT s.reconcile_required AND NOT p.paused
		AND NOT EXISTS (SELECT 1 FROM manual_reviews WHERE state IN ('approved', 'pending'))
		AND NOT EXISTS (SELECT 1 FROM action_intents WHERE state NOT IN ('completed', 'cancelled'))
		AND NOT EXISTS (SELECT 1 FROM empty_pack_recovery WHERE stage <> 'done')`
	var enabled bool
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT `+ready+`
			FROM app_state AS s JOIN action_pause AS p USING (singleton) WHERE s.singleton = 1`).Scan(&enabled)
	})
	if err != nil || !enabled {
		return err
	}
	items, err := s.cases.AutomaticImportCases(ctx, *after, 1)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		*after = ""
		return nil
	}
	item := items[0]
	*after = item.ID
	plan, err := prepareManualImport(ctx, s.cases, s.local, s.clock, s.transport, item.ID, item.Revision, s.importAudio)
	if s.automaticEvidenceHold(ctx, item, plan, err) != "" {
		return nil
	}
	effects, err := s.importQualified(ctx, plan.instance, plan.source)
	if errors.Is(err, arradapter.ErrQualification) || err == nil && effects == nil {
		return nil
	}
	if err != nil {
		return err
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO manual_reviews (case_id, token, state, case_revision, scope_hash, actor, updated_at, action)
			SELECT c.id, ?, 'approved', c.revision, ?, ?, ?, 'import' FROM observe_cases AS c
			JOIN app_state AS s ON s.singleton = 1 JOIN action_pause AS p ON p.singleton = 1
			WHERE c.id = ? AND c.revision = ? AND c.state = 'held' AND (`+ready+`)
			ON CONFLICT DO NOTHING`, store.NewID(), manualImportScopeHash(plan, effects), automaticSonarrActor,
			emptyPackStamp(s.clock.Now()), item.ID, item.Revision)
		return err
	})
}

func (s *ManualReviews) validateAutomaticImport(ctx context.Context, approval manualImportApproval, plan manualImportPlan) error {
	if !s.AutomaticImportsConfigured() {
		return app.ErrHeld
	}
	var ready bool
	if err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM manual_reviews AS m
			JOIN app_state AS s ON s.singleton = 1 JOIN action_pause AS p ON p.singleton = 1
			WHERE m.case_id = ? AND m.token = ? AND m.state = 'approved' AND m.actor = ?
			AND s.mode = 'autonomous' AND NOT s.reconcile_required AND NOT p.paused)`,
			approval.id, approval.token, automaticSonarrActor).Scan(&ready)
	}); err != nil {
		return err
	}
	if !ready {
		return app.ErrHeld
	}
	item, err := s.cases.Case(ctx, approval.id)
	if err != nil {
		return err
	}
	if item.Case.Revision != approval.revision || s.automaticEvidenceHold(ctx, item.Case, plan, nil) != "" {
		return app.ErrHeld
	}
	return nil
}

func (s *ManualReviews) releaseUnsentImport(ctx context.Context, slot manualSlot) error {
	if !slot.automatic {
		return resetManualReview(ctx, s.local, slot.id, slot.token, s.clock.Now())
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM manual_reviews
			WHERE case_id = ? AND token = ? AND actor = ? AND state = 'approved'
			AND NOT EXISTS (SELECT 1 FROM manual_import_attempts WHERE case_id = ?)`,
			slot.id, slot.token, automaticSonarrActor, slot.id)
		return err
	})
}
