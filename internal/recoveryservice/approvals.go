// Package recoveryservice connects local recovery decisions to the action journal.
package recoveryservice

import (
	"context"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
)

// ApprovalLookup resolves browser IDs into the server's current reviewed
// proposal and corroborated resource keys. The Journal independently rechecks
// local approval authority in its write transaction.
type ApprovalLookup interface {
	FindApproval(context.Context, domain.CaseID, domain.ProposalID, domain.Revision, time.Time) (ledger.Approval, error)
}

type Commands struct {
	Journal *ledger.Journal
	Lookup  ApprovalLookup
	Clock   app.Clock
}

var _ app.ConsoleCommands = Commands{}

func (c Commands) Approve(ctx context.Context, request app.ApproveProposal) (domain.Intent, error) {
	if c.Journal == nil || c.Lookup == nil || c.Clock == nil {
		return domain.Intent{}, app.ErrHeld
	}
	now := c.Clock.Now()
	approval, err := c.Lookup.FindApproval(ctx, request.CaseID, request.ProposalID, request.Revision, now)
	if err != nil {
		return domain.Intent{}, err
	}
	p := approval.Proposal
	if p.ID != request.ProposalID || p.CaseID != request.CaseID || p.CaseRevision != request.Revision {
		return domain.Intent{}, app.ErrConflict
	}
	approval.Actor = request.Actor
	approval.RequestKey = request.RequestKey
	intent, err := c.Journal.Approve(ctx, approval, now)
	switch {
	case errors.Is(err, ledger.ErrHeld):
		return domain.Intent{}, app.ErrHeld
	case errors.Is(err, ledger.ErrConflict):
		return domain.Intent{}, app.ErrConflict
	default:
		return intent, err
	}
}
