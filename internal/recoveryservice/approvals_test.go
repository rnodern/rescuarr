package recoveryservice

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/store"
)

type approvalFixture ledger.Approval

func (a approvalFixture) FindApproval(context.Context, domain.CaseID, domain.ProposalID, domain.Revision, time.Time) (ledger.Approval, error) {
	return ledger.Approval(a), nil
}

func TestRecoveryCommandsAuthorisesOnlyCurrentServerProposal(t *testing.T) {
	ctx := t.Context()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	now := time.Unix(12_000, 0).UTC()
	instance := domain.InstanceRef{ID: "fixture-arr", Epoch: 1}
	target := domain.TargetRef{Instance: instance, Kind: domain.TargetMovie, ID: 3}
	p := domain.Proposal{
		ID: "proposal", CaseID: "case", CaseRevision: 1, PolicyRevision: 2, CapabilityRevision: 3,
		DecisionHash: "decision", ExpiresAt: now.Add(time.Minute),
		Guards: []domain.Guard{{Reason: "eligible", Passed: true}},
		Operation: domain.Operation{
			Key: "operation", Job: domain.JobRef{Instance: instance, ID: "job", Generation: 1},
			Action: domain.ActionImport, Targets: []domain.TargetRef{target},
			Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}},
		},
	}
	authority := true
	journal, err := ledger.NewJournal(local, func(_ context.Context, _ *sql.Tx, a ledger.Approval, _ time.Time) error {
		if !authority {
			return ledger.ErrHeld
		}
		if a.Actor != "owner" || a.RequestKey != "browser-request" || a.PhysicalJob != "physical-job" || a.Client != "client" {
			t.Fatal("incorrect local approval attribution")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := Commands{Journal: journal, Clock: app.FixedClock{At: now}, Lookup: approvalFixture{
		Proposal: p, PhysicalJob: "physical-job", Client: "client", Actor: "ignored", RequestKey: "ignored",
	}}
	request := app.ApproveProposal{Actor: "owner", RequestKey: "browser-request", CaseID: p.CaseID, ProposalID: p.ID, Revision: p.CaseRevision}
	if _, err := commands.Approve(ctx, request); !errors.Is(err, app.ErrHeld) {
		t.Fatalf("Observe mode returned %v", err)
	}
	if err := local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE app_state SET mode = 'assist' WHERE singleton = 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request.Revision++
	if _, err := commands.Approve(ctx, request); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("stale browser revision returned %v", err)
	}
	request.Revision--
	authority = false
	if _, err := commands.Approve(ctx, request); !errors.Is(err, app.ErrHeld) {
		t.Fatalf("stale local authority returned %v", err)
	}
	authority = true
	first, err := commands.Approve(ctx, request)
	if err != nil || first.ID == "" || first.State != domain.IntentAuthorized {
		t.Fatalf("current approval did not commit: %v", err)
	}
	second, err := commands.Approve(ctx, request)
	if err != nil || second.ID != first.ID {
		t.Fatalf("identical browser submission changed committed intent: %v", err)
	}
}
