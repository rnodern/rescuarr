// Package app composes local service dependencies and defines the console boundary.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
)

type Clock interface {
	Now() time.Time
}

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

type FixedClock struct{ At time.Time }

func (c FixedClock) Now() time.Time { return c.At.UTC() }

type Overview struct {
	Revision          domain.Revision
	Mode              domain.Mode
	Paused            bool
	ReconcileRequired bool
	Cases             int
	HeldCases         int
	OpenIncidents     int
}

type CaseQuery struct {
	Instance domain.InstanceID
	State    domain.CaseState
	Reason   domain.ReasonCode
	Search   string
	After    string
	Limit    int
}

type CasePage struct {
	Cases []domain.Case
	After string
}

type CaseDetail struct {
	Case      domain.Case
	Proposals []domain.Proposal
	Outcomes  []domain.Outcome
}

type ImportPreview struct {
	Reason     domain.ReasonCode
	Candidates int
	Eligible   int
	Hold       string
	Episode    *domain.EpisodeNameReview
}

type ImportPreviewReader interface {
	ImportPreview(context.Context, domain.CaseID, domain.Revision) (ImportPreview, error)
}

type ConsoleReader interface {
	Overview(context.Context) (Overview, error)
	Cases(context.Context, CaseQuery) (CasePage, error)
	Case(context.Context, domain.CaseID) (CaseDetail, error)
	Intent(context.Context, domain.IntentID) (domain.Intent, error)
}

type ApproveProposal struct {
	CaseID     domain.CaseID
	ProposalID domain.ProposalID
	Revision   domain.Revision
	RequestKey string
	Actor      domain.ActorID
}

type ConsoleCommands interface {
	Approve(context.Context, ApproveProposal) (domain.Intent, error)
}

var (
	ErrNotFound = errors.New("record not found")
	ErrConflict = errors.New("revision conflict")
	ErrHeld     = errors.New("command held")
)
