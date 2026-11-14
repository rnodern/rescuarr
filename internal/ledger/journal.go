package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

const (
	reserveLease      = 30 * time.Second
	sendLease         = 30 * time.Second
	readLease         = 30 * time.Second
	maxJournalID      = 128
	maxJournalPayload = 1 << 20
)

// Approval carries a server-side proposal and local, corroborated ownership
// keys. The keys describe the physical resources shared across Arr instances.
type Approval struct {
	Proposal    domain.Proposal
	Actor       domain.ActorID
	RequestKey  string
	PhysicalJob string
	Client      string
}

// AuthorizeLocal revalidates authoritative local policy and revision state in
// the caller's SQLite transaction. It must not make any external requests.
type AuthorizeLocal func(context.Context, *sql.Tx, Approval, time.Time) error

// Journal implements the durable Store contract without a transport. It
// persists private preflight separately from public Attempt projections.
type EventWriter func(context.Context, *sql.Tx, domain.Event) error

type Journal struct {
	local     *store.Store
	authorize AuthorizeLocal
	notify    EventWriter
}

var _ Store = (*Journal)(nil)

func NewJournal(local *store.Store, authorize AuthorizeLocal, notify ...EventWriter) (*Journal, error) {
	if local == nil || authorize == nil {
		return nil, ErrInvalid
	}
	journal := &Journal{local: local, authorize: authorize}
	if len(notify) > 1 || (len(notify) == 1 && notify[0] == nil) {
		return nil, ErrInvalid
	}
	if len(notify) == 1 {
		journal.notify = notify[0]
	}
	return journal, nil
}

type journalIntent struct {
	intent domain.Intent
	job    string
	client string
	fence  uint64
	until  string
}

type rowScanner interface{ Scan(...any) error }

const intentColumns = "id, actor, request_key, proposal, physical_job, client, state, fence, lease_until, created_at"

func scanIntent(row rowScanner) (journalIntent, error) {
	var value journalIntent
	var proposal, created string
	err := row.Scan(&value.intent.ID, &value.intent.Actor, &value.intent.RequestKey, &proposal,
		&value.job, &value.client, &value.intent.State, &value.fence, &value.until, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return journalIntent{}, ErrConflict
	}
	if err != nil {
		return journalIntent{}, err
	}
	if err := json.Unmarshal([]byte(proposal), &value.intent.Proposal); err != nil {
		return journalIntent{}, ErrJournal
	}
	value.intent.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return journalIntent{}, ErrJournal
	}
	return value, nil
}

func readIntent(ctx context.Context, tx *sql.Tx, id domain.IntentID) (journalIntent, error) {
	return scanIntent(tx.QueryRowContext(ctx, "SELECT "+intentColumns+" FROM action_intents WHERE id = ?", id))
}

func (i journalIntent) approval() Approval {
	return Approval{Proposal: i.intent.Proposal, Actor: i.intent.Actor, RequestKey: i.intent.RequestKey,
		PhysicalJob: i.job, Client: i.client}
}

// Approve stores a one-shot proposal, returning an already committed intent
// only when the actor, request key and immutable plan all match.
func (j *Journal) Approve(ctx context.Context, approval Approval, now time.Time) (domain.Intent, error) {
	if !validApproval(approval) || now.IsZero() {
		return domain.Intent{}, ErrInvalid
	}
	var intent domain.Intent
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		record, err := scanIntent(tx.QueryRowContext(ctx, "SELECT "+intentColumns+" FROM action_intents WHERE actor = ? AND request_key = ?", approval.Actor, approval.RequestKey))
		if err == nil {
			canonical, err := marshalJournal(record.approval())
			if err != nil {
				return err
			}
			request, err := marshalJournal(approval)
			if err != nil || canonical != request {
				return ErrConflict
			}
			intent = record.intent
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		if err := j.allowed(ctx, tx, approval, now); err != nil {
			return err
		}
		// A new browser request or proposal nonce does not make a previously
		// sent semantic operation new. A fresh eligible replan must use a new
		// operation fingerprint. A proven unsent cancellation is exempt.
		var alreadySent bool
		const sent = `SELECT EXISTS (SELECT 1 FROM action_intents AS i JOIN action_attempts AS a ON a.intent_id = i.id
			WHERE i.operation_key = ? AND i.state != 'cancelled')`
		if err := tx.QueryRowContext(ctx, sent, approval.Proposal.Operation.Key).Scan(&alreadySent); err != nil {
			return err
		}
		if alreadySent {
			return ErrConflict
		}
		proposal, err := marshalJournal(approval.Proposal)
		if err != nil {
			return err
		}
		intent = domain.Intent{ID: domain.IntentID(store.NewID()), Proposal: approval.Proposal, Actor: approval.Actor,
			RequestKey: approval.RequestKey, CreatedAt: now.UTC(), State: domain.IntentAuthorized}
		change, err := tx.ExecContext(ctx, `
			INSERT INTO action_intents (id, proposal_id, actor, request_key, operation_key, proposal, physical_job, client, state, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			intent.ID, intent.Proposal.ID, intent.Actor, intent.RequestKey, intent.Proposal.Operation.Key, proposal, approval.PhysicalJob,
			approval.Client, intent.State, journalTime(now))
		if err := oneChange(change, err); err != nil {
			return err
		}
		return actionEvent(ctx, tx, intent, "action_authorized", "", now)
	})
	if err != nil {
		return domain.Intent{}, err
	}
	return intent, nil
}

func (j *Journal) allowed(ctx context.Context, tx *sql.Tx, approval Approval, now time.Time) error {
	if !now.Before(approval.Proposal.ExpiresAt) {
		return ErrHeld
	}
	var mode domain.Mode
	var reconcile, paused bool
	const state = `SELECT mode, reconcile_required, paused FROM app_state JOIN action_pause USING (singleton) WHERE singleton = 1`
	if err := tx.QueryRowContext(ctx, state).Scan(&mode, &reconcile, &paused); err != nil {
		return err
	}
	if reconcile || paused || (mode != domain.ModeAssist && mode != domain.ModeAutonomous) {
		return ErrHeld
	}
	return j.authorize(ctx, tx, approval, now)
}

// SetPaused commits the kill switch and audit together. A send linearises at
// BeginSend. Readback of previously marked sends remains available when paused.
func (j *Journal) SetPaused(ctx context.Context, paused bool, actor domain.ActorID, now time.Time) error {
	if !boundedID(string(actor)) || now.IsZero() {
		return ErrInvalid
	}
	return j.local.Write(ctx, func(tx *sql.Tx) error {
		update, err := tx.ExecContext(ctx, "UPDATE action_pause SET paused = ? WHERE singleton = 1 AND paused != ?", paused, paused)
		if err != nil {
			return err
		}
		changed, err := update.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		kind := domain.EventKind("action_resume")
		if paused {
			kind = "action_pause"
		}
		if err := store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Actor: actor, Kind: kind,
			Scope: domain.Scope{Kind: domain.ScopeStore}, OccurredAt: now}); err != nil {
			return err
		}
		if paused && j.notify != nil {
			return j.notify(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Actor: actor,
				Kind: "automation_paused", Scope: domain.Scope{Kind: domain.ScopeStore}, OccurredAt: now})
		}
		return nil
	})
}

type lockKey struct{ kind, key string }

func locksFor(row journalIntent) []lockKey {
	op := row.intent.Proposal.Operation
	keys := []lockKey{{"instance", string(op.Job.Instance.ID)}, {"job", row.job}, {"client", row.client}}
	for _, ref := range op.Targets {
		// Intentionally span an epoch change pending resolution of any ambiguous send.
		keys = append(keys, lockKey{"target", string(ref.Instance.ID) + "/" + string(ref.Kind) + "/" + strconv.FormatInt(ref.ID, 10)})
	}
	return keys
}

func (j *Journal) Reserve(ctx context.Context, id domain.IntentID, now time.Time) (Reservation, error) {
	if !boundedID(string(id)) || now.IsZero() {
		return Reservation{}, ErrInvalid
	}
	var reservation Reservation
	err := j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := readIntent(ctx, tx, id)
		if err != nil {
			return err
		}
		if row.intent.State != domain.IntentAuthorized {
			return ErrConflict
		}
		if err := j.allowed(ctx, tx, row.approval(), now); err != nil {
			return err
		}
		// Count in-flight slots, including expired sends whose effects are uncertain.
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM action_scope_locks WHERE kind = 'instance'").Scan(&active); err != nil {
			return err
		}
		if active >= 2 {
			return ErrHeld
		}
		for _, lock := range locksFor(row) {
			result, err := tx.ExecContext(ctx, `INSERT INTO action_scope_locks (kind, scope_key, intent_id)
				VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, lock.kind, lock.key, id)
			if err := oneChange(result, err); err != nil {
				return err
			}
		}
		if err := charge(ctx, tx, row, now); err != nil {
			return err
		}
		change, err := tx.ExecContext(ctx, `
			UPDATE action_intents SET state = 'reserved', fence = fence + 1, lease_until = ?
			WHERE id = ? AND state = 'authorized' AND fence = ?`, journalTime(now.Add(reserveLease)), id, row.fence)
		if err := oneChange(change, err); err != nil {
			return err
		}
		row.intent.State = domain.IntentReserved
		reservation = Reservation{Intent: row.intent, Fence: row.fence + 1}
		return actionEvent(ctx, tx, row.intent, "action_reserved", "", now)
	})
	if err != nil {
		return Reservation{}, err
	}
	return reservation, nil
}

func (j *Journal) CancelReserved(ctx context.Context, reservation Reservation, reason domain.ReasonCode, now time.Time) error {
	return j.local.Write(ctx, func(tx *sql.Tx) error {
		row, err := readIntent(ctx, tx, reservation.Intent.ID)
		if err != nil {
			return err
		}
		if !matchesReservation(row, reservation) {
			return ErrConflict
		}
		return cancelIntent(ctx, tx, row, reason, now)
	})
}

// ReapReserved releases leases that expired before a send marker. Marked
// attempts remain held for readback and are never reset to authorised.
func (j *Journal) ReapReserved(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return ErrInvalid
	}
	return j.local.Write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+intentColumns+" FROM action_intents WHERE state = 'reserved' AND lease_until <= ? LIMIT 2", journalTime(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		var held []journalIntent
		for rows.Next() {
			row, err := scanIntent(rows)
			if err != nil {
				return err
			}
			held = append(held, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, row := range held {
			if err := cancelIntent(ctx, tx, row, "reservation_expired_unsent", now); err != nil {
				return err
			}
		}
		return nil
	})
}

func matchesReservation(row journalIntent, reservation Reservation) bool {
	return row.intent.State == domain.IntentReserved && row.fence != 0 && row.fence == reservation.Fence &&
		reflect.DeepEqual(row.intent.Proposal, reservation.Intent.Proposal) &&
		row.intent.Actor == reservation.Intent.Actor && row.intent.RequestKey == reservation.Intent.RequestKey
}

func cancelIntent(ctx context.Context, tx *sql.Tx, row journalIntent, reason domain.ReasonCode, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE action_intents SET state = 'cancelled', fence = fence+1, lease_until = ''
		WHERE id = ? AND state = ? AND fence = ?`, row.intent.ID, row.intent.State, row.fence)
	if err := oneChange(result, err); err != nil {
		return err
	}
	if err := releaseLocks(ctx, tx, row.intent.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM action_charges WHERE intent_id = ?", row.intent.ID); err != nil {
		return err
	}
	return actionEvent(ctx, tx, row.intent, "action_cancelled_unsent", reason, now)
}

func releaseLocks(ctx context.Context, tx *sql.Tx, id domain.IntentID) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM action_scope_locks WHERE intent_id = ?", id)
	return err
}

func journalTime(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func oneChange(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}

func boundedID(id string) bool {
	if id == "" || len(id) > maxJournalID {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func validApproval(approval Approval) bool {
	p := approval.Proposal
	if !boundedID(string(p.ID)) || !boundedID(string(p.CaseID)) || !boundedID(string(approval.Actor)) || !boundedID(approval.RequestKey) ||
		!boundedID(approval.PhysicalJob) || !boundedID(approval.Client) || !boundedID(p.DecisionHash) ||
		!boundedID(string(p.Operation.Key)) || !boundedID(string(p.Operation.Job.ID)) || !boundedID(string(p.Operation.Job.Instance.ID)) ||
		p.CaseRevision == 0 || p.PolicyRevision == 0 || p.CapabilityRevision == 0 || len(p.Guards) == 0 || len(p.Guards) > maxPreflightFacts ||
		p.ExpiresAt.IsZero() || !validOperation(p.Operation) {
		return false
	}
	guardFacts := 0
	for _, guard := range p.Guards {
		guardFacts += len(guard.Evidence)
		if !guard.Passed || !boundedID(string(guard.Reason)) || guardFacts > maxPreflightFacts {
			return false
		}
		for _, evidence := range guard.Evidence {
			if !boundedID(string(evidence)) {
				return false
			}
		}
	}
	return true
}

func marshalJournal(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > maxJournalPayload {
		return "", ErrInvalid
	}
	return string(body), nil
}

func actionEvent(ctx context.Context, tx *sql.Tx, intent domain.Intent, kind domain.EventKind, reason domain.ReasonCode, now time.Time, evidence ...domain.EvidenceID) error {
	if now.IsZero() || (reason != "" && !boundedID(string(reason))) {
		return ErrInvalid
	}
	return store.AppendEvent(ctx, tx, domain.Event{ID: domain.EventID(store.NewID()), Actor: intent.Actor, Kind: kind,
		Scope:  domain.Scope{Kind: domain.ScopeInstance, Instance: intent.Proposal.Operation.Job.Instance},
		CaseID: intent.Proposal.CaseID, IntentID: intent.ID, Reason: reason, OccurredAt: now, Evidence: evidence})
}
