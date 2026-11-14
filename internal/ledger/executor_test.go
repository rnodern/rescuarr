package ledger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

var fixtureTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const fixtureAttempt = domain.AttemptID("attempt-1")

type fixedClock struct{}

func (fixedClock) Now() time.Time { return fixtureTime }

type gateFunc func(context.Context, domain.Intent) (Preflight, error)

func (f gateFunc) Check(ctx context.Context, intent domain.Intent) (Preflight, error) {
	return f(ctx, intent)
}

func fixtureIntent() domain.Intent {
	instance := domain.InstanceRef{ID: "fixture", Epoch: 2}
	target := domain.TargetRef{Instance: instance, Kind: domain.TargetMovie, ID: 9}
	return domain.Intent{ID: "intent-1", State: domain.IntentAuthorized, Proposal: domain.Proposal{
		ID: "proposal-1", CaseID: "case-1", CaseRevision: 2, PolicyRevision: 3, CapabilityRevision: 4,
		DecisionHash: "material-hash", ExpiresAt: fixtureTime.Add(time.Minute),
		Operation: domain.Operation{Key: "operation-1", Job: domain.JobRef{Instance: instance, ID: "job-1", Generation: 1},
			Targets: []domain.TargetRef{target}, Action: domain.ActionImport,
			Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}},
		},
	}}
}

func freshFacts(_ context.Context, intent domain.Intent) (Preflight, error) {
	return Preflight{DecisionHash: intent.Proposal.DecisionHash, CaseRevision: intent.Proposal.CaseRevision,
		PolicyRevision: intent.Proposal.PolicyRevision, CapabilityRevision: intent.Proposal.CapabilityRevision,
		Evidence: []domain.EvidenceID{"fresh-evidence"}, ObservedAt: fixtureTime.Add(-time.Second), ExpiresAt: fixtureTime.Add(time.Second)}, nil
}

// The in-memory store exercises caller ordering and crash recovery across
// executor instances. It is not a persistence implementation.
type fixtureStore struct {
	sync.Mutex
	intent          domain.Intent
	attempt         domain.Attempt
	outcome         domain.Outcome
	paused          bool
	sendLeaseActive bool
	failAudit       bool
	failBegin       bool
	invalidJournal  bool
	cancelled       int
	claimedReadback bool
}

func (s *fixtureStore) Reserve(ctx context.Context, id domain.IntentID, now time.Time) (Reservation, error) {
	s.Lock()
	defer s.Unlock()
	if err := ctx.Err(); err != nil {
		return Reservation{}, err
	}
	if s.paused || !now.Before(s.intent.Proposal.ExpiresAt) {
		return Reservation{}, ErrHeld
	}
	if s.intent.ID != id || s.intent.State != domain.IntentAuthorized {
		return Reservation{}, ErrConflict
	}
	s.intent.State = domain.IntentReserved
	return Reservation{Intent: copyIntent(s.intent), Fence: 1}, nil
}

func (s *fixtureStore) CancelReserved(_ context.Context, _ Reservation, _ domain.ReasonCode, _ time.Time) error {
	s.Lock()
	defer s.Unlock()
	if s.intent.State != domain.IntentReserved {
		return ErrConflict
	}
	s.intent.State = domain.IntentCancelled
	s.cancelled++
	return nil
}

func (s *fixtureStore) BeginSend(ctx context.Context, reservation Reservation, checked Preflight, now time.Time) (domain.Attempt, error) {
	s.Lock()
	defer s.Unlock()
	if s.failBegin {
		return domain.Attempt{}, errors.New("synthetic disk full at write fence")
	}
	if s.paused || ctx.Err() != nil {
		return domain.Attempt{}, ErrHeld
	}
	if s.intent.State != domain.IntentReserved || reservation.Fence != 1 {
		return domain.Attempt{}, ErrConflict
	}
	s.intent.State = domain.IntentSending
	s.sendLeaseActive = true
	s.attempt = domain.Attempt{ID: fixtureAttempt, IntentID: s.intent.ID, SentAt: now, Operation: copyIntent(s.intent).Proposal.Operation, Preflight: checked.Evidence}
	returned := s.attempt
	if s.invalidJournal {
		returned.Operation.Key = "wrong-operation"
	}
	return returned, nil
}

func (s *fixtureStore) RecordDispatch(ctx context.Context, id domain.AttemptID, ack domain.Acknowledgement, _ time.Time) error {
	s.Lock()
	defer s.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failAudit {
		return errors.New("synthetic disk full")
	}
	if s.attempt.ID != id || s.intent.State != domain.IntentSending {
		return ErrConflict
	}
	s.attempt.Acknowledgement = ack
	s.sendLeaseActive = false
	switch ack.State {
	case domain.DispatchAcknowledged:
		s.intent.State = domain.IntentAcknowledged
	case domain.DispatchRejectedBeforeEffect:
		s.intent.State = domain.IntentRejectedBeforeEffect
	default:
		s.intent.State = domain.IntentUncertain
	}
	return nil
}

func (s *fixtureStore) BeginReconcile(_ context.Context, id domain.AttemptID, _ time.Time) (Reconciliation, error) {
	s.Lock()
	defer s.Unlock()
	if s.attempt.ID != id || s.sendLeaseActive || s.claimedReadback || s.intent.State == domain.IntentCompleted {
		return Reconciliation{}, ErrConflict
	}
	s.claimedReadback = true
	return Reconciliation{Attempt: s.attempt, Fence: 2}, nil
}

func (s *fixtureStore) RecordOutcome(_ context.Context, r Reconciliation, out domain.Outcome) error {
	s.Lock()
	defer s.Unlock()
	if !s.claimedReadback || s.attempt.ID != r.Attempt.ID || r.Fence != 2 {
		return ErrConflict
	}
	s.claimedReadback = false
	s.outcome = out
	s.intent.State = domain.IntentCompleted
	for _, effect := range out.Effects {
		if effect.Verification == domain.VerificationUnresolved || effect.Verification == domain.VerificationPartial {
			s.intent.State = domain.IntentReconciling
		}
	}
	return nil
}

type fixtureAdapter struct {
	Send func(context.Context, domain.Operation) (domain.Acknowledgement, error)
	Read func(context.Context, domain.Attempt) (domain.Outcome, error)
}

func (a fixtureAdapter) Dispatch(ctx context.Context, op domain.Operation) (domain.Acknowledgement, error) {
	return a.Send(ctx, op)
}

func (a fixtureAdapter) Reconcile(ctx context.Context, attempt domain.Attempt) (domain.Outcome, error) {
	return a.Read(ctx, attempt)
}

func executorFor(t *testing.T, store *fixtureStore, adapter fixtureAdapter, gate gateFunc) *Executor {
	t.Helper()
	e, err := NewExecutor(store, adapter, gate, fixedClock{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDispatchWritesOnceAndOnlyAfterJournal(t *testing.T) {
	s := &fixtureStore{intent: fixtureIntent()}
	var calls atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	e := executorFor(t, s, fixtureAdapter{Send: func(_ context.Context, op domain.Operation) (domain.Acknowledgement, error) {
		if calls.Add(1) != 1 || s.intent.State != domain.IntentSending || s.attempt.Operation.Key != op.Key || s.attempt.Preflight[0] != "fresh-evidence" {
			t.Errorf("write entered without exact committed sending marker")
		}
		close(entered)
		<-release
		return domain.Acknowledgement{State: domain.DispatchAcknowledged, UpstreamReference: "fixture-command"}, nil
	}}, freshFacts)
	done := make(chan error, 1)
	go func() { _, err := e.Dispatch(t.Context(), s.intent.ID); done <- err }()
	<-entered
	if _, err := e.Dispatch(t.Context(), s.intent.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate dispatch: %v", err)
	}
	if _, err := e.Reconcile(t.Context(), fixtureAttempt); !errors.Is(err, ErrConflict) {
		t.Errorf("reconciliation raced active send: %v", err)
	}
	close(release)
	if err := <-done; err != nil || calls.Load() != 1 || s.attempt.Acknowledgement.State != domain.DispatchAcknowledged {
		t.Fatalf("dispatch calls %d, ack %+v, err %v", calls.Load(), s.attempt.Acknowledgement, err)
	}
}

func TestHeldPreflightAndPauseNeverSend(t *testing.T) {
	for _, name := range []string{"expired-proposal", "stale-case", "capability-changed", "stale-evidence", "pause-before-send", "invalid-journal", "full-at-fence"} {
		t.Run(name, func(t *testing.T) {
			s := &fixtureStore{intent: fixtureIntent(), invalidJournal: name == "invalid-journal", failBegin: name == "full-at-fence"}
			if name == "expired-proposal" {
				s.intent.Proposal.ExpiresAt = fixtureTime
			}
			e := executorFor(t, s, fixtureAdapter{Send: func(context.Context, domain.Operation) (domain.Acknowledgement, error) {
				t.Fatal("adapter was dispatched")
				return domain.Acknowledgement{}, nil
			}}, func(ctx context.Context, intent domain.Intent) (Preflight, error) {
				checked, _ := freshFacts(ctx, intent)
				switch name {
				case "stale-case":
					checked.CaseRevision--
				case "capability-changed":
					checked.CapabilityRevision++
				case "stale-evidence":
					checked.ObservedAt = fixtureTime.Add(-time.Minute)
				case "pause-before-send":
					s.paused = true
				}
				return checked, nil
			})
			wantErr, wantCancelled := ErrHeld, 1
			switch name {
			case "expired-proposal":
				wantCancelled = 0
			case "invalid-journal", "full-at-fence":
				wantCancelled, wantErr = 0, ErrJournal
			}
			if _, err := e.Dispatch(t.Context(), s.intent.ID); !errors.Is(err, wantErr) || s.cancelled != wantCancelled {
				t.Fatalf("cancelled %d, expected %d, error %v", s.cancelled, wantCancelled, err)
			}
		})
	}
}

func TestLostAcknowledgementAndRestartReconcileWithoutResending(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		s := &fixtureStore{intent: fixtureIntent(), failAudit: failAudit}
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		a := fixtureAdapter{Send: func(context.Context, domain.Operation) (domain.Acknowledgement, error) {
			calls++
			cancel()
			return domain.Acknowledgement{State: domain.DispatchAcknowledged}, errors.New("lost acknowledgement after upstream accept")
		}, Read: func(_ context.Context, attempt domain.Attempt) (domain.Outcome, error) {
			return domain.Outcome{AttemptID: attempt.ID, ObservedAt: fixtureTime, Verification: domain.VerificationEffect,
				Effects: []domain.EffectResult{{Effect: attempt.Operation.Effects[0], Verification: domain.VerificationExternal, Evidence: []domain.EvidenceID{"library-file"}}},
			}, nil
		}}
		e := executorFor(t, s, a, freshFacts)
		id, err := e.Dispatch(ctx, s.intent.ID)
		if id != fixtureAttempt || (failAudit && !errors.Is(err, ErrJournal)) || (!failAudit && err != nil) || calls != 1 {
			t.Fatalf("dispatch id %s, calls %d, err %v", id, calls, err)
		}
		if failAudit {
			if _, err := e.Dispatch(t.Context(), s.intent.ID); !errors.Is(err, ErrHalted) {
				t.Fatalf("ledger failure did not halt dispatch: %v", err)
			}
			s.sendLeaseActive = false // a process restart after expiry of the write lease
		} else if s.attempt.Acknowledgement.State != domain.DispatchUncertain {
			t.Fatalf("unexpected acknowledgement: %+v", s.attempt.Acknowledgement)
		}
		s.paused = true
		restarted := executorFor(t, s, a, freshFacts)
		out, err := restarted.Reconcile(t.Context(), id)
		if err != nil || calls != 1 || out.Verification != domain.VerificationPartial || out.Effects[0].Verification != domain.VerificationExternal || out.Effects[1].Verification != domain.VerificationUnresolved {
			t.Fatalf("reconcile %+v, calls %d, err %v", out, calls, err)
		}
		if _, err := restarted.Dispatch(t.Context(), s.intent.ID); err == nil || calls != 1 {
			t.Fatalf("uncertainty enabled retransmission: err %v, calls %d", err, calls)
		}
	}
}

func TestOutcomeRequiresExactAndEvidencedEffects(t *testing.T) {
	attempt := domain.Attempt{ID: fixtureAttempt, Operation: fixtureIntent().Proposal.Operation, SentAt: fixtureTime}
	original := attempt.Operation.Effects[0]
	retained := attempt.Operation.Effects[1]
	readback := []domain.EffectResult{
		{Effect: original, Verification: domain.VerificationNoEffect, Evidence: []domain.EvidenceID{"verified-unchanged-file"}},
		{Effect: retained, Verification: domain.VerificationNoEffect, Evidence: []domain.EvidenceID{"verified-effect-absence"}},
	}
	out := normaliseOutcome(attempt, domain.Outcome{AttemptID: attempt.ID, ObservedAt: fixtureTime, Verification: domain.VerificationEffect, Effects: readback}, fixtureTime)
	if out.Verification != domain.VerificationNoEffect {
		t.Fatalf("expected independent no effect: %+v", out)
	}
	readback[0].Effect.Target.Instance.Epoch++
	out = normaliseOutcome(attempt, domain.Outcome{AttemptID: attempt.ID, ObservedAt: fixtureTime, Effects: readback}, fixtureTime)
	if out.Verification != domain.VerificationUnresolved || out.Effects[0].Verification != domain.VerificationUnresolved || out.Effects[1].Verification != domain.VerificationUnresolved {
		t.Fatalf("foreign target was treated as verified: %+v", out)
	}
}
