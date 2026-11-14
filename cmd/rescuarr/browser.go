package main

import (
	"net/http"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/learningstore"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/recoveryservice"
	"github.com/rnodern/rescuarr/internal/store"
	"github.com/rnodern/rescuarr/internal/webconsole"
)

func browserHandler(cfg config.Config, auth *access.Auth, local *store.Store, observations *observeservice.Service, notifications webconsole.NotificationService, recoveryPause webconsole.RecoveryPause, manual app.ManualReviewService) http.Handler {
	patterns, _ := learningstore.New(local)
	browser := webconsole.New(webconsole.Options{
		BasePath:         cfg.BasePath,
		Read:             recoveryViews{ConsoleReader: observations, local: local},
		Connections:      observations,
		RecoveryActivity: observations,
		ImportPreview:    recoveryservice.NewPreviewService(observations, local, nil, app.SystemClock{}),
		Authorize:        app.CurrentBrowserSession,
		Notifications:    notifications,
		Learning:         patterns,
		RecoveryPause:    recoveryPause,
		ManualReview:     manual,
		ProwlDelivery:    cfg.ProwlDelivery,
	})
	return app.ConsoleHandler(cfg, auth, local, browser)
}
