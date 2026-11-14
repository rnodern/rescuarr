package webconsole

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type manualReviewPage struct {
	app.ManualReviewView
	Episode *domain.EpisodeNameReview
}

func (c console) manualReview(w http.ResponseWriter, r *http.Request) {
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	if status.Mode != domain.ModeAssist {
		http.Redirect(w, r, c.BasePath+"/", http.StatusSeeOther)
		return
	}
	result, err := c.ManualReview.ManualReview(r.Context())
	if err != nil {
		c.manualReviewError(w, r, err)
		return
	}
	view := manualReviewPage{ManualReviewView: result}
	if result.Case != nil {
		if episode := result.Preview.Episode; episode != nil {
			view.Episode = cleanEpisodeNames(*episode)
		} else if check := result.Case.Display.EpisodeCheck; check != nil {
			view.Episode = cleanEpisodeNames(check.EpisodeNameReview)
		}
		if status.Paused || status.ReconcileRequired {
			view.CanApprove = false
			if view.Hold == "" {
				view.Hold = "Recovery is paused or awaiting reconciliation."
			}
		}
	}
	view.Folder = manualBaseName(view.Folder)
	c.render(w, r, http.StatusOK, "manual-review", page{Title: "Manual Review", Active: "manual-review", Status: status, Body: view})
}

func cleanEpisodeNames(episode domain.EpisodeNameReview) *domain.EpisodeNameReview {
	episode.Folder = manualBaseName(episode.Folder)
	episode.Filename = manualBaseName(episode.Filename)
	return &episode
}

// Arr paths use the upstream machine's separators. Only show an item's name.
func manualBaseName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
	if name == "." || name == ".." || name == "/" || strings.HasSuffix(name, ":") {
		return ""
	}
	return name
}

func (c console) setManualMode(w http.ResponseWriter, r *http.Request) {
	if !c.manualReviewForm(w, r, "overview", "mode") {
		return
	}
	requested := r.PostForm.Get("mode")
	if requested != "manual" && requested != "observe" {
		c.errorPage(w, r, http.StatusBadRequest, "overview", "Invalid recovery mode", "Return to Overview to change the mode.")
		return
	}
	session := currentSession(r)
	if err := c.ManualReview.SetManualMode(r.Context(), requested == "manual", session.Actor); err != nil {
		c.manualReviewError(w, r, err)
		return
	}
	destination := "/"
	if requested == "manual" {
		destination = "/manual-review"
	}
	http.Redirect(w, r, c.BasePath+destination, http.StatusSeeOther)
}

func (c console) decideManualReview(w http.ResponseWriter, r *http.Request) {
	if !c.manualReviewForm(w, r, "manual-review", "decision", "token") {
		return
	}
	decision, token := r.PostForm.Get("decision"), r.PostForm.Get("token")
	if (decision != "approve" && decision != "decline") || token == "" || len(token) > 1024 {
		c.errorPage(w, r, http.StatusBadRequest, "manual-review", "Invalid review decision", "Open Manual Review and try again.")
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	if status.Mode != domain.ModeAssist || (decision == "approve" && (status.Paused || status.ReconcileRequired)) {
		c.manualReviewError(w, r, app.ErrHeld)
		return
	}
	session := currentSession(r)
	err := c.ManualReview.DecideManualReview(r.Context(), app.ManualDecision{
		Token: token, Approve: decision == "approve", Actor: session.Actor,
	})
	if err != nil {
		c.manualReviewError(w, r, err)
		return
	}
	http.Redirect(w, r, c.BasePath+"/manual-review", http.StatusSeeOther)
}

func (c console) manualReviewForm(w http.ResponseWriter, r *http.Request, active string, fields ...string) bool {
	session := currentSession(r)
	if !sameOrigin(r, session.Origin) {
		c.errorPage(w, r, http.StatusForbidden, active, "Request origin rejected", "Return to the console and try again.")
		return false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		c.errorPage(w, r, http.StatusBadRequest, active, "Invalid review form", "Return to the console and try again.")
		return false
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
		c.errorPage(w, r, http.StatusForbidden, active, "Session token rejected", "Sign in and reopen the console.")
		return false
	}
	valid := len(r.PostForm) == len(fields)+1
	for _, field := range fields {
		valid = valid && oneValue(r.PostForm, field)
	}
	if !valid {
		c.errorPage(w, r, http.StatusBadRequest, active, "Invalid review form", "Return to the console and try again.")
	}
	return valid
}

func (c console) manualReviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, app.ErrConflict):
		c.render(w, r, http.StatusConflict, "manual-review-error", page{
			Title: "The review changed", Body: "Open the current review to see the updated evidence.",
		})
	case errors.Is(err, app.ErrHeld):
		c.render(w, r, http.StatusConflict, "manual-review-error", page{
			Title: "This action is held", Body: "Check the current mode, pause state and review before trying again.",
		})
	default:
		c.readError(w, r, err)
	}
}
