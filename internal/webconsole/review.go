package webconsole

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type reviewView struct {
	Proposal               domain.Proposal
	RequestKey, CSRF, Hold string
}

func (c console) review(w http.ResponseWriter, r *http.Request) {
	id, proposalID := r.PathValue("caseID"), r.PathValue("proposalID")
	if !localID(id) || !localID(proposalID) {
		c.readError(w, app.ErrNotFound)
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	detail, err := c.Read.Case(r.Context(), domain.CaseID(id))
	if err != nil {
		c.readError(w, err)
		return
	}
	for _, proposal := range detail.Proposals {
		if proposal.ID != domain.ProposalID(proposalID) || proposal.CaseID != detail.Case.ID || string(proposal.CaseID) != id {
			continue
		}
		session := currentSession(r)
		content := reviewView{Proposal: proposal, CSRF: session.CSRF}
		content.Hold = c.approvalHold(status, detail.Case, proposal)
		if content.Hold == "" {
			nonce := rand.Text()
			content.RequestKey = nonce + "." + c.signedRequest(session, nonce, id, proposalID, strconv.FormatUint(uint64(proposal.CaseRevision), 10))
		}
		c.render(w, http.StatusOK, "review", page{Title: "Review " + actionLabel(proposal.Operation.Action), Active: "cases", Status: status, Body: content})
		return
	}
	c.readError(w, app.ErrNotFound)
}

func (c console) approvalHold(status app.Overview, current domain.Case, proposal domain.Proposal) string {
	switch {
	case c.Commands == nil:
		return "This console has read-only access to recovery actions."
	case status.Paused || status.ReconcileRequired:
		return "Actions are held while recovery is paused or reconciliation is required."
	case status.Mode != domain.ModeAssist && status.Mode != domain.ModeAutonomous:
		return "Actions are unavailable while the service is observing."
	case proposal.CaseRevision == 0 || proposal.CaseRevision != current.Revision || !c.Clock.Now().Before(proposal.ExpiresAt):
		return "This proposal has expired or the case has changed. Return to the case for a current decision."
	}
	switch proposal.Operation.Action {
	case domain.ActionImport, domain.ActionReject, domain.ActionSearch:
	default:
		return "This proposal has an unrecognised action."
	}
	if len(proposal.Guards) == 0 || len(proposal.Operation.Effects) == 0 {
		return "This proposal has insufficient effect or eligibility evidence."
	}
	for _, check := range proposal.Guards {
		if !check.Passed {
			return "An eligibility check is holding this proposal."
		}
	}
	for _, effect := range proposal.Operation.Effects {
		switch effect.Kind {
		case domain.EffectLibraryImport, domain.EffectSourceRetained, domain.EffectQueueRemoval,
			domain.EffectBlocklist, domain.EffectSearchDispatch, domain.EffectReplacementGrab:
		default:
			return "Review of client removal or unknown upstream effects is unavailable."
		}
	}
	return ""
}

func (c console) approve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("caseID")
	if !localID(id) {
		c.readError(w, app.ErrNotFound)
		return
	}
	session := currentSession(r)
	if !sameOrigin(r, session.Origin) {
		c.errorPage(w, http.StatusForbidden, "cases", "Request origin rejected", "Submit the approval from the console review page.")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		c.errorPage(w, http.StatusBadRequest, "cases", "Invalid approval form", "Open the proposal and submit the review form again.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		c.errorPage(w, http.StatusBadRequest, "cases", "Invalid approval form", "Open the proposal and submit the review form again.")
		return
	}
	if !oneValue(r.PostForm, "csrf") || !equalSecret(r.PostForm.Get("csrf"), session.CSRF) {
		c.errorPage(w, http.StatusForbidden, "cases", "Approval token rejected", "Sign in and reopen the review before approving.")
		return
	}
	proposal, revision, key := r.PostForm.Get("proposal_id"), r.PostForm.Get("revision"), r.PostForm.Get("request_key")
	value, err := strconv.ParseUint(revision, 10, 64)
	nonce, signature, signed := strings.Cut(key, ".")
	if len(r.PostForm) != 4 || !oneValue(r.PostForm, "proposal_id") || !oneValue(r.PostForm, "revision") ||
		!oneValue(r.PostForm, "request_key") || !localID(proposal) || err != nil || value == 0 ||
		!signed || !localID(nonce) || !equalSecret(signature, c.signedRequest(session, nonce, id, proposal, revision)) {
		c.errorPage(w, http.StatusBadRequest, "cases", "Invalid approval form", "Open the proposal and submit the review form again.")
		return
	}
	if c.Commands == nil {
		c.errorPage(w, http.StatusServiceUnavailable, "cases", "Actions are unavailable", "This console has read-only access to recovery actions.")
		return
	}
	intent, err := c.Commands.Approve(r.Context(), app.ApproveProposal{
		CaseID: domain.CaseID(id), ProposalID: domain.ProposalID(proposal),
		Revision: domain.Revision(value), RequestKey: key, Actor: session.Actor,
	})
	switch {
	case errors.Is(err, app.ErrConflict):
		c.render(w, http.StatusConflict, "approval-error", page{
			Title: "This proposal changed", Active: "cases", Body: id,
		})
	case errors.Is(err, app.ErrHeld):
		c.render(w, http.StatusConflict, "approval-error", page{
			Title: "The current policy is holding this action", Active: "cases", Body: id,
		})
	case err != nil:
		c.readError(w, err)
	case !localID(string(intent.ID)):
		c.readError(w, errors.New("invalid local intent identity"))
	default:
		http.Redirect(w, r, c.BasePath+"/actions/"+string(intent.ID), http.StatusSeeOther)
	}
}

func canonicalOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.Hostname() != "" && u.User == nil && u.Scheme+"://"+u.Host == value
}

func sameOrigin(r *http.Request, origin string) bool {
	if values := r.Header.Values("Origin"); len(values) != 0 {
		return len(values) == 1 && values[0] == origin
	}
	from, err := url.Parse(r.Referer())
	return err == nil && from.User == nil && from.Scheme+"://"+from.Host == origin
}

func (c console) signedRequest(session Session, fields ...string) string {
	mac := hmac.New(sha256.New, c.key[:])
	for _, field := range append([]string{string(session.Actor), session.CSRF}, fields...) {
		mac.Write([]byte(strconv.Itoa(len(field))))
		mac.Write([]byte{':'})
		mac.Write([]byte(field))
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func oneValue(form url.Values, field string) bool { return len(form[field]) == 1 }

func etagFields(status app.Overview) string {
	return strings.Join([]string{
		strconv.FormatUint(uint64(status.Revision), 10), string(status.Mode),
		strconv.FormatBool(status.Paused), strconv.FormatBool(status.ReconcileRequired),
	}, ":")
}
