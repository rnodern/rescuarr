package webconsole

import (
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type importPreviewView struct {
	CaseID string
	Result app.ImportPreview
}

func (v importPreviewView) Summary() string {
	switch v.Result.Reason {
	case "import_preview_complete":
		return "Arr found a candidate for every target."
	case "import_preview_partial":
		return "Arr found candidates for some of the targets."
	default:
		return "Arr did not provide an unambiguous candidate for this case."
	}
}

func (c console) importPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("caseID")
	if !localID(id) {
		c.readError(w, r, app.ErrNotFound)
		return
	}
	session := currentSession(r)
	if !sameOrigin(r, session.Origin) {
		c.errorPage(w, r, http.StatusForbidden, "cases", "Request origin rejected", "Open the case and try again.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil ||
		len(r.PostForm) != 2 || !oneValue(r.PostForm, "revision") {
		c.errorPage(w, r, http.StatusBadRequest, "cases", "Invalid preview request", "Open the case and try again.")
		return
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
		c.errorPage(w, r, http.StatusForbidden, "cases", "Session token rejected", "Sign in and reopen the case.")
		return
	}
	revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 63)
	if err != nil || revision == 0 {
		c.errorPage(w, r, http.StatusBadRequest, "cases", "Invalid case revision", "Open the case and try again.")
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	result, err := c.ImportPreview.ImportPreview(r.Context(), domain.CaseID(id), domain.Revision(revision))
	switch {
	case errors.Is(err, app.ErrConflict):
		c.render(w, r, http.StatusConflict, "approval-error", page{Title: "The case changed", Active: "cases", Body: id, Status: status})
	case errors.Is(err, app.ErrHeld):
		c.render(w, r, http.StatusConflict, "approval-error", page{Title: "Import preview is unavailable for this case", Active: "cases", Body: id, Status: status})
	case err != nil:
		c.readError(w, r, err)
	default:
		c.render(w, r, http.StatusOK, "import-preview", page{Title: "Import preview", Active: "cases", Status: status, Body: importPreviewView{id, result}})
	}
}
