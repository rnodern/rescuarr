package webconsole

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
)

type importDryRunFixture struct {
	after []string
	page  app.ImportDryRunPage
}

func (f *importDryRunFixture) ImportDryRun(_ context.Context, after string) (app.ImportDryRunPage, error) {
	f.after = append(f.after, after)
	return f.page, nil
}

func TestImportDryRunRequiresAnOwnerAndAnExplicitCheck(t *testing.T) {
	opts := fixture()
	item := opts.Read.(*apptest.Console).Page.Cases[0]
	service := &importDryRunFixture{page: app.ImportDryRunPage{
		Cases: []app.ImportDryRunCase{{Case: item, Assessment: "Needs review", Reason: "Audio unverified. Listen to the download in Manual Review."}},
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
