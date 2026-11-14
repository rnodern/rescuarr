// Package apptest contains deterministic fakes for console and executor tests.
package apptest

import (
	"context"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type Console struct {
	Dashboard app.Overview
	Page      app.CasePage
	Details   map[domain.CaseID]app.CaseDetail
	Intents   map[domain.IntentID]domain.Intent
	OnApprove func(context.Context, app.ApproveProposal) (domain.Intent, error)
}

var _ app.ConsoleReader = (*Console)(nil)
var _ app.ConsoleCommands = (*Console)(nil)

func (f *Console) Overview(context.Context) (app.Overview, error) {
	return f.Dashboard, nil
}

func (f *Console) Cases(context.Context, app.CaseQuery) (app.CasePage, error) {
	return f.Page, nil
}

func (f *Console) Case(_ context.Context, id domain.CaseID) (app.CaseDetail, error) {
	detail, ok := f.Details[id]
	if !ok {
		return app.CaseDetail{}, app.ErrNotFound
	}
	return detail, nil
}

func (f *Console) Intent(_ context.Context, id domain.IntentID) (domain.Intent, error) {
	intent, ok := f.Intents[id]
	if !ok {
		return domain.Intent{}, app.ErrNotFound
	}
	return intent, nil
}

func (f *Console) Approve(ctx context.Context, request app.ApproveProposal) (domain.Intent, error) {
	if f.OnApprove == nil {
		return domain.Intent{}, app.ErrHeld
	}
	return f.OnApprove(ctx, request)
}

type EffectAdapter struct {
	OnDispatch  func(context.Context, domain.Operation) (domain.Acknowledgement, error)
	OnReconcile func(context.Context, domain.Attempt) (domain.Outcome, error)
}

var _ domain.EffectAdapter = EffectAdapter{}

func (f EffectAdapter) Dispatch(ctx context.Context, operation domain.Operation) (domain.Acknowledgement, error) {
	if f.OnDispatch == nil {
		return domain.Acknowledgement{}, app.ErrHeld
	}
	return f.OnDispatch(ctx, operation)
}

func (f EffectAdapter) Reconcile(ctx context.Context, attempt domain.Attempt) (domain.Outcome, error) {
	if f.OnReconcile == nil {
		return domain.Outcome{}, app.ErrHeld
	}
	return f.OnReconcile(ctx, attempt)
}
