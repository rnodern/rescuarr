package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

func schedulerFixture(t *testing.T, journal *Journal, clock Clock, adapter fixtureAdapter) *Scheduler {
	t.Helper()
	gate := gateFunc(func(ctx context.Context, intent domain.Intent) (Preflight, error) {
		proof, err := freshFacts(ctx, intent)
		proof.ObservedAt = clock.Now()
		proof.ExpiresAt = proof.ObservedAt.Add(time.Minute)
		return proof, err
	})
	executor, err := NewExecutor(journal, adapter, gate, clock)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(executor)
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func separateScopeApproval(id string) Approval {
	approval := fixtureApproval()
	approval.Proposal.ID = domain.ProposalID(id + "-proposal")
	approval.Proposal.Operation.Key = domain.OperationKey(id + "-operation")
	approval.RequestKey = id + "-request"
	op := &approval.Proposal.Operation
	op.Job.Instance.ID = domain.InstanceID(id)
	op.Targets[0].Instance = op.Job.Instance
	op.Effects[0].Target = op.Targets[0]
	approval.PhysicalJob, approval.Client = id+"-physical-job", id+"-client"
	return approval
}

func TestSchedulerPacesReadbackAcrossRestartAndPause(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	local, journal := openJournal(t, dir, fixtureAuthorize)
	setFixtureMode(t, local)
	intent := authorisedFixture(t, journal, fixtureApproval(), fixtureTime)
	clock := &advancingClock{time: fixtureTime}
	var sends, probes int
	var attempted domain.AttemptID
	adapter := fixtureAdapter{
		Send: func(context.Context, domain.Operation) (domain.Acknowledgement, error) {
			sends++
			return domain.Acknowledgement{}, errors.New("response lost")
		},
		Read: func(_ context.Context, attempt domain.Attempt) (domain.Outcome, error) {
			probes++
			attempted = attempt.ID
			if attempt.IntentID != intent.ID || attempt.Acknowledgement.State != domain.DispatchUncertain {
				t.Errorf("readback observed an unexpected attempt: %+v", attempt)
			}
			if probes == 1 {
				return domain.Outcome{}, errors.New("read temporarily unavailable")
			}
			return verifiedReadback(SendLease{Attempt: attempt}, clock.Now()), nil
		},
	}
	scheduler := schedulerFixture(t, journal, clock, adapter)
	var cursor dispatchCursor
	if err := scheduler.dispatch(t.Context(), &cursor); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("got %d dispatches, want one", sends)
	}
	for _, offset := range []time.Duration{initialReadback - time.Nanosecond, initialReadback} {
		clock.time = fixtureTime.Add(offset)
		if err := scheduler.readback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if offset < initialReadback && probes != 0 {
			t.Fatal("readback ran before the initial delay")
		}
	}
	if probes != 1 || attempted == "" {
		t.Fatalf("got %d initial readbacks for %q", probes, attempted)
	}
	blocked := fixtureApproval()
	blocked.Proposal.ID, blocked.Proposal.Operation.Key, blocked.RequestKey = "pending-proposal", "pending-operation", "pending-request"
	pending := authorisedFixture(t, journal, blocked, clock.Now())
	if err := journal.SetPaused(t.Context(), true, "owner", clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	local, journal = openJournal(t, dir, fixtureAuthorize)
	scheduler = schedulerFixture(t, journal, clock, adapter)
	cursor = dispatchCursor{}
	due := clock.Now().Add(readbackDelay(attempted, 1))
	clock.time = due.Add(-time.Nanosecond)
	if err := scheduler.readback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Fatal("restart bypassed persisted readback pacing")
	}
	clock.time = due
	if err := scheduler.readback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if probes != 2 {
		t.Fatalf("got %d readbacks, want two while paused", probes)
	}
	if err := scheduler.dispatch(t.Context(), &cursor); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatalf("sent %d times across uncertainty, restart and pause", sends)
	}
	if err := local.Read(t.Context(), func(tx *sql.Tx) error {
		row, err := readIntent(t.Context(), tx, pending.ID)
		if err == nil && row.intent.State != domain.IntentAuthorized {
			t.Fatalf("paused pending intent advanced to %s", row.intent.State)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertActionAudit(t, local, "action_sending_recorded", 1)
	assertActionAudit(t, local, "action_readback_recorded", 2)
}

func TestSchedulerRecoversAfterLeaseExpiryAndReapsUnsent(t *testing.T) {
	local, journal := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	sent := authorisedFixture(t, journal, fixtureApproval(), fixtureTime)
	_, lease := beginFixture(t, journal, sent)
	reserved := authorisedFixture(t, journal, separateScopeApproval("other"), fixtureTime)
	if _, err := journal.Reserve(t.Context(), reserved.ID, fixtureTime); err != nil {
		t.Fatal(err)
	}
	clock := &advancingClock{time: lease.ExpiresAt.Add(-time.Nanosecond)}
	probes := 0
	scheduler := schedulerFixture(t, journal, clock, fixtureAdapter{
		Send: func(context.Context, domain.Operation) (domain.Acknowledgement, error) {
			t.Fatal("scheduled a send after lease recovery")
			return domain.Acknowledgement{}, nil
		},
		Read: func(_ context.Context, attempt domain.Attempt) (domain.Outcome, error) {
			probes++
			if attempt.ID != lease.Attempt.ID || attempt.Acknowledgement != (domain.Acknowledgement{}) {
				t.Errorf("readback did not recover the active sending marker: %+v", attempt)
			}
			return verifiedReadback(lease, clock.Now()), nil
		},
	})
	if err := scheduler.readback(t.Context()); err != nil || probes != 0 {
		t.Fatalf("active sending lease was not respected: %d, %v", probes, err)
	}
	clock.time = lease.ExpiresAt
	var cursor dispatchCursor
	if err := scheduler.dispatch(t.Context(), &cursor); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.readback(t.Context()); err != nil || probes != 1 {
		t.Fatalf("expired sending marker was not reconciled: %d, %v", probes, err)
	}
	assertActionAudit(t, local, "action_cancelled_unsent", 1)
	assertActionAudit(t, local, "action_sending_recorded", 1)
	assertActionAudit(t, local, "action_readback_recorded", 1)
}

func TestSchedulerDispatchMovesPastHeldAuthorisations(t *testing.T) {
	var checked bool
	local, journal := openJournal(t, filepath.Join(t.TempDir(), "state"), func(_ context.Context, _ *sql.Tx, approval Approval, _ time.Time) error {
		if checked && approval.Proposal.Operation.Key != "available-operation" {
			return ErrHeld
		}
		return nil
	})
	setFixtureMode(t, local)
	for n := range schedulerPageSize + 1 {
		approval := fixtureApproval()
		suffix := fmt.Sprintf("-%02d", n)
		approval.Proposal.ID += domain.ProposalID(suffix)
		approval.Proposal.Operation.Key += domain.OperationKey(suffix)
		approval.RequestKey += suffix
		if n == schedulerPageSize {
			approval.Proposal.Operation.Key = "available-operation"
		}
		authorisedFixture(t, journal, approval, fixtureTime.Add(time.Duration(n)*time.Nanosecond))
	}
	checked = true
	sends := 0
	scheduler := schedulerFixture(t, journal, &advancingClock{time: fixtureTime.Add(time.Second)}, fixtureAdapter{
		Send: func(_ context.Context, op domain.Operation) (domain.Acknowledgement, error) {
			sends++
			if op.Key != "available-operation" {
				t.Errorf("dispatched a held operation: %s", op.Key)
			}
			return domain.Acknowledgement{State: domain.DispatchAcknowledged}, nil
		},
	})
	var cursor dispatchCursor
	for range schedulerPageSize + 1 {
		if err := scheduler.dispatch(t.Context(), &cursor); err != nil {
			t.Fatal(err)
		}
	}
	if sends != 1 {
		t.Fatalf("held authorisations left later work waiting: %d sends, want one", sends)
	}
}

func TestSchedulerExpiresUnsentAuthorisationWhilePaused(t *testing.T) {
	local, journal := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	initial := fixtureApproval()
	intent := authorisedFixture(t, journal, initial, fixtureTime)
	expires := intent.Proposal.ExpiresAt
	if err := journal.SetPaused(t.Context(), true, "owner", expires); err != nil {
		t.Fatal(err)
	}
	scheduler := schedulerFixture(t, journal, &advancingClock{time: expires}, fixtureAdapter{})
	var cursor dispatchCursor
	if err := scheduler.dispatch(t.Context(), &cursor); err != nil {
		t.Fatal(err)
	}
	assertActionAudit(t, local, "action_cancelled_unsent", 1)
	if err := journal.SetPaused(t.Context(), false, "owner", expires); err != nil {
		t.Fatal(err)
	}
	initial.Proposal.ID, initial.RequestKey = "new-proposal", "new-request"
	initial.Proposal.ExpiresAt = expires.Add(time.Minute)
	authorisedFixture(t, journal, initial, expires)
}

func TestSchedulerAlternatesDueReadbacksAndHonoursDispatchHalt(t *testing.T) {
	local, journal := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	first := authorisedFixture(t, journal, fixtureApproval(), fixtureTime)
	_, firstSend := beginFixture(t, journal, first)
	second := authorisedFixture(t, journal, separateScopeApproval("other"), fixtureTime)
	_, secondSend := beginFixture(t, journal, second)
	for _, send := range []SendLease{firstSend, secondSend} {
		if err := journal.RecordDispatch(t.Context(), send, domain.Acknowledgement{State: domain.DispatchUncertain}, fixtureTime); err != nil {
			t.Fatal(err)
		}
	}
	clock := &advancingClock{time: fixtureTime.Add(initialReadback)}
	var probes []domain.AttemptID
	scheduler := schedulerFixture(t, journal, clock, fixtureAdapter{
		Read: func(_ context.Context, attempt domain.Attempt) (domain.Outcome, error) {
			probes = append(probes, attempt.ID)
			return domain.Outcome{}, errors.New("read unavailable")
		},
	})
	scheduler.executor.halted.Store(true)
	if err := scheduler.dispatch(t.Context(), &dispatchCursor{}); !errors.Is(err, ErrHalted) {
		t.Fatalf("dispatch bypassed the executor's halt: %v", err)
	}
	for range 3 {
		if err := scheduler.readback(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(probes) != 2 || probes[0] == probes[1] {
		t.Fatalf("unresolved readback starved the other due attempt: %v", probes)
	}
	assertActionAudit(t, local, "action_readback_recorded", 2)
}

func TestSchedulerRunStartsReadbackWhileDispatchIsBlocked(t *testing.T) {
	local, journal := openJournal(t, filepath.Join(t.TempDir(), "state"), fixtureAuthorize)
	setFixtureMode(t, local)
	sent := authorisedFixture(t, journal, fixtureApproval(), fixtureTime)
	_, send := beginFixture(t, journal, sent)
	if err := journal.RecordDispatch(t.Context(), send, domain.Acknowledgement{State: domain.DispatchUncertain}, fixtureTime); err != nil {
		t.Fatal(err)
	}
	authorisedFixture(t, journal, separateScopeApproval("other"), fixtureTime)
	clock := &advancingClock{time: fixtureTime.Add(initialReadback)}
	dispatchStarted, readbackStarted := make(chan struct{}), make(chan struct{})
	scheduler := schedulerFixture(t, journal, clock, fixtureAdapter{
		Send: func(ctx context.Context, _ domain.Operation) (domain.Acknowledgement, error) {
			close(dispatchStarted)
			<-ctx.Done()
			return domain.Acknowledgement{}, ctx.Err()
		},
		Read: func(ctx context.Context, _ domain.Attempt) (domain.Outcome, error) {
			select {
			case <-dispatchStarted:
			case <-ctx.Done():
				return domain.Outcome{}, ctx.Err()
			}
			close(readbackStarted)
			return verifiedReadback(send, clock.Now()), nil
		},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx, nil) }()
	select {
	case <-readbackStarted:
	case <-ctx.Done():
		t.Fatal("readback did not start alongside the blocked dispatch")
	}
	if err := scheduler.Run(ctx, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("second scheduler run returned %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertActionAudit(t, local, "action_sending_recorded", 2)
	assertActionAudit(t, local, "action_readback_recorded", 1)
}
