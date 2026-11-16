package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
	"github.com/rnodern/rescuarr/internal/domain"
)

type importDryRunFixture struct {
	after []string
	page  app.ImportDryRunPage
}

type automaticImportControlFixture struct {
	configured bool
	enabled    bool
}

func (f *automaticImportControlFixture) AutomaticImportsConfigured() bool { return f.configured }

func (f *automaticImportControlFixture) SetAutomaticImportMode(_ context.Context, enabled bool, _ domain.ActorID) error {
	f.enabled = enabled
	return nil
}

func TestAutomaticImportModeRequiresOwnerPOST(t *testing.T) {
	opts := fixture()
	opts.ManualReview = &manualReviewFixture{}
	control := &automaticImportControlFixture{configured: true}
	opts.AutomaticImport = control
	handler := New(opts)
	const action = "/automatic-import/mode"
	form := url.Values{"csrf": {csrf}, "mode": {"automatic"}}
	expect(t, request(handler, "GET", "/", "owner", "", nil), http.StatusOK, "Enable automatic Sonarr imports")
	expect(t, request(handler, "POST", action, "", origin, form), http.StatusUnauthorized)
	expect(t, request(handler, "POST", action, "other", origin, form), http.StatusForbidden)
	if control.enabled {
		t.Fatal("an unauthorised request activated automatic imports")
	}
	expect(t, request(handler, "POST", action, "owner", origin, form), http.StatusSeeOther)
	if !control.enabled {
		t.Fatal("the owner could not activate automatic imports")
	}
}

func (f *importDryRunFixture) ImportDryRun(_ context.Context, after string) (app.ImportDryRunPage, error) {
	f.after = append(f.after, after)
	return f.page, nil
}

func TestImportDryRunRequiresAnOwnerAndAnExplicitCheck(t *testing.T) {
	opts := fixture()
	item := opts.Read.(*apptest.Console).Page.Cases[0]
	service := &importDryRunFixture{page: app.ImportDryRunPage{
		Cases: []app.ImportDryRunCase{{Case: item, Assessment: "Needs review", Reason: "Audio unverified. Listen to the file before approving it in Manual Review."}},
		Next:  "next-case",
	}}
	opts.ImportDryRun = service
	handler := New(opts)
	expect(t, request(handler, "GET", "/", "owner", "", nil), http.StatusOK, "Import dry run")
	expect(t, request(handler, "GET", "/automatic-import?run=1", "", "", nil), http.StatusUnauthorized)
	expect(t, request(handler, "POST", "/automatic-import?run=1", "owner", "", nil), http.StatusMethodNotAllowed)
	expect(t, request(handler, "GET", "/automatic-import", "owner", "", nil), http.StatusOK, "Run dry run")
	if len(service.after) != 0 {
		t.Fatal("dry run started before the owner requested a check")
	}
	result := request(handler, "GET", "/automatic-import?run=1&after=prior-case", "owner", "", nil)
	expect(t, result, http.StatusOK, "Fixture Series", "S02E03", "Needs review", "Audio unverified",
		"Check next page", "First page", prefix+"/automatic-import?run=1&amp;after=next-case")
	if len(service.after) != 1 || service.after[0] != "prior-case" {
		t.Fatalf("unexpected dry run cursor %v", service.after)
	}
	if strings.Contains(result.Body.String(), `name="decision"`) {
		t.Fatal("dry run offered an action form")
	}
}
