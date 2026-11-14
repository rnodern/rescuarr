package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
	"github.com/rnodern/rescuarr/internal/learningstore"
)

type learningFixture struct {
	entry   learningstore.CatalogueEntry
	command learning.Command
}

func (f *learningFixture) Catalogue(context.Context, domain.PatternID, int) ([]learningstore.CatalogueEntry, error) {
	return []learningstore.CatalogueEntry{f.entry}, nil
}

func (f *learningFixture) Apply(_ context.Context, id domain.PatternID, command learning.Command) (learning.Controls, error) {
	if id != f.entry.ID {
		return learning.Controls{}, learningstore.ErrNotFound
	}
	if command.ExpectedRevision != f.entry.Revision {
		return learning.Controls{}, learning.ErrConflict
	}
	f.command = command
	f.entry.Revision++
	f.entry.Paused = command.Change == learning.Pause
	return learning.Controls{Revision: f.entry.Revision, Paused: f.entry.Paused}, nil
}

func TestLearningControlsRequireCurrentOwner(t *testing.T) {
	pattern := domain.PatternID(strings.Repeat("a", 64))
	data := &learningFixture{entry: learningstore.CatalogueEntry{
		ID: pattern, Product: domain.ProductSonarr, Family: learning.OriginalImport, Reason: "import_ambiguous",
	}}
	options := fixture()
	options.Learning = data
	handler := New(options)
	path := "/learning/" + string(pattern)
	form := url.Values{"csrf": {csrf}, "revision": {"0"}, "change": {"pause"}}
	expect(t, request(handler, http.MethodGet, "/learning", "", "", nil), http.StatusUnauthorized)
	expect(t, request(handler, http.MethodGet, "/learning", "owner", "", nil), http.StatusOK, "Sonarr", "Pause", "</html>")
	expect(t, request(handler, http.MethodPost, path, "owner", "https://foreign.invalid", form), http.StatusForbidden)
	expect(t, request(handler, http.MethodPost, path, "other", origin, form), http.StatusForbidden)
	if data.command.Actor != "" {
		t.Fatal("untrusted request reached learning")
	}
	expect(t, request(handler, http.MethodPost, path, "owner", origin, form), http.StatusSeeOther)
	if data.command.Actor != "owner" || data.command.Change != learning.Pause || data.command.Event == "" {
		t.Fatal("learning did not receive owner and audit data")
	}
	expect(t, request(handler, http.MethodPost, path, "owner", origin, form), http.StatusConflict)
	expect(t, request(handler, http.MethodGet, "/learning", "owner", "", nil), http.StatusOK, "Resume", "</html>")
}
