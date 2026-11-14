package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	t.Parallel()
	unavailable := func(context.Context) error { return errors.New("private store error") }
	available := func(context.Context) error { return nil }
	tests := []struct {
		path   string
		check  Readiness
		status int
		body   string
	}{
		{"/healthz", unavailable, http.StatusOK, `{"status":"ok"}`},
		{"/readyz", unavailable, http.StatusServiceUnavailable, `{"status":"unready"}`},
		{"/readyz", available, http.StatusOK, `{"status":"ready"}`},
		{"/rescuer/", available, http.StatusOK, "<h1>Rescuarr</h1>"},
		{"/", available, http.StatusNotFound, "404 page not found"},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		Handler("/rescuer", test.check).ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.body) {
			t.Errorf("%s returned %d %q", test.path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "private") {
			t.Errorf("%s leaked its backing error", test.path)
		}
	}
}
