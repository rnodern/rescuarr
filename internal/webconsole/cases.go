package webconsole

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

type casesView struct {
	Cases    []domain.Case
	Query    app.CaseQuery
	States   []domain.CaseState
	Next     string
	Services []observeservice.InstanceStatus
}

type caseView struct {
	app.CaseDetail
	PreviewHold string
	ReviewNames bool
}

var caseStates = []domain.CaseState{
	domain.CaseObserved, domain.CaseWaiting, domain.CaseAssessing, domain.CaseProposed,
	domain.CaseReady, domain.CaseExecuting, domain.CaseVerifying, domain.CaseWaitingReplacement,
	domain.CaseHeld, domain.CaseClosed,
}

func (c console) cases(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	query := app.CaseQuery{
		Instance: domain.InstanceID(params.Get("instance")),
		State:    domain.CaseState(params.Get("state")),
		Reason:   domain.ReasonCode(params.Get("cause")),
		Search:   strings.TrimSpace(params.Get("q")),
		After:    params.Get("after"),
		Limit:    50,
	}
	if raw := params.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			c.errorPage(w, r, http.StatusBadRequest, "cases", "Invalid page size", "Choose a page size from 1 to 100.")
			return
		}
		query.Limit = limit
	}
	if (query.Instance != "" && !localID(string(query.Instance))) ||
		(query.Reason != "" && !localID(string(query.Reason))) ||
		(query.State != "" && !slicesState(query.State)) ||
		len(query.Search) > 128 || len(query.After) > 128 {
		c.errorPage(w, r, http.StatusBadRequest, "cases", "Invalid case filters", "Remove the filters or start again from Cases.")
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	cases, err := c.Read.Cases(r.Context(), query)
	if err != nil {
		c.readError(w, r, err)
		return
	}
	content := casesView{Cases: cases.Cases, Query: query, States: caseStates}
	if c.Connections != nil {
		content.Services, err = c.Connections.Instances(r.Context())
		if err != nil {
			c.readError(w, r, err)
			return
		}
	}
	if cases.After != "" {
		next := url.Values{}
		for _, key := range []string{"instance", "state", "cause", "q", "limit"} {
			if v := params.Get(key); v != "" {
				next.Set(key, v)
			}
		}
		next.Set("after", cases.After)
		content.Next = c.BasePath + "/cases?" + next.Encode()
	}
	c.render(w, r, http.StatusOK, "cases", page{Title: "Cases", Active: "cases", Status: status, Body: content})
}

func slicesState(state domain.CaseState) bool {
	for _, member := range caseStates {
		if member == state {
			return true
		}
	}
	return false
}

func (c console) caseDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("caseID")
	if !localID(id) {
		c.readError(w, r, app.ErrNotFound)
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	detail, err := c.Read.Case(r.Context(), domain.CaseID(id))
	if err != nil {
		c.readError(w, r, err)
		return
	}
	display := detail.Case.Display
	body := caseView{CaseDetail: detail, PreviewHold: previewVersionHold(display)}
	if detail.Case.Reason == domain.ReasonCode(observe.FindingSeriesIDOnly) {
		body.ReviewNames = len(detail.Case.Targets) == 1 && importflow.SupportsEpisodeNameReview(display.Product, display.Version)
		if body.ReviewNames {
			body.PreviewHold = ""
		} else {
			body.PreviewHold = "Open Manual Import in Sonarr to review this download."
		}
	}
	c.render(w, r, http.StatusOK, "case", page{Title: caseTitle(detail.Case), Active: "cases", Status: status, Body: body})
}

func previewVersionHold(display domain.CaseDisplay) string {
	switch display.Product {
	case domain.ProductSonarr:
		if display.Version != "" && display.Version != importflow.SonarrVersion {
			return "This build can read the queue on Sonarr " + display.Version +
				". Import preview is currently supported on Sonarr " + importflow.SonarrVersion + "."
		}
	case domain.ProductRadarr:
		if display.Version != "" && display.Version != importflow.RadarrVersion {
			return "This build can read the queue on Radarr " + display.Version +
				". Import preview is currently supported on Radarr " + importflow.RadarrVersion + "."
		}
	}
	return ""
}

func (c console) intent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("intentID")
	if !localID(id) {
		c.readError(w, r, app.ErrNotFound)
		return
	}
	status, ok := c.loadStatus(w, r)
	if !ok {
		return
	}
	intent, err := c.Read.Intent(r.Context(), domain.IntentID(id))
	if err != nil {
		c.readError(w, r, err)
		return
	}
	c.render(w, r, http.StatusOK, "intent", page{Title: "Action " + id, Active: "cases", Status: status, Body: intent})
}
