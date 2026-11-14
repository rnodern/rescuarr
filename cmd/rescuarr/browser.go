package main

import (
	"net/http"

	"github.com/rnodern/rescuarr/internal/access"
	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/config"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/prowloutbox"
	"github.com/rnodern/rescuarr/internal/store"
	"github.com/rnodern/rescuarr/internal/webconsole"
)

func browserHandler(cfg config.Config, auth *access.Auth, local *store.Store, observations *observeservice.Service, notifications *prowloutbox.Service) http.Handler {
	browser := webconsole.New(webconsole.Options{
		BasePath:      cfg.BasePath,
		Read:          observations,
		Connections:   observations,
		Authorize:     app.CurrentBrowserSession,
		Notifications: notifications,
		ProwlDelivery: cfg.ProwlDelivery,
	})
	return app.ConsoleHandler(cfg, auth, local, browser)
}
