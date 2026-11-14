package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

func (j *Journal) BeginSend(ctx context.Context, reservation Reservation, proof Preflight, now time.Time) (SendLease, error) {
	var lease SendLease
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := readIntent(ctx, tx, reservation.Intent.ID)
		if err != nil {
			return err
		}
		if !matchesReservation(row, reservation) || row.until <= journalTime(now) {
			return ErrConflict
		}
		if !eligible(row.intent.Proposal, proof, now) || slices.ContainsFunc(proof.Evidence, func(id domain.EvidenceID) bool {
			return !boundedID(string(id))
		}) {
			return ErrHeld
		}
		if err := j.allowed(ctx, tx, row.approval(), now); err != nil {
			return err
		}
		preflight, err := marshalJournal(proof.Evidence)
		if err != nil {
			return err
		}
		attempt := domain.Attempt{ID: domain.AttemptID(store.NewID()), IntentID: row.intent.ID,
			Operation: row.intent.Proposal.Operation, Preflight: slices.Clone(proof.Evidence), SentAt: now.UTC()}
		_, err = tx.ExecContext(ctx, `INSERT INTO action_attempts (id, intent_id, preflight, sent_at) VALUES (?, ?, ?, ?)`,
			attempt.ID, row.intent.ID, preflight, journalTime(now))
		if err != nil {
			return err
		}
		lease = SendLease{Attempt: attempt, Fence: row.fence + 1, ExpiresAt: now.Add(sendLease).UTC()}
		if err := updateIntent(ctx, tx, row, domain.IntentSending, journalTime(lease.ExpiresAt)); err != nil {
			return err
		}
		return actionEvent(ctx, tx, row.intent, "action_sending_recorded", "", now, proof.Evidence...)
	})
	if err != nil {
		return SendLease{}, err
	}
	return lease, nil
}

func updateIntent(ctx context.Context, tx *sql.Tx, row journalIntent, state domain.IntentState, expiry string) error {
	result, err := tx.ExecContext(ctx, `UPDATE action_intents SET state = ?, fence = fence+1, lease_until = ?
		WHERE id = ? AND state = ? AND fence = ?`, state, expiry, row.intent.ID, row.intent.State, row.fence)
	return oneChange(result, err)
}

type journalAttempt struct {
	attempt     domain.Attempt
	outcome     domain.Outcome
	conflicting bool
}

func readAttempt(ctx context.Context, tx *sql.Tx, row journalIntent, id domain.AttemptID) (journalAttempt, error) {
	var a journalAttempt
	var preflight, sent, ack, outcome string
	err := tx.QueryRowContext(ctx, `SELECT id, intent_id, preflight, sent_at, acknowledgement, outcome, conflicting_readback
		FROM action_attempts WHERE id = ? AND intent_id = ?`, id, row.intent.ID).Scan(
		&a.attempt.ID, &a.attempt.IntentID, &preflight, &sent, &ack, &outcome, &a.conflicting)
	if errors.Is(err, sql.ErrNoRows) {
		return journalAttempt{}, ErrConflict
	}
	if err != nil {
		return journalAttempt{}, err
	}
	a.attempt.Operation = row.intent.Proposal.Operation
	if err := json.Unmarshal([]byte(preflight), &a.attempt.Preflight); err != nil {
		return journalAttempt{}, ErrJournal
	}
	if err := json.Unmarshal([]byte(ack), &a.attempt.Acknowledgement); err != nil {
		return journalAttempt{}, ErrJournal
	}
	if err := json.Unmarshal([]byte(outcome), &a.outcome); err != nil {
		return journalAttempt{}, ErrJournal
	}
	a.attempt.SentAt, err = time.Parse(time.RFC3339Nano, sent)
	if err != nil {
		return journalAttempt{}, ErrJournal
	}
	return a, nil
}

func intentForAttempt(ctx context.Context, tx *sql.Tx, id domain.AttemptID) (journalIntent, error) {
	var owner domain.IntentID
	err := tx.QueryRowContext(ctx, "SELECT intent_id FROM action_attempts WHERE id = ?", id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return journalIntent{}, ErrConflict
	}
	if err != nil {
		return journalIntent{}, err
	}
	return readIntent(ctx, tx, owner)
}

func readSendLease(ctx context.Context, tx *sql.Tx, lease SendLease) (journalIntent, error) {
	row, err := intentForAttempt(ctx, tx, lease.Attempt.ID)
	if err != nil {
		return journalIntent{}, err
	}
	if row.intent.ID != lease.Attempt.IntentID || row.intent.State != domain.IntentSending ||
		row.fence != lease.Fence || row.until != journalTime(lease.ExpiresAt) {
		return journalIntent{}, ErrConflict
	}
	return row, nil
}

func (j *Journal) RecordNotSent(ctx context.Context, lease SendLease, reason domain.ReasonCode, now time.Time) error {
	return j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := readSendLease(ctx, tx, lease)
		if err != nil {
			return err
		}
		return cancelIntent(ctx, tx, row, reason, now)
	})
}

func (j *Journal) RecordDispatch(ctx context.Context, lease SendLease, ack domain.Acknowledgement, now time.Time) error {
	state := domain.IntentUncertain
	switch ack.State {
	case domain.DispatchAcknowledged:
		state = domain.IntentAcknowledged
	case domain.DispatchRejectedBeforeEffect:
		state = domain.IntentRejectedBeforeEffect
	case domain.DispatchUncertain:
	default:
		return ErrInvalid
	}
	if now.IsZero() || (ack.Reason != "" && !boundedID(string(ack.Reason))) ||
		(ack.UpstreamReference != "" && !boundedID(ack.UpstreamReference)) {
		return ErrInvalid
	}
	return j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := readSendLease(ctx, tx, lease)
		if err != nil {
			return err
		}
		payload, err := marshalJournal(ack)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE action_attempts SET acknowledgement = ? WHERE id = ? AND intent_id = ?", payload, lease.Attempt.ID, row.intent.ID)
		if err := oneChange(result, err); err != nil {
			return err
		}
		if err := updateIntent(ctx, tx, row, state, ""); err != nil {
			return err
		}
		return actionEvent(ctx, tx, row.intent, domain.EventKind("action_"+state), ack.Reason, now)
	})
}

func (j *Journal) BeginReconcile(ctx context.Context, id domain.AttemptID, now time.Time) (Reconciliation, error) {
	if !boundedID(string(id)) || now.IsZero() {
		return Reconciliation{}, ErrInvalid
	}
	var lease Reconciliation
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := intentForAttempt(ctx, tx, id)
		if err != nil {
			return err
		}
		if row.until > journalTime(now) {
			return ErrHeld
		}
		switch row.intent.State {
		case domain.IntentSending, domain.IntentAcknowledged, domain.IntentRejectedBeforeEffect, domain.IntentUncertain, domain.IntentReconciling:
		default:
			return ErrConflict
		}
		attempt, err := readAttempt(ctx, tx, row, id)
		if err != nil {
			return err
		}
		if now.Before(attempt.attempt.SentAt) {
			return ErrHeld
		}
		if err := updateIntent(ctx, tx, row, domain.IntentReconciling, journalTime(now.Add(readLease))); err != nil {
			return err
		}
		lease = Reconciliation{Attempt: attempt.attempt, Fence: row.fence + 1}
		return actionEvent(ctx, tx, row.intent, "action_reconciling", "", now)
	})
	if err != nil {
		return Reconciliation{}, err
	}
	return lease, nil
}

func (j *Journal) RecordOutcome(ctx context.Context, lease Reconciliation, outcome domain.Outcome) (domain.Outcome, error) {
	if outcome.AttemptID != lease.Attempt.ID || outcome.ObservedAt.IsZero() {
		return domain.Outcome{}, ErrInvalid
	}
	var committed domain.Outcome
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := intentForAttempt(ctx, tx, outcome.AttemptID)
		if err != nil {
			return err
		}
		if row.intent.State != domain.IntentReconciling || row.fence != lease.Fence ||
			row.until <= journalTime(outcome.ObservedAt) {
			return ErrConflict
		}
		attempt, err := readAttempt(ctx, tx, row, outcome.AttemptID)
		if err != nil {
			return err
		}
		if outcome.ObservedAt.Before(attempt.attempt.SentAt) ||
			(attempt.outcome.AttemptID != "" && outcome.ObservedAt.Before(attempt.outcome.ObservedAt)) {
			return ErrConflict
		}
		observed := normaliseOutcome(attempt.attempt, outcome, outcome.ObservedAt)
		current, conflict := mergeReadback(attempt, observed)
		payload, err := marshalJournal(current)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE action_attempts SET outcome = ?, conflicting_readback = ? WHERE id = ?", payload, conflict, current.AttemptID)
		if err := oneChange(result, err); err != nil {
			return err
		}
		state, reason := domain.IntentReconciling, domain.ReasonCode(current.Verification)
		switch {
		case conflict:
			reason = "conflicting_effect_readback"
		case current.Verification == domain.VerificationEffect, current.Verification == domain.VerificationNoEffect,
			current.Verification == domain.VerificationExternal:
			state = domain.IntentCompleted
			if err := releaseLocks(ctx, tx, row.intent.ID); err != nil {
				return err
			}
		}
		if err := updateIntent(ctx, tx, row, state, ""); err != nil {
			return err
		}
		var evidence []domain.EvidenceID
		for _, effect := range observed.Effects {
			for _, id := range effect.Evidence {
				if boundedID(string(id)) {
					evidence = append(evidence, id)
				}
			}
		}
		slices.Sort(evidence)
		if err := actionEvent(ctx, tx, row.intent, "action_readback_recorded", reason, current.ObservedAt, slices.Compact(evidence)...); err != nil {
			return err
		}
		committed = current
		return nil
	})
	if err != nil {
		return domain.Outcome{}, err
	}
	return committed, nil
}

func mergeReadback(attempt journalAttempt, current domain.Outcome) (domain.Outcome, bool) {
	conflict := attempt.conflicting
	for i, effect := range current.Effects {
		if slices.ContainsFunc(effect.Evidence, func(id domain.EvidenceID) bool { return !boundedID(string(id)) }) {
			effect = domain.EffectResult{Effect: effect.Effect, Verification: domain.VerificationUnresolved}
		}
		previous, found := findEffect(attempt.outcome, effect.Effect)
		if found && previous.Verification != domain.VerificationUnresolved {
			switch {
			case effect.Verification == domain.VerificationUnresolved:
				effect = previous
			case previous.Verification != effect.Verification && previous.Verification != domain.VerificationPartial &&
				effect.Verification != domain.VerificationPartial:
				// Never replace terminal evidence with its contradiction. Keep the
				// effect hold durably until there is an explicit resolution path.
				conflict, effect = true, previous
			case previous.Verification != domain.VerificationPartial:
				effect = previous
			}
		}
		current.Effects[i] = effect
	}
	current = normaliseOutcome(attempt.attempt, current, current.ObservedAt)
	if conflict {
		current.Verification = domain.VerificationPartial
	}
	return current, conflict
}

func findEffect(outcome domain.Outcome, effect domain.Effect) (domain.EffectResult, bool) {
	for _, result := range outcome.Effects {
		if result.Effect == effect {
			return result, true
		}
	}
	return domain.EffectResult{}, false
}
