package app

import (
	"context"

	"github.com/rnodern/rescuarr/internal/domain"
)

type ManualReviewService interface {
	SetManualMode(context.Context, bool, domain.ActorID) error
	ManualReview(context.Context) (ManualReviewView, error)
	DecideManualReview(context.Context, ManualDecision) error
}

type ManualReviewView struct {
	Case              *domain.Case
	Preview           ImportPreview
	Token             string
	ApprovalToken     string
	Hold              string
	Diagnosis         string
	CanApprove        bool
	Processing        bool
	ImportAction      bool
	CanCloseUncertain bool
	Folder            string
	SearchLabels      []string
}

type ManualDecision struct {
	Token          string
	Approve        bool
	CloseUncertain bool
	Actor          domain.ActorID
}
