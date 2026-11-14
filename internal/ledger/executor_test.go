package ledger

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

type advancingClock struct{ time time.Time }

func (c *advancingClock) Now() time.Time { return c.time }

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
		ArrOwnsImplicitSearch: intent.Proposal.Operation.Action == domain.ActionReject && slices.Contains(intent.Proposal.Operation.Effects, domain.Effect{Kind: domain.EffectSearchDispatch}),
		Evidence:              []domain.EvidenceID{"fresh-evidence"}, ObservedAt: fixtureTime.Add(-time.Second), ExpiresAt: fixtureTime.Add(time.Second)}, nil
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
	sendLeaseExpiry time.Time
	afterBegin      func()
	failAudit       bool
	failBegin       bool
	invalidJournal  bool
	cancelled       int
	notSent         int
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

func (s *fixtureStore) BeginSend(ctx context.Context, reservation Reservation, checked Preflight, now time.Time) (SendLease, error) {
	s.Lock()
	defer s.Unlock()
	if s.failBegin {
		return SendLease{}, errors.New("synthetic disk full at write fence")
	}
	if s.paused || ctx.Err() != nil {
		return SendLease{}, ErrHeld
	}
	if s.intent.State != domain.IntentReserved || reservation.Fence != 1 {
		return SendLease{}, ErrConflict
	}
	s.intent.State = domain.IntentSending
	s.sendLeaseActive = true
	s.attempt = domain.Attempt{ID: fixtureAttempt, IntentID: s.intent.ID, SentAt: now, Operation: copyIntent(s.intent).Proposal.Operation, Preflight: checked.Evidence}
	returned := s.attempt
	if s.invalidJournal {
		returned.Operation.Key = "wrong-operation"
	}
	expiry := s.sendLeaseExpiry
	if expiry.IsZero() {
		expiry = now.Add(time.Minute)
	}
	if s.afterBegin != nil {
		s.afterBegin()
	}
	return SendLease{Attempt: returned, Fence: 3, ExpiresAt: expiry}, nil
}

func (s *fixtureStore) RecordNotSent(ctx context.Context, lease SendLease, _ domain.ReasonCode, _ time.Time) error {
	s.Lock()
	defer s.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.attempt.ID != lease.Attempt.ID || lease.Fence != 3 || s.intent.State != domain.IntentSending || !s.sendLeaseActive {
		return ErrConflict
	}
	s.intent.State = domain.IntentCancelled
	s.sendLeaseActive = false
	s.notSent++
	return nil
}

func (s *fixtureStore) RecordDispatch(ctx context.Context, lease SendLease, ack domain.Acknowledgement, _ time.Time) error {
	s.Lock()
	defer s.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failAudit {
		return errors.New("synthetic disk full")
	}
	if s.attempt.ID != lease.Attempt.ID || lease.Fence != 3 || s.intent.State != domain.IntentSending {
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
	if s.attempt.ID != id || s.sendLeaseActive || s.claimedReadback || s.intent.State == domain.IntentCompleted || s.intent.State == domain.IntentCancelled {
		return Reconciliation{}, ErrConflict
	}
	s.claimedReadback = true
	return Reconciliation{Attempt: s.attempt, Fence: 2}, nil
}

func (s *fixtureStore) RecordOutcome(_ context.Context, r Reconciliation, out domain.Outcome) (domain.Outcome, error) {
	s.Lock()
	defer s.Unlock()
	if !s.claimedReadback || s.attempt.ID != r.Attempt.ID || r.Fence != 2 {
		return domain.Outcome{}, ErrConflict
	}
	s.claimedReadback = false
	s.outcome = out
	s.intent.State = domain.IntentCompleted
	for _, effect := range out.Effects {
		if effect.Verification == domain.VerificationUnresolved || effect.Verification == domain.VerificationPartial {
			s.intent.State = domain.IntentReconciling
		}
	}
	return s.outcome, nil
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

func TestPostFenceExpiryRecordsDefiniteNoSend(t *testing.T) {
	for _, name := range []string{"proposal", "preflight", "lease"} {
		t.Run(name, func(t *testing.T) {
			clock := &advancingClock{time: fixtureTime}
			s := &fixtureStore{intent: fixtureIntent(), afterBegin: func() { clock.time = clock.time.Add(2 * time.Second) }}
			if name == "proposal" {
				s.intent.Proposal.ExpiresAt = fixtureTime.Add(time.Second)
			}
			if name == "lease" {
				s.sendLeaseExpiry = fixtureTime.Add(operationTimeout + time.Second)
			}
			gate := gateFunc(func(ctx context.Context, intent domain.Intent) (Preflight, error) {
				proof, err := freshFacts(ctx, intent)
				if name != "preflight" {
					proof.ExpiresAt = fixtureTime.Add(time.Minute)
				}
				return proof, err
			})
			adapter := fixtureAdapter{Send: func(context.Context, domain.Operation) (domain.Acknowledgement, error) {
				t.Fatal("expired proof or lease reached adapter")
				return domain.Acknowledgement{}, nil
			}}
			e, err := NewExecutor(s, adapter, gate, clock)
			if err != nil {
				t.Fatal(err)
			}
			id, err := e.Dispatch(t.Context(), s.intent.ID)
			if id != fixtureAttempt || !errors.Is(err, ErrHeld) || s.notSent != 1 || s.intent.State != domain.IntentCancelled {
				t.Fatalf("id %v, state %v, no send %d, err %v", id, s.intent.State, s.notSent, err)
			}
			if _, err := e.Reconcile(t.Context(), id); !errors.Is(err, ErrConflict) {
				t.Fatalf("definitely unsent attempt should not need upstream reconciliation: %v", err)
			}
		})
	}
}

func TestImplicitRejectSearchRequiresExactPreflightAuthority(t *testing.T) {
	for _, authority := range []bool{false, true} {
		s := &fixtureStore{intent: fixtureIntent()}
		op := &s.intent.Proposal.Operation
		op.Action = domain.ActionReject
		op.Effects = []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}, {Kind: domain.EffectSourceRetained}, {Kind: domain.EffectSearchDispatch}}
		calls := 0
		e := executorFor(t, s, fixtureAdapter{Send: func(_ context.Context, op domain.Operation) (domain.Acknowledgement, error) {
			calls++
			if op.Action != domain.ActionReject {
				t.Fatal("implicit search became an explicit dispatch")
			}
			return domain.Acknowledgement{State: domain.DispatchAcknowledged}, nil
		}}, func(ctx context.Context, intent domain.Intent) (Preflight, error) {
			proof, err := freshFacts(ctx, intent)
			proof.ArrOwnsImplicitSearch = authority
			return proof, err
		})
		_, err := e.Dispatch(t.Context(), s.intent.ID)
		if (authority && (err != nil || calls != 1)) || (!authority && (!errors.Is(err, ErrHeld) || calls != 0)) {
			t.Fatalf("authority %t, calls %d, error %v", authority, calls, err)
		}
	}
}

func TestPreflightEvidenceEnvelopeAllowsSeasonPacks(t *testing.T) {
	intent := fixtureIntent()
	proof, err := freshFacts(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPreflightFacts-1; i++ {
		proof.Evidence = append(proof.Evidence, domain.EvidenceID(fmt.Sprintf("pack-evidence-%d", i)))
	}
	if !eligible(intent.Proposal, proof, fixtureTime) {
		t.Fatal("valid pack evidence within the bounded envelope was refused")
	}
	proof.Evidence = append(proof.Evidence, "too-many")
	if eligible(intent.Proposal, proof, fixtureTime) {
		t.Fatal("unbounded evidence was accepted")
	}
}

func TestOperationRejectNeedsExactCompatibleEffects(t *testing.T) {
	op := fixtureIntent().Proposal.Operation
	op.Action = domain.ActionReject
	base := []domain.Effect{{Kind: domain.EffectQueueRemoval}, {Kind: domain.EffectBlocklist}}
	for _, effects := range [][]domain.Effect{
		append(slices.Clone(base), domain.Effect{Kind: domain.EffectDataRemoval}),
		append(slices.Clone(base), domain.Effect{Kind: domain.EffectClientRemoval}, domain.Effect{Kind: domain.EffectSourceRetained}),
		{{Kind: domain.EffectSourceRetained}},
	} {
		op.Effects = effects
		if validOperation(op) {
			t.Fatalf("unexpected effect combination allowed: %+v", effects)
		}
	}
	op.Effects = append(slices.Clone(base), domain.Effect{Kind: domain.EffectDataRemoval}, domain.Effect{Kind: domain.EffectClientRemoval})
	if !validOperation(op) {
		t.Fatal("complete reject-and-remove effect set refused")
	}
}
