package recoveryservice

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

type ManualEmptyPackQualification func(context.Context, domain.Instance) (*arradapter.EmptyPackAdapter, error)

type ManualReviews struct {
	local      *store.Store
	cases      *observeservice.Service
	preview    *PreviewService
	transport  http.RoundTripper
	clock      app.Clock
	qualified  ManualEmptyPackQualification
	signingKey [32]byte
}

var _ app.ManualReviewService = (*ManualReviews)(nil)

func NewManualReviews(local *store.Store, cases *observeservice.Service, preview *PreviewService, transport http.RoundTripper,
	clock app.Clock, qualified ManualEmptyPackQualification) *ManualReviews {
	s := &ManualReviews{local: local, cases: cases, preview: preview, transport: transport, clock: clock, qualified: qualified}
	rand.Read(s.signingKey[:])
	return s
}

type manualSlot struct {
	id    domain.CaseID
	token string
	state string
}

func (s *ManualReviews) SetManualMode(ctx context.Context, enabled bool, actor domain.ActorID) error {
	if !emptyPackToken(string(actor), 128) {
		return app.ErrConflict
	}
	next, kind := domain.ModeObserve, domain.EventKind("recovery_manual_mode_disabled")
	if enabled {
		next, kind = domain.ModeAssist, "recovery_manual_mode_enabled"
	}
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		var mode domain.Mode
		var reconcile bool
		if err := tx.QueryRowContext(ctx, `SELECT mode, reconcile_required FROM app_state WHERE singleton = 1`).Scan(&mode, &reconcile); err != nil {
			return err
		}
		if mode == domain.ModeAutonomous || enabled && reconcile {
			return app.ErrHeld
		}
		if next == mode {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE manual_reviews SET token = ?, state = 'pending',
			search_ids = '[]', scope_hash = '', updated_at = ?
			WHERE state IN ('pending', 'approved')
			AND NOT EXISTS (SELECT 1 FROM empty_pack_recovery AS r WHERE r.case_id = manual_reviews.case_id)`,
			store.NewID(), emptyPackStamp(s.clock.Now())); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE app_state SET mode = ? WHERE singleton = 1`, next); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE observe_revision SET revision = revision + 1 WHERE singleton = 1`); err != nil {
			return err
		}
		return store.AppendEvent(ctx, tx, domain.Event{
			ID: domain.EventID(store.NewID()), Kind: kind, Actor: actor, OccurredAt: s.clock.Now(),
		})
	})
}

// activeSlot serialises selection across tabs. A pending case stays in the slot
// even if a subsequent scan changes or closes it.
func (s *ManualReviews) activeSlot(ctx context.Context) (manualSlot, error) {
	var slot manualSlot
	err := s.local.Write(ctx, func(tx *sql.Tx) error {
		var mode domain.Mode
		if err := tx.QueryRowContext(ctx, `SELECT mode FROM app_state WHERE singleton = 1`).Scan(&mode); err != nil {
			return err
		}
		if mode != domain.ModeAssist {
			return app.ErrHeld
		}
		err := tx.QueryRowContext(ctx, `SELECT case_id, token, state FROM manual_reviews WHERE state IN ('pending', 'approved')`).
			Scan(&slot.id, &slot.token, &slot.state)
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var revision domain.Revision
		err = tx.QueryRowContext(ctx, `SELECT c.id, c.revision FROM observe_cases AS c
			WHERE c.state = 'held' AND c.observation_only = 0 AND c.missing_complete_scans = 0
				AND c.reason IN ('no_eligible_files', 'series_matched_by_id')
				AND NOT EXISTS (SELECT 1 FROM manual_reviews AS r WHERE r.case_id = c.id)
				AND NOT EXISTS (SELECT 1 FROM empty_pack_recovery AS r WHERE r.case_id = c.id)
			ORDER BY c.id LIMIT 1`).Scan(&slot.id, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		slot.token, slot.state = store.NewID(), "pending"
		_, err = tx.ExecContext(ctx, `INSERT INTO manual_reviews (case_id, token, state, case_revision, updated_at)
			VALUES (?, ?, 'pending', ?, ?)`, slot.id, slot.token, revision, emptyPackStamp(s.clock.Now()))
		return err
	})
	return slot, err
}

func (s *ManualReviews) ManualReview(ctx context.Context) (app.ManualReviewView, error) {
	slot, err := s.activeSlot(ctx)
	if err != nil || slot.id == "" {
		return app.ManualReviewView{}, err
	}
	detail, err := s.cases.Case(ctx, slot.id)
	if err != nil && !errors.Is(err, app.ErrNotFound) {
		return app.ManualReviewView{}, err
	}
	item := detail.Case
	if item.ID == "" {
		item.ID = slot.id
	}
	view := app.ManualReviewView{Case: &item, Token: slot.token, Processing: slot.state == "approved"}
	if view.Processing {
		var stage string
		err := s.local.Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT stage FROM empty_pack_recovery WHERE manual_token = ?`, slot.token).Scan(&stage)
		})
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return app.ManualReviewView{}, err
		}
		switch EmptyPackStage(stage) {
		case EmptyPackRejectSending:
			view.Hold = "Waiting for Sonarr to confirm queue removal and blocklisting. Check the Sonarr queue and blocklist if this persists."
		case EmptyPackRejectVerified:
			view.Hold = "Sonarr confirmed removal and blocklisting. The episode search is waiting for fresh evidence and a verified connection."
		case EmptyPackSearchSending:
			view.Hold = "Sonarr’s search acknowledgement is pending or unavailable. Check Sonarr’s Activity before starting another search."
		}
		return view, nil
	}
	if item.State == domain.CaseClosed || item.Revision == 0 {
		view.Hold = "This download has left the recovery queue. Decline to move on."
		return view, nil
	}
	if item.Reason != domain.ReasonCode(observe.FindingNoEligibleFiles) {
		view.Diagnosis = "Sonarr identified the series from the grab history. The episode needs review before import."
		preview, err := s.preview.ImportPreview(ctx, item.ID, item.Revision)
		if err == nil || errors.Is(err, app.ErrHeld) {
			view.Preview = preview
		}
		if view.Preview.Episode == nil && item.Display.EpisodeCheck != nil {
			view.Preview.Episode = &item.Display.EpisodeCheck.EpisodeNameReview
		}
		view.Hold = "Import approval is unavailable in this build. Decline to move on."
		return view, nil
	}
	view.Diagnosis = "Sonarr found no eligible files to import from this download."
	plan, err := prepareManualEmptyPack(ctx, s.cases, s.local, s.clock, s.transport, item.ID, item.Revision)
	if err != nil {
		view.Hold = "The pack or episode status could not be confirmed. Refresh to check it again or decline to move on."
		return view, nil
	}
	view.Preview.Replacement, view.Folder = plan.preview, plan.folder
	for _, episode := range plan.preview.Episodes {
		if episode.Status == app.ReplacementSearch {
			view.SearchLabels = append(view.SearchLabels, episode.Label)
		}
	}
	if err := s.validateApproval(ctx, item, plan); err != nil {
		view.Hold = "The replacement plan is available for review. Approval requires an unpaused, verified Sonarr connection."
		return view, nil
	}
	expires := s.clock.Now().Add(5 * time.Minute).Unix()
	until := strconv.FormatInt(expires, 10)
	view.ApprovalToken = slot.token + "." + until + "." + s.sign(slot.token, until, plan.scopeHash)
	view.CanApprove = true
	return view, nil
}

func (s *ManualReviews) validateApproval(ctx context.Context, item domain.Case, plan manualPackPlan) error {
	if s.qualified == nil || plan.preview == nil || plan.preview.UnclearCount != 0 || len(item.Incidents) != 0 {
		return app.ErrHeld
	}
	var paused, reconcile bool
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT paused, reconcile_required FROM action_pause JOIN app_state USING (singleton)`).Scan(&paused, &reconcile)
	})
	if err != nil {
		return err
	}
	if paused || reconcile {
		return app.ErrHeld
	}
	instance, err := s.cases.RecoveryInstance(ctx, plan.checkpoint.Key.Job.Instance, plan.checkpoint.ConnectionRevision)
	if err != nil {
		return err
	}
	adapter, err := s.qualified(ctx, instance)
	if err != nil {
		return err
	}
	return adapter.Preflight(ctx, emptyPackScope(plan.checkpoint))
}

func (s *ManualReviews) sign(slot, until, scope string) string {
	mac := hmac.New(sha256.New, s.signingKey[:])
	mac.Write([]byte(slot + "." + until + "." + scope))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *ManualReviews) DecideManualReview(ctx context.Context, choice app.ManualDecision) error {
	if len(choice.Token) > 160 || !emptyPackToken(string(choice.Actor), 128) {
		return app.ErrConflict
	}
	token, until, sig := choice.Token, "", ""
	if choice.Approve {
		fields := strings.Split(choice.Token, ".")
		if len(fields) != 3 {
			return app.ErrConflict
		}
		token, until, sig = fields[0], fields[1], fields[2]
		expiry, err := strconv.ParseInt(until, 10, 64)
		if err != nil || s.clock.Now().Unix() >= expiry {
			return app.ErrConflict
		}
	}
	var id domain.CaseID
	err := s.local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT case_id FROM manual_reviews
			WHERE token = ? AND state = 'pending' AND (SELECT mode FROM app_state) = 'assist'`, token).Scan(&id)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return app.ErrConflict
	}
	if err != nil {
		return err
	}
	state, kind := "declined", domain.EventKind("recovery_manual_declined")
	hash, search := "", "[]"
	var revision domain.Revision
	if choice.Approve {
		detail, err := s.cases.Case(ctx, id)
		if err != nil {
			return err
		}
		revision = detail.Case.Revision
		plan, err := prepareManualEmptyPack(ctx, s.cases, s.local, s.clock, s.transport, id, revision)
		if err != nil || !hmac.Equal([]byte(sig), []byte(s.sign(token, until, plan.scopeHash))) {
			return app.ErrConflict
		}
		if err := s.validateApproval(ctx, detail.Case, plan); err != nil {
			return app.ErrHeld
		}
		encoded, err := json.Marshal(append([]int64{}, plan.searchIDs...))
		if err != nil {
			return err
		}
		hash, search, state, kind = plan.scopeHash, string(encoded), "approved", "recovery_manual_approved"
	}
	now := s.clock.Now()
	return s.local.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE manual_reviews SET state = ?, scope_hash = ?, search_ids = ?,
			case_revision = CASE WHEN ? > 0 THEN ? ELSE case_revision END, actor = ?, updated_at = ?
			WHERE case_id = ? AND token = ? AND state = 'pending' AND (SELECT mode FROM app_state) = 'assist'
			AND (? = 0 OR (EXISTS (SELECT 1 FROM observe_cases WHERE id = ? AND revision = ? AND state = 'held')
				AND (SELECT NOT reconcile_required FROM app_state) AND (SELECT NOT paused FROM action_pause)))`,
			state, hash, search, revision, revision, choice.Actor, emptyPackStamp(now), id, token, revision, id, revision)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return app.ErrConflict
		}
		return store.AppendEvent(ctx, tx, domain.Event{
			ID: domain.EventID(store.NewID()), Kind: kind, CaseID: id, Actor: choice.Actor, OccurredAt: now,
		})
	})
}

func resetManualReview(ctx context.Context, local *store.Store, id domain.CaseID, token string, now time.Time) error {
	return local.Write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE manual_reviews SET token = ?, state = 'pending', search_ids = '[]', scope_hash = '', updated_at = ?
			WHERE case_id = ? AND token = ? AND state = 'approved'
			AND NOT EXISTS (SELECT 1 FROM empty_pack_recovery WHERE case_id = ?)`,
			store.NewID(), emptyPackStamp(now), id, token, id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			return err
		}
		return store.AppendEvent(ctx, tx, domain.Event{
			ID: domain.EventID(store.NewID()), Kind: "recovery_manual_review_changed", CaseID: id, Actor: "manual-review-worker", OccurredAt: now,
		})
	})
}
