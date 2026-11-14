package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/prowloutbox"
)

type notificationFixture struct {
	saved    []prowloutbox.ConfigureRoute
	queued   int
	disabled int
}

func (f *notificationFixture) Routes(context.Context) ([]prowloutbox.Route, error) {
	return []prowloutbox.Route{{ID: "route-1", Revision: 2, Enabled: true,
		Categories: 13, QuietStart: 22 * 60, QuietEnd: 7 * 60, Timezone: "Australia/Melbourne"}}, nil
}

func (f *notificationFixture) Recent(context.Context, domain.RouteID, int) ([]prowloutbox.Status, error) {
	return []prowloutbox.Status{{Category: "test", State: domain.OutboxAccepted, Scheduled: time.Unix(1, 0)}}, nil
}

func (f *notificationFixture) Configure(_ context.Context, input prowloutbox.ConfigureRoute) (prowloutbox.Route, error) {
	f.saved = append(f.saved, input)
	if input.Route.Revision != 2 {
		return prowloutbox.Route{}, app.ErrConflict
	}
	return input.Route, nil
}

func (f *notificationFixture) Disable(_ context.Context, route domain.RouteID, revision domain.Revision, actor domain.ActorID) (prowloutbox.Route, error) {
	if route != "route-1" || revision != 2 || actor != "owner" {
		return prowloutbox.Route{}, app.ErrConflict
	}
	f.disabled++
	return prowloutbox.Route{ID: route, Revision: 3}, nil
}

func (f *notificationFixture) QueueTest(_ context.Context, route domain.RouteID, revision domain.Revision, actor domain.ActorID) (domain.OutboxID, error) {
	if route != "route-1" || revision != 2 || actor != "owner" {
		return "", app.ErrConflict
	}
	f.queued++
	return "fixture-delivery", nil
}

func TestNotificationRoutesAreOwnerOnlyAndExplicit(t *testing.T) {
	options := fixture()
	backend := &notificationFixture{}
	options.Notifications = backend
	handler := New(options)
	route := "/notifications/route-1/test"
	expect(t, request(handler, http.MethodGet, "/notifications", "", "", nil), http.StatusUnauthorized)
	reply := request(handler, http.MethodGet, "/notifications", "owner", "", nil)
	expect(t, reply, http.StatusOK, "Prowl delivery is off", "test · accepted", "Australia/Melbourne", "22:00")
	if strings.Contains(reply.Body.String(), "Queue test message") || !strings.Contains(reply.Body.String(), "</html>") {
		t.Fatalf("disabled notification route rendered an interactive test or incomplete form: %s", reply.Body.String())
	}
	validTest := url.Values{"csrf": {csrf}, "revision": {"2"}}
	expect(t, request(handler, http.MethodPost, route, "owner", origin, validTest), http.StatusConflict)
	if backend.queued != 0 {
		t.Fatal("disabled delivery allowed a test")
	}
	expect(t, request(handler, http.MethodPost, "/notifications/route-1/disable", "owner", origin, validTest), http.StatusSeeOther)
	if backend.disabled != 1 {
		t.Fatal("owner cannot disable a route without its key")
	}
	options.ProwlDelivery = true
	handler = New(options)
	reply = request(handler, http.MethodGet, "/notifications", "owner", "", nil)
	expect(t, reply, http.StatusOK, "Queue test message", "Recent deliveries")
	expect(t, request(handler, http.MethodPost, route, "owner", "https://foreign.invalid", validTest), http.StatusForbidden)
	expect(t, request(handler, http.MethodPost, route, "other", origin, validTest), http.StatusForbidden)
	expect(t, request(handler, http.MethodGet, route, "owner", "", nil), http.StatusMethodNotAllowed)
	expect(t, request(handler, http.MethodPost, route, "owner", origin, validTest), http.StatusSeeOther)
	if backend.queued != 1 {
		t.Fatal("test notification was not queued exactly once")
	}

	configure := url.Values{
		"csrf": {csrf}, "id": {"route-1"}, "revision": {"2"}, "enabled": {"true"},
		"key_ref": {"private-key-filename"}, "timezone": {"Australia/Melbourne"},
		"quiet_start": {"22:00"}, "quiet_end": {"07:00"}, "digest": {"false"}, "categories": {"13"},
	}
	expect(t, request(handler, http.MethodPost, "/notifications", "owner", origin, configure), http.StatusSeeOther)
	if len(backend.saved) != 1 || backend.saved[0].Actor != "owner" || backend.saved[0].Route.QuietStart != 1320 {
		t.Fatal("configuration was not passed with current owner and parsed values")
	}
	configure.Set("revision", "1")
	expect(t, request(handler, http.MethodPost, "/notifications", "owner", origin, configure), http.StatusConflict)
	if strings.Contains(request(handler, http.MethodGet, "/notifications", "owner", "", nil).Body.String(), "private-key-filename") {
		t.Fatal("private key reference appeared in the browser")
	}
}
