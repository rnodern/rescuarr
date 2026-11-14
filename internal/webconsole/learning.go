package webconsole

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/learning"
	"github.com/rnodern/rescuarr/internal/learningstore"
	"github.com/rnodern/rescuarr/internal/store"
)

type LearningService interface {
	Catalogue(context.Context, domain.PatternID, int) ([]learningstore.CatalogueEntry, error)
	Apply(context.Context, domain.PatternID, learning.Command) (learning.Controls, error)
}

type learningPage struct {
	Patterns []learningstore.CatalogueEntry
	Next     domain.PatternID
}

func (c console) learn(w http.ResponseWriter, r *http.Request) {
	const limit = 50
	after := r.URL.Query().Get("after")
	if after != "" && !patternID(after) {
		c.errorPage(w, r, http.StatusBadRequest, "learning", "Invalid pattern page", "Return to Learning.")
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	items, err := c.Learning.Catalogue(r.Context(), domain.PatternID(after), limit+1)
	if err != nil {
		c.readError(w, r, err)
		return
	}
	body := learningPage{Patterns: items}
	if len(items) > limit {
		body.Patterns = items[:limit]
		body.Next = items[limit-1].ID
	}
	c.render(w, r, http.StatusOK, "learning", page{Title: "Learning", Active: "learning", Status: status, Body: body})
}

func (c console) changePattern(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !patternID(id) {
		c.errorPage(w, r, http.StatusNotFound, "learning", "Pattern unavailable", "Return to Learning.")
		return
	}
	session := currentSession(r)
	if !sameOrigin(r, session.Origin) {
		c.errorPage(w, r, http.StatusForbidden, "learning", "Request origin rejected", "Open Learning and try again.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil || len(r.PostForm) != 3 ||
		!oneValue(r.PostForm, "revision") || !oneValue(r.PostForm, "change") {
		c.errorPage(w, r, http.StatusBadRequest, "learning", "Invalid learning form", "Open Learning and try again.")
		return
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
		c.errorPage(w, r, http.StatusForbidden, "learning", "Session token rejected", "Sign in and reopen Learning.")
		return
	}
	revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 63)
	change := learning.Change(r.PostForm.Get("change"))
	if err != nil || (change != learning.Pause && change != learning.Resume && change != learning.Reset) {
		c.errorPage(w, r, http.StatusBadRequest, "learning", "Invalid learning settings", "Open Learning and try again.")
		return
	}
	_, err = c.Learning.Apply(r.Context(), domain.PatternID(id), learning.Command{
		ExpectedRevision: domain.Revision(revision), Change: change, Actor: session.Actor,
		Reason: "operator_request", Event: domain.EventID(store.NewID()), At: c.Clock.Now(),
	})
	switch {
	case errors.Is(err, learning.ErrConflict):
		c.errorPage(w, r, http.StatusConflict, "learning", "Pattern changed", "Refresh Learning before changing this pattern.")
	case errors.Is(err, learningstore.ErrNotFound):
		c.errorPage(w, r, http.StatusNotFound, "learning", "Pattern unavailable", "Return to Learning.")
	case err != nil:
		c.readError(w, r, err)
	default:
		http.Redirect(w, r, c.BasePath+"/learning", http.StatusSeeOther)
	}
}

func patternID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, ch := range id {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}
