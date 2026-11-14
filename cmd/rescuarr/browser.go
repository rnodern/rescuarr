package main

import (
	"context"
	"net/http"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/store"
	"github.com/rnodern/rescuarr/internal/webconsole"
)

func browserHandler(cfg config.Config, auth *access.Auth, local *store.Store) http.Handler {
	browser := webconsole.New(webconsole.Options{
		BasePath:  cfg.BasePath,
		Read:      localOverview{local},
		Authorize: app.CurrentBrowserSession,
	})
	return app.ConsoleHandler(cfg, auth, local, browser)
}

type localOverview struct{ store *store.Store }

func (view localOverview) Overview(ctx context.Context) (app.Overview, error) {
	state, err := view.store.State(ctx)
	return app.Overview{Mode: state.Mode, ReconcileRequired: state.ReconcileRequired}, err
}

func (localOverview) Cases(context.Context, app.CaseQuery) (app.CasePage, error) {
	return app.CasePage{}, nil
}

func (localOverview) Case(context.Context, domain.CaseID) (app.CaseDetail, error) {
	return app.CaseDetail{}, app.ErrNotFound
}

func (localOverview) Intent(context.Context, domain.IntentID) (domain.Intent, error) {
	return domain.Intent{}, app.ErrNotFound
}
