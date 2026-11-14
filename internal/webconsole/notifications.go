package webconsole

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowloutbox"
)

type NotificationService interface {
	Routes(context.Context) ([]prowloutbox.Route, error)
	Configure(context.Context, prowloutbox.ConfigureRoute) (prowloutbox.Route, error)
	Recent(context.Context, domain.RouteID, int) ([]prowloutbox.Status, error)
	QueueTest(context.Context, domain.RouteID, domain.Revision, domain.ActorID) (domain.OutboxID, error)
}

type notificationRoute struct {
	prowloutbox.Route
	Recent []prowloutbox.Status
}

type notificationsView struct {
	Delivery bool
	Routes   []notificationRoute
	Blank    prowloutbox.Route
}

func (c console) notifications(w http.ResponseWriter, r *http.Request) {
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	routes, err := c.Notifications.Routes(r.Context())
	if err != nil {
		c.readError(w, r, err)
		return
	}
	body := notificationsView{Delivery: c.ProwlDelivery, Blank: prowloutbox.Route{Timezone: "UTC", Categories: 13}}
	for _, route := range routes {
		deliveries, err := c.Notifications.Recent(r.Context(), route.ID, 8)
		if err != nil {
			c.readError(w, r, err)
			return
		}
		body.Routes = append(body.Routes, notificationRoute{Route: route, Recent: deliveries})
	}
	c.render(w, r, http.StatusOK, "notifications", page{Title: "Notifications", Active: "notifications", Status: status, Body: body})
}

func (c console) notificationForm(w http.ResponseWriter, r *http.Request, fields ...string) bool {
	session := currentSession(r)
	if !sameOrigin(r, session.Origin) {
		c.notificationError(w, r, http.StatusForbidden, "Request origin rejected")
		return false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil ||
		len(r.PostForm) != len(fields)+1 {
		c.notificationError(w, r, http.StatusBadRequest, "Invalid notification form")
		return false
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
		c.notificationError(w, r, http.StatusForbidden, "Session token rejected")
		return false
	}
	for _, field := range fields {
		if !oneValue(r.PostForm, field) {
			c.notificationError(w, r, http.StatusBadRequest, "Invalid notification form")
			return false
		}
	}
	return true
}

func (c console) configureNotification(w http.ResponseWriter, r *http.Request) {
	if !c.notificationForm(w, r, "id", "revision", "key_ref", "enabled", "categories", "timezone", "quiet_start", "quiet_end", "digest") {
		return
	}
	input := r.PostForm
	id, revision, valid := notificationIdentity(input.Get("id"), input.Get("revision"))
	mask, categoryErr := strconv.ParseUint(input.Get("categories"), 10, 8)
	start, end := readMinutes(input.Get("quiet_start")), readMinutes(input.Get("quiet_end"))
	enabled, digest := input.Get("enabled"), input.Get("digest")
	if !valid || categoryErr != nil || start < 0 || end < 0 || !formBool(enabled) || !formBool(digest) {
		c.notificationError(w, r, http.StatusBadRequest, "Invalid notification settings")
		return
	}
	_, err := c.Notifications.Configure(r.Context(), prowloutbox.ConfigureRoute{
		Route: prowloutbox.Route{ID: id, Revision: revision, Enabled: enabled == "true", Categories: prowloutbox.Category(mask),
			Timezone: input.Get("timezone"), QuietStart: start, QuietEnd: end, Digest: digest == "true"},
		KeyRef: input.Get("key_ref"), Actor: currentSession(r).Actor,
	})
	c.notificationResult(w, r, err)
}

func (c console) testNotification(w http.ResponseWriter, r *http.Request) {
	if !c.notificationForm(w, r, "revision") {
		return
	}
	id, revision, valid := notificationIdentity(r.PathValue("id"), r.PostForm.Get("revision"))
	if id == "" || !valid {
		c.notificationError(w, r, http.StatusBadRequest, "Invalid notification route")
		return
	}
	if !c.ProwlDelivery {
		c.notificationError(w, r, http.StatusConflict, "Prowl delivery is disabled for this process")
		return
	}
	_, err := c.Notifications.QueueTest(r.Context(), id, revision, currentSession(r).Actor)
	c.notificationResult(w, r, err)
}

func (c console) notificationResult(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case err == nil:
		http.Redirect(w, r, c.BasePath+"/notifications", http.StatusSeeOther)
	case errors.Is(err, prowloutbox.ErrInvalid):
		c.notificationError(w, r, http.StatusBadRequest, "Check the route settings and private Prowl key file")
	case errors.Is(err, app.ErrConflict), errors.Is(err, prowloutbox.ErrDisabled), errors.Is(err, sql.ErrNoRows):
		c.notificationError(w, r, http.StatusConflict, "Notification route changed or is unavailable")
	case errors.Is(err, prowloutbox.ErrLimit):
		c.notificationError(w, r, http.StatusTooManyRequests, "Notification limit reached")
	default:
		c.readError(w, r, err)
	}
}

func (c console) notificationError(w http.ResponseWriter, r *http.Request, code int, title string) {
	c.errorPage(w, r, code, "notifications", title, "Return to Notifications to check the current settings.")
}

func notificationIdentity(id, rawRevision string) (domain.RouteID, domain.Revision, bool) {
	revision, err := strconv.ParseUint(rawRevision, 10, 64)
	valid := err == nil && ((id == "" && revision == 0) || (localID(id) && revision > 0))
	return domain.RouteID(id), domain.Revision(revision), valid
}

func formBool(value string) bool { return value == "true" || value == "false" }

func readMinutes(value string) int {
	parsed, err := time.Parse("15:04", value)
	if err != nil || len(value) != 5 {
		return -1
	}
	return parsed.Hour()*60 + parsed.Minute()
}

func clockMinutes(value int) string { return fmt.Sprintf("%02d:%02d", value/60, value%60) }
