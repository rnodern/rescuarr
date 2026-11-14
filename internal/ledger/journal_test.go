package ledger

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
)

func openJournal(t *testing.T, dir string, authorize AuthorizeLocal) (*store.Store, *Journal) {
	t.Helper()
	local, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := NewJournal(local, authorize)
	if err != nil {
		_ = local.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	return local, journal
}

func fixtureAuthorize(context.Context, *sql.Tx, Approval, time.Time) error { return nil }

func setFixtureMode(t *testing.T, local *store.Store) {
	t.Helper()
	if err := local.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "UPDATE app_state SET mode = 'assist' WHERE singleton = 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func fixtureApproval() Approval {
	proposal := fixtureIntent().Proposal
	proposal.Guards = []domain.Guard{{Reason: "test_guard", Passed: true}}
	return Approval{Proposal: proposal, Actor: "owner", RequestKey: "request",
		PhysicalJob: "known-physical-job", Client: "shared-client"}
}

func authorisedFixture(t *testing.T, j *Journal, approval Approval, now time.Time) domain.Intent {
	t.Helper()
	intent, err := j.Approve(t.Context(), approval, now)
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func beginFixture(t *testing.T, j *Journal, intent domain.Intent) (Reservation, SendLease) {
	t.Helper()
	reservation, err := j.Reserve(t.Context(), intent.ID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := freshFacts(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	send, err := j.BeginSend(t.Context(), reservation, proof, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	return reservation, send
}

func TestSQLiteApprovalRequiresPolicyAndIsIdempotent(t *testing.T) {
	var revision domain.Revision = 2
	local, j := openJournal(t, filepath.Join(t.TempDir(), "state"), func(_ context.Context, _ *sql.Tx, attempted Approval, _ time.Time) error {
		if attempted.Proposal.CaseRevision != revision {
			return ErrHeld
		}
		return nil
	})
	input := fixtureApproval()
	if _, err := j.Approve(t.Context(), input, fixtureTime); !errors.Is(err, ErrHeld) {
		t.Fatalf("fresh Observe permitted approval: %v", err)
	}
	setFixtureMode(t, local)
	start := make(chan struct{})
	var threads sync.WaitGroup
	ids := make(chan domain.IntentID, 2)
	for range 2 {
		threads.Go(func() {
			<-start
			intent, err := j.Approve(t.Context(), input, fixtureTime)
			if err != nil {
				t.Error(err)
			}
			ids <- intent.ID
		})
	}
	close(start)
	threads.Wait()
	original, duplicate := <-ids, <-ids
	if original == "" || original != duplicate {
		t.Fatalf("same request created distinct intents: %q %q", original, duplicate)
	}
	changed := input
	changed.Proposal.DecisionHash = "different-hash"
	if _, err := j.Approve(t.Context(), changed, fixtureTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("different payload reused its request key: %v", err)
	}
	changed = input
	changed.RequestKey = "different-request"
	if _, err := j.Approve(t.Context(), changed, fixtureTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("proposal was approved twice: %v", err)
	}
	reservation, err := j.Reserve(t.Context(), original, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := freshFacts(t.Context(), reservation.Intent)
	if err != nil {
		t.Fatal(err)
	}
	revision++
	if _, err := j.BeginSend(t.Context(), reservation, proof, fixtureTime); !errors.Is(err, ErrHeld) {
		t.Fatalf("changed authoritative case revision reached send: %v", err)
	}
	if err := j.CancelReserved(t.Context(), reservation, "revision_changed", fixtureTime); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteSendMarkerAndPauseSurviveReopen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	local, j := openJournal(t, dir, fixtureAuthorize)
	setFixtureMode(t, local)
	input := fixtureApproval()
	intent := authorisedFixture(t, j, input, fixtureTime)
	_, send := beginFixture(t, j, intent)
	if err := j.SetPaused(t.Context(), true, "owner", fixtureTime); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local, j = openJournal(t, dir, fixtureAuthorize)

	if err := j.ReapReserved(t.Context(), fixtureTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Reserve(t.Context(), intent.ID, fixtureTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("committed send became dispatchable after reopen: %v", err)
	}
	if _, err := j.BeginReconcile(t.Context(), send.Attempt.ID, send.ExpiresAt.Add(-time.Second)); !errors.Is(err, ErrHeld) {
		t.Fatalf("readback raced the active sender: %v", err)
	}
	at := send.ExpiresAt
	reconcile, err := j.BeginReconcile(t.Context(), send.Attempt.ID, at)
	if err != nil || reconcile.Attempt.Operation.Key != input.Proposal.Operation.Key ||
		len(reconcile.Attempt.Preflight) != 1 || reconcile.Attempt.Preflight[0] != "fresh-evidence" {
		t.Fatalf("durable attempt unavailable: %+v %v", reconcile, err)
	}
	ack := domain.Acknowledgement{State: domain.DispatchAcknowledged, UpstreamReference: "123"}
	if err := j.RecordDispatch(t.Context(), send, ack, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("old send fence overwrote its successor: %v", err)
	}
	if err := j.RecordNotSent(t.Context(), send, "late_sender", at); !errors.Is(err, ErrConflict) {
		t.Fatalf("old send fence cleared its successor: %v", err)
	}
	if _, err := j.RecordOutcome(t.Context(), reconcile, verifiedReadback(send, at)); err != nil {
		t.Fatal(err)
	}
	second := input
	second.RequestKey, second.Proposal.ID, second.Proposal.Operation.Key = "next-request", "next-proposal", "next-op"
	if _, err := j.Approve(t.Context(), second, at); !errors.Is(err, ErrHeld) {
		t.Fatalf("persistent pause did not stop a new action: %v", err)
	}
	if err := j.SetPaused(t.Context(), false, "owner", at); err != nil {
		t.Fatal(err)
	}
	replay := second
	replay.Proposal.Operation.Key = input.Proposal.Operation.Key
	if _, err := j.Approve(t.Context(), replay, at); !errors.Is(err, ErrConflict) {
		t.Fatalf("new browser request reopened the completed operation: %v", err)
	}
	next := authorisedFixture(t, j, second, at)
	if _, err := j.Reserve(t.Context(), next.ID, at); err != nil {
		t.Fatalf("terminal readback did not release scopes: %v", err)
	}
	assertActionAudit(t, local, "action_sending_recorded", 1)
	assertActionAudit(t, local, "action_readback_recorded", 1)
}

func TestSQLiteLocksAndExpiredUnsentReservation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	local, j := openJournal(t, dir, fixtureAuthorize)
	setFixtureMode(t, local)
	first := fixtureApproval()
	intent := authorisedFixture(t, j, first, fixtureTime)
	reservation, err := j.Reserve(t.Context(), intent.ID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	conflict := first
	conflict.Proposal = copyIntent(intent).Proposal
	conflict.Proposal.ID, conflict.Proposal.Operation.Key, conflict.RequestKey = "alias-proposal", "alias-op", "alias-request"
	ref := domain.InstanceRef{ID: "other-arr", Epoch: 3}
	conflict.Proposal.Operation.Job.Instance = ref
	conflict.Proposal.Operation.Targets[0].Instance = ref
	conflict.Proposal.Operation.Effects[0].Target.Instance = ref
	alias := authorisedFixture(t, j, conflict, fixtureTime)
	if _, err := j.Reserve(t.Context(), alias.ID, fixtureTime); !errors.Is(err, ErrConflict) {
		t.Fatalf("shared physical download crossed scopes: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	_, j = openJournal(t, dir, fixtureAuthorize)
	expired := fixtureTime.Add(reserveLease)
	if err := j.ReapReserved(t.Context(), expired); err != nil {
		t.Fatal(err)
	}
	proof, err := freshFacts(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.BeginSend(t.Context(), reservation, proof, expired); !errors.Is(err, ErrConflict) {
		t.Fatalf("old reservation retained a send fence: %v", err)
	}
	if _, err := j.Reserve(t.Context(), alias.ID, expired); err != nil {
		t.Fatalf("reaped pre-send reservation did not release scope: %v", err)
	}
}

func TestSQLiteEffectProofPreservedAcrossReadbackFailure(t *testing.T) {
	local, j := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	intent := authorisedFixture(t, j, fixtureApproval(), fixtureTime)
	_, send := beginFixture(t, j, intent)
	if err := j.RecordDispatch(t.Context(), send, domain.Acknowledgement{State: domain.DispatchUncertain}, fixtureTime); err != nil {
		t.Fatal(err)
	}
	for step := range 3 {
		now := fixtureTime.Add(time.Duration(step+1) * time.Second)
		read, err := j.BeginReconcile(t.Context(), send.Attempt.ID, now)
		if err != nil {
			t.Fatal(err)
		}
		outcome := verifiedReadback(send, now)
		switch step {
		case 0:
			outcome.Effects = outcome.Effects[:1]
		case 1:
			outcome.Effects = nil
		case 2:
			outcome.Effects = outcome.Effects[1:]
		}
		if _, err := j.RecordOutcome(t.Context(), read, outcome); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.BeginReconcile(t.Context(), send.Attempt.ID, fixtureTime.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("separately verified effects did not complete after transient readback failures: %v", err)
	}
	assertActionAudit(t, local, "action_readback_recorded", 3)
}

func TestSQLiteExecutorReturnsCommittedReadback(t *testing.T) {
	for _, contradiction := range []bool{false, true} {
		name := "retained evidence"
		if contradiction {
			name = "contradiction"
		}
		t.Run(name, func(t *testing.T) {
			local, j := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
			setFixtureMode(t, local)
			intent := authorisedFixture(t, j, fixtureApproval(), fixtureTime)
			_, send := beginFixture(t, j, intent)
			if err := j.RecordDispatch(t.Context(), send, domain.Acknowledgement{State: domain.DispatchUncertain}, fixtureTime); err != nil {
				t.Fatal(err)
			}
			clock := &advancingClock{time: fixtureTime.Add(time.Second)}
			probes := 0
			adapter := fixtureAdapter{Read: func(_ context.Context, attempt domain.Attempt) (domain.Outcome, error) {
				probes++
				outcome := verifiedReadback(send, clock.Now())
				switch {
				case probes == 1:
					outcome.Effects = outcome.Effects[:1]
					if contradiction {
						outcome.Effects[0].Verification = domain.VerificationNoEffect
					}
				case !contradiction:
					outcome.Effects = outcome.Effects[1:]
				}
				return outcome, nil
			}}
			executor, err := NewExecutor(j, adapter, gateFunc(freshFacts), clock)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := executor.Reconcile(t.Context(), send.Attempt.ID); err != nil {
				t.Fatal(err)
			}
			clock.time = clock.time.Add(time.Second)
			outcome, err := executor.Reconcile(t.Context(), send.Attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.VerificationEffect
			if contradiction {
				want = domain.VerificationPartial
			}
			if outcome.Verification != want {
				t.Fatalf("executor reported latest probe instead of the committed %s outcome: %+v", want, outcome)
			}
			if contradiction && outcome.Effects[0].Verification != domain.VerificationNoEffect {
				t.Fatalf("executor reported a contradicted effect as verified: %+v", outcome.Effects)
			}
			if err := local.Read(t.Context(), func(tx *sql.Tx) error {
				var stored string
				if err := tx.QueryRowContext(t.Context(), "SELECT outcome FROM action_attempts WHERE id = ?", send.Attempt.ID).Scan(&stored); err != nil {
					return err
				}
				canonical, err := marshalJournal(outcome)
				if err == nil && canonical != stored {
					t.Fatalf("executor returned %s, journal committed %s", canonical, stored)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLiteAuditFailureRollsBackSendingMarker(t *testing.T) {
	local, j := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	intent := authorisedFixture(t, j, fixtureApproval(), fixtureTime)
	reservation, err := j.Reserve(t.Context(), intent.ID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER fail_fixture_audit BEFORE INSERT ON audit_events
			WHEN NEW.kind = 'action_sending_recorded' BEGIN SELECT RAISE(ABORT, 'synthetic audit failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	proof, err := freshFacts(t.Context(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.BeginSend(t.Context(), reservation, proof, fixtureTime); err == nil {
		t.Fatal("send marker returned success without audit")
	}
	if err := j.CancelReserved(t.Context(), reservation, "definitely_unsent", fixtureTime); err != nil {
		t.Fatalf("failed commit moved the reservation or introduced a sending marker: %v", err)
	}
	assertActionAudit(t, local, "action_sending_recorded", 0)
}

func verifiedReadback(send SendLease, at time.Time) domain.Outcome {
	outcome := domain.Outcome{AttemptID: send.Attempt.ID, ObservedAt: at}
	for i, effect := range send.Attempt.Operation.Effects {
		outcome.Effects = append(outcome.Effects, domain.EffectResult{Effect: effect,
			Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{domain.EvidenceID("effect-" + string(rune('a'+i)))}})
	}
	return outcome
}

func assertActionAudit(t *testing.T, local *store.Store, kind string, want int) {
	t.Helper()
	if err := local.Read(t.Context(), func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events WHERE kind = ?", kind).Scan(&count); err != nil {
			return err
		}
		if count != want {
			t.Errorf("audit %s: got %d want %d", kind, count, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteReservationBudgetPersistsAndUnsentRefunds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	local, j := openJournal(t, dir, fixtureAuthorize)
	setFixtureMode(t, local)
	var intents []domain.Intent
	for _, suffix := range []string{"one", "two", "three"} {
		a := fixtureApproval()
		a.RequestKey, a.Proposal.ID, a.Proposal.Operation.Key = suffix, domain.ProposalID(suffix), domain.OperationKey(suffix)
		intents = append(intents, authorisedFixture(t, j, a, fixtureTime))
	}
	first, err := j.Reserve(t.Context(), intents[0].ID, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.CancelReserved(t.Context(), first, "definitely_unsent", fixtureTime); err != nil {
		t.Fatal(err)
	}
	for _, intent := range intents[1:] {
		_, send := beginFixture(t, j, intent)
		ack := domain.Acknowledgement{State: domain.DispatchUncertain}
		if err := j.RecordDispatch(t.Context(), send, ack, fixtureTime); err != nil {
			t.Fatal(err)
		}
		read, err := j.BeginReconcile(t.Context(), send.Attempt.ID, fixtureTime)
		if err != nil {
			t.Fatal(err)
		}
		outcome := verifiedReadback(send, fixtureTime)
		for i := range outcome.Effects {
			outcome.Effects[i].Verification = domain.VerificationNoEffect
		}
		if _, err := j.RecordOutcome(t.Context(), read, outcome); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	_, j = openJournal(t, dir, fixtureAuthorize)

	a := fixtureApproval()
	a.RequestKey, a.Proposal.ID, a.Proposal.Operation.Key = "four", "four", "four"
	at := fixtureTime.Add(59 * time.Second)
	blocked := authorisedFixture(t, j, a, at)
	if _, err := j.Reserve(t.Context(), blocked.ID, at); !errors.Is(err, ErrHeld) {
		t.Fatalf("second process did not preserve exhausted budget: %v", err)
	}
}

func TestSQLiteSearchRequiresDurableParent(t *testing.T) {
	local, j := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	a := fixtureApproval()
	a.Proposal.Operation.Action = domain.ActionSearch
	a.Proposal.Operation.Effects = []domain.Effect{{Kind: domain.EffectSearchDispatch, Target: a.Proposal.Operation.Targets[0]}}
	intent := authorisedFixture(t, j, a, fixtureTime)
	if _, err := j.Reserve(t.Context(), intent.ID, fixtureTime); !errors.Is(err, ErrHeld) {
		t.Fatalf("unbound search had authority: %v", err)
	}
	assertActionAudit(t, local, "action_reserved", 0)
}
