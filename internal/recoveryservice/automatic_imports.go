package recoveryservice

import (
	"context"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/policy"
)

// AutomaticImportPlanner assesses fresh, qualified evidence. Holds and review
// decisions must be returned without an autonomous proposal.
type AutomaticImportPlanner func(context.Context, domain.Case) (policy.Result, string, string, error)

type AutomaticImportQueue interface {
	AutomaticImportCases(context.Context, domain.CaseID, int) ([]domain.Case, error)
}

// RunAutomaticImports admits one qualified case at a time. The ledger
// scheduler owns writes and reconciliation. An uncertain import keeps its
// durable slot even across process restarts.
func RunAutomaticImports(ctx context.Context, queue AutomaticImportQueue, plan AutomaticImportPlanner, journal *ledger.Journal, scheduler *ledger.Scheduler, clock app.Clock, onError func(error)) error {
	if queue == nil || plan == nil || journal == nil || scheduler == nil || clock == nil {
		return ledger.ErrInvalid
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	finished := make(chan error, 1)
	go func() { finished <- scheduler.Run(ctx, onError) }()
	timer := time.NewTimer(0)
	defer timer.Stop()
	var after domain.CaseID
	for {
		select {
		case err := <-finished:
			return err
		case <-ctx.Done():
			return <-finished
		case <-timer.C:
			pass, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := admitAutomaticImport(pass, queue, plan, journal, clock, &after)
			cancel()
			if err != nil && ctx.Err() == nil && onError != nil {
				onError(err)
			}
			timer.Reset(5 * time.Second)
		}
	}
}

func admitAutomaticImport(ctx context.Context, queue AutomaticImportQueue, plan AutomaticImportPlanner, journal *ledger.Journal, clock app.Clock, after *domain.CaseID) error {
	ready, err := journal.CanPlanAutomaticImport(ctx)
	if !ready || err != nil {
		return err
	}
	cases, err := queue.AutomaticImportCases(ctx, *after, 1)
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		*after = ""
		return nil
	}
	item := cases[0]
	*after = item.ID
	result, job, client, err := plan(ctx, item)
	if err != nil {
		return err
	}
	proposal := result.Proposal
	if result.Authority != policy.AuthorityAutonomous || result.Path != policy.ImportOriginal ||
		proposal == nil || proposal.CaseID != item.ID || proposal.CaseRevision != item.Revision || proposal.Operation.Job != item.Job.Ref {
		return nil
	}
	_, err = journal.AuthorizeAutomaticImport(ctx, ledger.Approval{Proposal: *proposal, PhysicalJob: job, Client: client}, clock.Now())
	if errors.Is(err, ledger.ErrHeld) || errors.Is(err, ledger.ErrConflict) {
		return nil
	}
	return err
}
