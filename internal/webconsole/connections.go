package webconsole

import (
	"context"
	"errors"
	"mime"
	"net/http"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

type ConnectionService interface {
	Instances(context.Context) ([]observeservice.InstanceStatus, error)
	AddInstance(context.Context, observeservice.InstanceInput) (domain.Instance, error)
	RemoveInstance(context.Context, domain.InstanceID, domain.ActorID) error
	Collect(context.Context, domain.InstanceID) error
}

func (c console) connections(w http.ResponseWriter, r *http.Request) {
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	instances, err := c.Connections.Instances(r.Context())
	if err != nil {
		c.readError(w, r, err)
		return
	}
	c.render(w, r, http.StatusOK, "connections", page{Title: "Connections", Active: "connections", Status: status, Body: instances})
}

func (c console) connectionForm(w http.ResponseWriter, r *http.Request, fields ...string) bool {
	if !sameOrigin(r, currentSession(r).Origin) {
		c.errorPage(w, r, http.StatusForbidden, "connections", "Request origin rejected", "Open Connections and try again.")
		return false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != len(fields)+1 {
		c.errorPage(w, r, http.StatusBadRequest, "connections", "Invalid connection form", "Open Connections and try again.")
		return false
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), currentSession(r).CSRF) {
		c.errorPage(w, r, http.StatusForbidden, "connections", "Session token rejected", "Sign in and reopen Connections.")
		return false
	}
	for _, field := range fields {
		if !oneValue(r.PostForm, field) {
			c.errorPage(w, r, http.StatusBadRequest, "connections", "Invalid connection form", "Open Connections and try again.")
			return false
		}
	}
	return true
}

func (c console) addConnection(w http.ResponseWriter, r *http.Request) {
	if !c.connectionForm(w, r, "product", "name", "url", "api_key") {
		return
	}
	_, err := c.Connections.AddInstance(r.Context(), observeservice.InstanceInput{
		Product: domain.Product(r.PostForm.Get("product")), DisplayName: r.PostForm.Get("name"),
		BaseURL: r.PostForm.Get("url"), APIKey: r.PostForm.Get("api_key"), Actor: currentSession(r).Actor,
	})
	switch {
	case errors.Is(err, observeservice.ErrInvalid):
		c.errorPage(w, r, http.StatusBadRequest, "connections", "Invalid connection", "Check the service, display name, URL and API key.")
	case errors.Is(err, observeservice.ErrConflict):
		c.errorPage(w, r, http.StatusConflict, "connections", "Connection already exists", "Open Connections to check the saved service.")
	case errors.Is(err, observeservice.ErrLimit):
		c.errorPage(w, r, http.StatusConflict, "connections", "Connection limit reached", "This installation already has the maximum number of connections.")
	case err != nil:
		c.readError(w, r, err)
	default:
		http.Redirect(w, r, c.BasePath+"/connections", http.StatusSeeOther)
	}
}

func (c console) confirmRemoveConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !localID(id) {
		c.connectionMissing(w, r)
		return
	}
	instances, err := c.Connections.Instances(r.Context())
	if err != nil {
		c.readError(w, r, err)
		return
	}
	for _, instance := range instances {
		if string(instance.ID) != id {
			continue
		}
		status, ok := c.loadStatus(w, r)
		if ok {
			c.render(w, r, http.StatusOK, "remove-connection", page{
				Title: "Remove connection", Active: "connections", Status: status, Body: instance,
			})
		}
		return
	}
	c.connectionMissing(w, r)
}

func (c console) removeConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !localID(id) {
		c.connectionMissing(w, r)
		return
	}
	if !c.connectionForm(w, r) {
		return
	}
	err := c.Connections.RemoveInstance(r.Context(), domain.InstanceID(id), currentSession(r).Actor)
	switch {
	case errors.Is(err, app.ErrNotFound):
		c.connectionMissing(w, r)
	case errors.Is(err, app.ErrConflict):
		c.errorPage(w, r, http.StatusConflict, "connections", "Connection is busy", "Wait for its queue scan or recovery to finish, then try again.")
	case errors.Is(err, observeservice.ErrCredentialCleanup):
		c.errorPage(w, r, http.StatusInternalServerError, "connections", "Connection removed", "Rescuarr could not clean up the local API key file.")
	case err != nil:
		c.readError(w, r, err)
	default:
		http.Redirect(w, r, c.BasePath+"/connections", http.StatusSeeOther)
	}
}

func (c console) connectionMissing(w http.ResponseWriter, r *http.Request) {
	c.errorPage(w, r, http.StatusNotFound, "connections", "Connection unavailable", "Choose a saved connection.")
}

func (c console) checkConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !localID(id) {
		c.connectionMissing(w, r)
		return
	}
	if !c.connectionForm(w, r) {
		return
	}
	err := c.Connections.Collect(r.Context(), domain.InstanceID(id))
	switch {
	case errors.Is(err, app.ErrNotFound):
		c.connectionMissing(w, r)
	case errors.Is(err, observeservice.ErrLimit):
		w.Header().Set("Retry-After", "3")
		c.errorPage(w, r, http.StatusTooManyRequests, "connections", "Queue checks are busy", "Try again after the current checks finish.")
	case errors.Is(err, app.ErrConflict):
		c.errorPage(w, r, http.StatusConflict, "connections", "The connection changed", "Refresh Connections before checking again.")
	case err == nil, errors.Is(err, observe.ErrConfiguration), errors.Is(err, observe.ErrAccess),
		errors.Is(err, observe.ErrUnavailable), errors.Is(err, observe.ErrNotFound), errors.Is(err, observe.ErrRateLimited),
		errors.Is(err, observe.ErrResponse), errors.Is(err, observe.ErrPartial):
		// Observe has persisted the upstream outcome, including failed reads.
		http.Redirect(w, r, c.BasePath+"/connections", http.StatusSeeOther)
	default:
		c.readError(w, r, err)
	}
}

func connectionProduct(product domain.Product) string {
	switch product {
	case domain.ProductSonarr:
		return "Sonarr"
	case domain.ProductRadarr:
		return "Radarr"
	default:
		return "Unknown product"
	}
}

func connectionStatus(status domain.ReasonCode) string {
	switch status {
	case "awaiting_scan":
		return "Awaiting first check"
	case "available":
		return "Available"
	case "partial_queue":
		return "Queue read incomplete"
	case "authentication_failed":
		return "API key rejected or unavailable"
	case "rate_limited":
		return "Rate limited"
	case "contract_changed":
		return "Version changed. Check again to read the queue."
	case "unsupported_payload":
		return "Unsupported response"
	case "upstream_unavailable":
		return "Service unavailable"
	default:
		return "Status unknown"
	}
}

func connectionCoverage(coverage domain.ObservationState) string {
	switch coverage {
	case domain.ObservationKnown:
		return "Complete"
	case "partial":
		return "Partial"
	case domain.ObservationStale:
		return "Stale"
	default:
		return "Unknown"
	}
}
