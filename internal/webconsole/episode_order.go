package webconsole

import (
	"context"
	"net/http"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

type EpisodeOrderService interface {
	EpisodeOrder(context.Context, domain.Case) (observeservice.EpisodeOrder, error)
	SetEpisodeTitleRequired(context.Context, domain.CaseID, bool, domain.ActorID) error
}

func (c console) saveEpisodeOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("caseID")
	if !localID(id) {
		c.readError(w, r, app.ErrNotFound)
		return
	}
	if !c.manualReviewForm(w, r, "cases", "require_title") {
		return
	}
	required := r.PostForm.Get("require_title")
	if !formBool(required) {
		c.errorPage(w, r, http.StatusBadRequest, "cases", "Invalid episode order setting", "Reopen the download and try again.")
		return
	}
	if err := c.EpisodeOrder.SetEpisodeTitleRequired(r.Context(), domain.CaseID(id), required == "true", currentSession(r).Actor); err != nil {
		c.readError(w, r, err)
		return
	}
	http.Redirect(w, r, c.BasePath+"/cases/"+id, http.StatusSeeOther)
}
