package webconsole

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/ledger"
)

type RecoveryPause interface {
	PauseState(context.Context) (bool, uint64, error)
	ComparePause(context.Context, bool, uint64, domain.ActorID, time.Time) error
}

type recoveryPauseView struct{ Revision uint64 }

func (c console) changeRecoveryPause(pause bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session := currentSession(r)
		if !sameOrigin(r, session.Origin) {
			c.errorPage(w, r, http.StatusForbidden, "overview", "Request origin rejected", "Open Overview and try again.")
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err != nil || media != "application/x-www-form-urlencoded" || r.ParseForm() != nil ||
			len(r.PostForm) != 2 || !oneValue(r.PostForm, "revision") {
			c.errorPage(w, r, http.StatusBadRequest, "overview", "Invalid recovery control", "Open Overview and try again.")
			return
		}
		if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
			c.errorPage(w, r, http.StatusForbidden, "overview", "Session token rejected", "Sign in and reopen Overview.")
			return
		}
		revision, err := strconv.ParseUint(r.PostForm.Get("revision"), 10, 63)
		if err != nil {
			c.errorPage(w, r, http.StatusBadRequest, "overview", "Invalid recovery revision", "Open Overview and try again.")
			return
		}
		err = c.RecoveryPause.ComparePause(r.Context(), pause, revision, session.Actor, c.Clock.Now())
		switch {
		case errors.Is(err, ledger.ErrConflict):
			c.errorPage(w, r, http.StatusConflict, "overview", "The recovery control changed", "Refresh Overview before changing the recovery state.")
		case err != nil:
			c.readError(w, r, err)
		default:
			http.Redirect(w, r, c.BasePath+"/", http.StatusSeeOther)
		}
	}
}
