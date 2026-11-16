package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

type episodeOrderFixture struct {
	required bool
	updates  int
}

func (f *episodeOrderFixture) EpisodeOrder(context.Context, domain.Case) (observeservice.EpisodeOrder, error) {
	return observeservice.EpisodeOrder{Available: true, RequireTitle: f.required}, nil
}

func (f *episodeOrderFixture) SetEpisodeTitleRequired(_ context.Context, id domain.CaseID, required bool, actor domain.ActorID) error {
	if id == "case-1" && actor == "owner" {
		f.required = required
		f.updates++
	}
	return nil
}

func TestEpisodeOrderRequiresOwnerPOST(t *testing.T) {
	const endpoint = "/cases/case-1/episode-order"
	options := fixture()
	service := &episodeOrderFixture{}
	options.EpisodeOrder = service
	handler := New(options)
	expect(t, request(handler, "GET", "/cases/case-1", "owner", "", nil), http.StatusOK, "Require matching episode title", prefix+endpoint)
	form := url.Values{"csrf": {csrf}, "require_title": {"true"}}
	expect(t, request(handler, "POST", endpoint, "", origin, form), http.StatusUnauthorized)
	expect(t, request(handler, "GET", endpoint, "owner", "", nil), http.StatusMethodNotAllowed)
	expect(t, request(handler, "POST", endpoint, "owner", "https://another-origin.example", form), http.StatusForbidden)
	expect(t, request(handler, "POST", endpoint, "other", origin, form), http.StatusForbidden)
	if service.updates != 0 {
		t.Fatal("an unauthorised or invalid request changed the requirement")
	}
	changed := request(handler, "POST", endpoint, "owner", origin, form)
	expect(t, changed, http.StatusSeeOther)
	if changed.Header().Get("Location") != prefix+"/cases/case-1" || !service.required || service.updates != 1 {
		t.Fatal("the setting was not saved for the selected case")
	}
	expect(t, request(handler, "GET", "/cases/case-1", "owner", "", nil), http.StatusOK,
		"The import dry run requires a matching episode title for this series.", "Use standard matching")
}
