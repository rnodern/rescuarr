// Package ledger coordinates the write fence and independent effect readback.
package ledger

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync/atomic"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	// Mutation calls and verification reads each get their own bounded context.
	operationTimeout  = 10 * time.Second
	journalTimeout    = 5 * time.Second
	maxPreflightAge   = 30 * time.Second
	maxPreflightFacts = 64
)

var (
	ErrInvalid  = errors.New("invalid ledger input")
	ErrHeld     = errors.New("operation held")
	ErrConflict = errors.New("ledger state conflict")
	ErrJournal  = errors.New("ledger write unavailable")
	ErrHalted   = errors.New("dispatch halted after ledger failure")
)

type Clock interface{ Now() time.Time }

type Reservation struct {
	Intent domain.Intent
	Fence  uint64
}

type Reconciliation struct {
	Attempt domain.Attempt
	Fence   uint64
}

type Preflight struct {
	DecisionHash       string
	CaseRevision       domain.Revision
	PolicyRevision     domain.Revision
	CapabilityRevision domain.Revision
	Evidence           []domain.EvidenceID
	ObservedAt         time.Time
	ExpiresAt          time.Time
}

// Gate performs only fresh read-side checks. A positive result must match the
// reserved plan exactly. A Store must check the current local revisions again
// in the same transaction as the sending marker.
type Gate interface {
	Check(context.Context, domain.Intent) (Preflight, error)
}

// Store methods are short transactions and must never perform upstream I/O.
// Each successful state change must commit its corresponding audit record in
// the same transaction. A fence is the durable generation of an acquired lease.
// ErrHeld and ErrConflict denote a refusal to commit a change. Any other storage
// error halts further dispatch by this Executor and is reconciled separately.
//
// A production implementation must also keep the exact typed upstream
// selectors in the attempt before returning from BeginSend. Until that shared
// contract exists, the executor is exercised with fixture adapters only.
type Store interface {
	// Reserve checks expiry, duplicate operation, current policy, pause, locks,
	// concurrency and remaining durable budgets. It charges before reservation.
	Reserve(context.Context, domain.IntentID, time.Time) (Reservation, error)
	// CancelReserved clears only the matching, definitely unsent reservation,
	// releases its locks and reverses any refundable pre-send attempt charge.
	CancelReserved(context.Context, Reservation, domain.ReasonCode, time.Time) error
	// BeginSend validates the lease, expiry, pause, epoch and exact decision,
	// then durably commits the sending marker and evidence before returning.
	BeginSend(context.Context, Reservation, Preflight, time.Time) (domain.Attempt, error)
	// RecordDispatch attaches acknowledgement or uncertainty to the attempt.
	// Any write error or conflict leaves the committed sending marker recoverable.
	RecordDispatch(context.Context, domain.AttemptID, domain.Acknowledgement, time.Time) error
	// BeginReconcile acquires a readback lease for a sent or possibly sent
	// attempt. A still-active send must be excluded until its lease expires.
	BeginReconcile(context.Context, domain.AttemptID, time.Time) (Reconciliation, error)
	// RecordOutcome stores effect evidence and any outbox entry atomically.
	// An unresolved effect retains its relevant hold and never requeues dispatch.
	RecordOutcome(context.Context, Reconciliation, domain.Outcome) error
}

type Executor struct {
	store   Store
	adapter domain.EffectAdapter
	gate    Gate
	clock   Clock
	halted  atomic.Bool
}

func NewExecutor(store Store, adapter domain.EffectAdapter, gate Gate, clock Clock) (*Executor, error) {
	if store == nil || adapter == nil || gate == nil || clock == nil {
		return nil, ErrInvalid
	}
	return &Executor{store: store, adapter: adapter, gate: gate, clock: clock}, nil
}

// Dispatch handles one explicitly scheduled intent. It never retries or
// schedules another operation. A caller must send prior attempts to Reconcile.
func (e *Executor) Dispatch(ctx context.Context, intentID domain.IntentID) (domain.AttemptID, error) {
	if intentID == "" {
		return "", ErrInvalid
	}
	if e.halted.Load() {
		return "", ErrHalted
	}
	reservation, err := e.store.Reserve(ctx, intentID, e.clock.Now())
	if err != nil {
		return "", e.checkStore(err)
	}
	intent := reservation.Intent
	if reservation.Fence == 0 || intent.ID != intentID || intent.State != domain.IntentReserved || !validOperation(intent.Proposal.Operation) {
		return "", e.checkStore(ErrJournal)
	}

	checkCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	checked, err := e.gate.Check(checkCtx, copyIntent(intent))
	checkedInTime := checkCtx.Err() == nil
	cancel()
	now := e.clock.Now()
	if err != nil || !checkedInTime || !eligible(intent.Proposal, checked, now) || e.halted.Load() {
		if err := e.cancelReserved(ctx, reservation, "preflight_held"); err != nil {
			return "", err
		}
		return "", ErrHeld
	}

	attempt, err := e.store.BeginSend(ctx, reservation, checked, now)
	if errors.Is(err, ErrHeld) {
		if err := e.cancelReserved(ctx, reservation, "dispatch_revalidated"); err != nil {
			return "", err
		}
		return "", ErrHeld
	}
	if err != nil {
		return "", e.checkStore(err)
	}
	if attempt.ID == "" || attempt.IntentID != intent.ID || attempt.SentAt.IsZero() || !attempt.SentAt.Equal(now) || attempt.Acknowledgement != (domain.Acknowledgement{}) ||
		!reflect.DeepEqual(attempt.Operation, intent.Proposal.Operation) || !reflect.DeepEqual(attempt.Preflight, checked.Evidence) {
		return attempt.ID, e.checkStore(ErrJournal)
	}

	ack := domain.Acknowledgement{State: domain.DispatchUncertain, Reason: "dispatch_uncertain"}
	if ctx.Err() == nil && !e.halted.Load() {
		sendCtx, cancel := context.WithTimeout(ctx, operationTimeout)
		observed, sendErr := e.adapter.Dispatch(sendCtx, attempt.Operation)
		sendInTime := sendCtx.Err() == nil
		cancel()
		if sendErr == nil && sendInTime && (observed.State == domain.DispatchAcknowledged || observed.State == domain.DispatchRejectedBeforeEffect) {
			ack = observed
		}
	}

	// The original caller may have disconnected while the operation was in
	// flight. Complete the local journal step even when its context expired.
	journalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalTimeout)
	defer cancel()
	if err := e.store.RecordDispatch(journalCtx, attempt.ID, ack, e.clock.Now()); err != nil {
		// Once an attempt is fenced, any failed journal acknowledgement is
		// ambiguous even if a transaction reports cancellation or a conflict.
		e.halted.Store(true)
		return attempt.ID, ErrJournal
	}
	return attempt.ID, nil
}

// Reconcile reads back an existing attempt. It may run while dispatch is
// paused or latched off due to a journal fault.
func (e *Executor) Reconcile(ctx context.Context, attemptID domain.AttemptID) (domain.Outcome, error) {
	if attemptID == "" {
		return domain.Outcome{}, ErrInvalid
	}
	lease, err := e.store.BeginReconcile(ctx, attemptID, e.clock.Now())
	if err != nil {
		return domain.Outcome{}, e.checkStore(err)
	}
	if lease.Fence == 0 || lease.Attempt.ID != attemptID || lease.Attempt.SentAt.IsZero() || !validOperation(lease.Attempt.Operation) {
		return domain.Outcome{}, e.checkStore(ErrJournal)
	}
	probeCtx, cancel := context.WithTimeout(ctx, operationTimeout)
	observed, probeErr := e.adapter.Reconcile(probeCtx, lease.Attempt)
	probeInTime := probeCtx.Err() == nil
	cancel()
	if probeErr != nil || !probeInTime {
		observed = domain.Outcome{}
	}
	out := normaliseOutcome(lease.Attempt, observed, e.clock.Now())
	journalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalTimeout)
	defer cancel()
	if err := e.store.RecordOutcome(journalCtx, lease, out); err != nil {
		return domain.Outcome{}, e.checkStore(err)
	}
	return out, nil
}

func (e *Executor) cancelReserved(ctx context.Context, reservation Reservation, reason domain.ReasonCode) error {
	journalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalTimeout)
	defer cancel()
	return e.checkStore(e.store.CancelReserved(journalCtx, reservation, reason, e.clock.Now()))
}

func (e *Executor) checkStore(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrHeld, ErrConflict, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, known) {
			return known
		}
	}
	e.halted.Store(true)
	return ErrJournal
}

func copyIntent(intent domain.Intent) domain.Intent {
	intent.Proposal.Operation.Targets = slices.Clone(intent.Proposal.Operation.Targets)
	intent.Proposal.Operation.Effects = slices.Clone(intent.Proposal.Operation.Effects)
	intent.Proposal.Guards = slices.Clone(intent.Proposal.Guards)
	for i := range intent.Proposal.Guards {
		intent.Proposal.Guards[i].Evidence = slices.Clone(intent.Proposal.Guards[i].Evidence)
	}
	return intent
}

func eligible(proposal domain.Proposal, current Preflight, now time.Time) bool {
	if proposal.DecisionHash == "" || proposal.DecisionHash != current.DecisionHash || proposal.CaseRevision == 0 || proposal.PolicyRevision == 0 || proposal.CapabilityRevision == 0 ||
		proposal.CaseRevision != current.CaseRevision ||
		proposal.PolicyRevision != current.PolicyRevision || proposal.CapabilityRevision != current.CapabilityRevision ||
		!now.Before(proposal.ExpiresAt) || !now.Before(current.ExpiresAt) || current.ObservedAt.IsZero() ||
		now.Before(current.ObservedAt) || now.Sub(current.ObservedAt) > maxPreflightAge ||
		len(current.Evidence) == 0 || len(current.Evidence) > maxPreflightFacts {
		return false
	}
	for _, guard := range proposal.Guards {
		if !guard.Passed {
			return false
		}
	}
	ids := make(map[domain.EvidenceID]bool)
	for _, id := range current.Evidence {
		if id == "" || ids[id] {
			return false
		}
		ids[id] = true
	}
	return true
}
