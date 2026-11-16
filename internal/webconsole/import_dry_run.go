package webconsole

import (
	"errors"
	"net/http"

	"github.com/rnodern/rescuarr/internal/app"
)

func (c console) setAutomaticImportMode(w http.ResponseWriter, r *http.Request) {
	if !c.manualReviewForm(w, r, "overview", "mode") {
		return
	}
	mode := r.PostForm.Get("mode")
	if mode != "automatic" && mode != "manual" {
		c.errorPage(w, r, http.StatusBadRequest, "overview", "Invalid recovery mode", "Return to Overview to change the mode.")
		return
	}
	err := c.AutomaticImport.SetAutomaticImportMode(r.Context(), mode == "automatic", currentSession(r).Actor)
	if errors.Is(err, app.ErrHeld) {
		c.errorPage(w, r, http.StatusConflict, "overview", "Automatic imports are held",
			"Check the Sonarr profile, download audio setup, pause state and any recovery in progress.")
		return
	}
	if err != nil {
		c.readError(w, r, err)
		return
	}
	http.Redirect(w, r, c.BasePath+"/", http.StatusSeeOther)
}

type importDryRunView struct {
	app.ImportDryRunPage
	Ran   bool
	After string
}

func (c console) importDryRun(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	view := importDryRunView{Ran: query.Get("run") == "1", After: query.Get("after")}
	if view.After != "" && !localID(view.After) {
		c.errorPage(w, r, http.StatusBadRequest, "import-dry-run", "Invalid dry run page", "Start again from Automatic import dry run.")
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	if c.RecoveryPause != nil {
		paused, _, err := c.RecoveryPause.PauseState(r.Context())
		if err != nil {
			c.readError(w, r, err)
			return
		}
		status.Paused = paused
	}
	if view.Ran {
		result, err := c.ImportDryRun.ImportDryRun(r.Context(), view.After)
		if errors.Is(err, app.ErrConflict) {
			c.errorPage(w, r, http.StatusConflict, "import-dry-run", "A dry run is already checking the queue", "Wait for it to finish, then try again.")
			return
		}
		if err != nil {
			c.readError(w, r, err)
			return
		}
		view.ImportDryRunPage = result
	}
	c.render(w, r, http.StatusOK, "import-dry-run", page{
		Title: "Automatic import dry run", Active: "import-dry-run", Status: status, Body: view,
	})
}
