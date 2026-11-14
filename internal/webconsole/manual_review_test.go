package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
	"github.com/rnodern/rescuarr/internal/domain"
)

type manualReviewFixture struct {
	dashboard *apptest.Console
	view      app.ManualReviewView
	reviews   int
	switches  int
	decisions []app.ManualDecision
	failure   error
}

func (f *manualReviewFixture) SetManualMode(_ context.Context, enabled bool, actor domain.ActorID) error {
	if actor != "owner" {
		return app.ErrHeld
	}
	f.switches++
	f.dashboard.Dashboard.Mode = domain.ModeObserve
	if enabled {
		f.dashboard.Dashboard.Mode = domain.ModeAssist
	}
	return nil
}

func (f *manualReviewFixture) ManualReview(context.Context) (app.ManualReviewView, error) {
	f.reviews++
	return f.view, nil
}

func (f *manualReviewFixture) DecideManualReview(_ context.Context, decision app.ManualDecision) error {
	f.decisions = append(f.decisions, decision)
	return f.failure
}

func TestManualReviewNavigationAndPackPlan(t *testing.T) {
	opts := fixture()
	dashboard := opts.Read.(*apptest.Console)
	dashboard.Dashboard.Mode = domain.ModeObserve
	item := dashboard.Page.Cases[0]
	item.Reason = "no_eligible_files"
	manual := &manualReviewFixture{
		dashboard: dashboard,
		view: app.ManualReviewView{
			Case: &item, Token: "decline-token", ApprovalToken: "approve-token", CanApprove: true,
			Folder:    `C:\private\fixture-folder`,
			Diagnosis: "Sonarr found no eligible files for import",
			Preview: app.ImportPreview{Replacement: &app.ReplacementPreview{
				SearchCount: 1, Episodes: []app.ReplacementEpisode{
					{Label: "S02E02 · Already have it", Status: app.ReplacementInLibrary},
					{Label: "S02E03 · Fixture Episode", Status: app.ReplacementSearch},
					{Label: "S02E04 · Pending", Status: app.ReplacementOtherDownload},
				},
			}},
			SearchLabels: []string{"S02E03 · Fixture Episode"},
		},
	}
	opts.ManualReview = manual
	h := New(opts)

	observe := request(h, "GET", "/", "owner", "", nil)
	expect(t, observe, 200, "Switch to Manual")
	if strings.Contains(observe.Body.String(), "/manual-review\">Manual Review") {
		t.Fatal("Manual Review appeared in Observe")
	}
	expect(t, request(h, "GET", "/manual-review", "owner", "", nil), 303)
	if manual.reviews != 0 {
		t.Fatal("Observe rendered the manual review")
	}

	w := request(h, "POST", "/manual-review/mode", "owner", origin, url.Values{"csrf": {csrf}, "mode": {"manual"}})
	expect(t, w, http.StatusSeeOther)
	if w.Header().Get("Location") != prefix+"/manual-review" || manual.switches != 1 {
		t.Fatal("Manual mode did not open the review")
	}
	for range 2 {
		review := request(h, "GET", "/manual-review", "owner", "", nil)
		expect(t, review, 200, "<h1>Manual Review</h1>", `href="/tools/manual-review" aria-current="page"`,
			"Fixture Series", "Fixture.Series.S02E03", "Sonarr found no eligible files for import",
			"fixture-folder", "Already in library", "Another download in queue", "Still missing",
			"Remove this download from the Sonarr queue", "Blocklist this release and keep the download client job",
			"Search only for S02E03 · Fixture Episode, if still missing", ">Approve<", ">Decline<",
			`name="token" value="approve-token"`, `name="token" value="decline-token"`)
		if strings.Contains(review.Body.String(), "case-2") || strings.Contains(review.Body.String(), `C:\private`) {
			t.Fatal("Review exposed a different case or the full upstream path")
		}
	}

	manual.view.Processing = true
	review := request(h, "GET", "/manual-review", "owner", "", nil)
	expect(t, review, 200, "checking the approved action", "Refresh review")
	if strings.Contains(review.Body.String(), ">Approve<") || strings.Contains(review.Body.String(), ">Decline<") {
		t.Fatal("Processing review exposed another decision")
	}
	observe = request(h, "POST", "/manual-review/mode", "owner", origin, url.Values{"csrf": {csrf}, "mode": {"observe"}})
	expect(t, observe, 303)
	if observe.Header().Get("Location") != prefix+"/" || manual.switches != 2 {
		t.Fatal("Returning to Observe did not open Overview")
	}
}

func TestManualReviewDecisionUsesOwnerSessionAndCurrentMode(t *testing.T) {
	opts := fixture()
	dashboard := opts.Read.(*apptest.Console)
	manual := &manualReviewFixture{dashboard: dashboard}
	opts.ManualReview = manual
	h := New(opts)
	form := url.Values{"csrf": {csrf}, "decision": {"approve"}, "token": {"approved-scope"}}

	expect(t, request(h, "POST", "/manual-review", "", origin, form), 401)
	expect(t, request(h, "POST", "/manual-review", "owner", "https://other.example.test", form), 403)
	expect(t, request(h, "POST", "/manual-review", "other", origin, form), 403)
	dashboard.Dashboard.Mode = domain.ModeObserve
	expect(t, request(h, "POST", "/manual-review", "owner", origin, form), 409, "This action is held")
	dashboard.Dashboard.Mode = domain.ModeAssist
	dashboard.Dashboard.Paused = true
	expect(t, request(h, "POST", "/manual-review", "owner", origin, form), 409, "This action is held")
	if len(manual.decisions) != 0 {
		t.Fatal("A held or unauthorised action reached the service")
	}

	form.Set("decision", "decline")
	form.Set("token", "selected-item")
	expect(t, request(h, "POST", "/manual-review", "owner", origin, form), 303)
	dashboard.Dashboard.Paused = false
	form.Set("decision", "approve")
	form.Set("token", "approved-scope")
	manual.failure = app.ErrConflict
	expect(t, request(h, "POST", "/manual-review", "owner", origin, form), 409, "The review changed", "Open Manual Review")
	if len(manual.decisions) != 2 ||
		manual.decisions[0] != (app.ManualDecision{Token: "selected-item", Actor: "owner"}) ||
		manual.decisions[1] != (app.ManualDecision{Token: "approved-scope", Actor: "owner", Approve: true}) {
		t.Fatalf("unexpected decisions: %+v", manual.decisions)
	}
}

func TestManualReviewEpisodeEvidenceEscapesAndHidesPaths(t *testing.T) {
	opts := fixture()
	dashboard := opts.Read.(*apptest.Console)
	item := dashboard.Page.Cases[0]
	service := &manualReviewFixture{
		dashboard: dashboard,
		view: app.ManualReviewView{
			Case: &item, Token: "selected-item", Hold: "Import approval is unavailable.",
			Preview: app.ImportPreview{Episode: &domain.EpisodeNameReview{
				Matched: true, ArrRejection: true, EpisodeTitle: "Fixture Episode",
				Folder: "/home/secret/Candidates", Filename: `\\host\share\<b>fixture.mkv`,
			}, Audio: []app.AudioLanguageReview{{AudioAssessment: domain.AudioAssessment{Decision: domain.AudioAccept}}}},
		},
	}
	opts.ManualReview = service
	page := request(New(opts), "GET", "/manual-review", "owner", "", nil)
	expect(t, page, 200, "The names omit the episode title", "Sonarr still reports an import rejection",
		"Candidates", "&lt;b&gt;fixture.mkv", "Import the matching file into the library", "Import approval is unavailable", ">Decline<")
	if strings.Contains(page.Body.String(), "/home/secret") || strings.Contains(page.Body.String(), "<b>fixture") || strings.Contains(page.Body.String(), ">Approve<") {
		t.Fatal("Review exposed private data, executable markup or an unavailable decision")
	}
	service.view.Preview.Audio = nil
	page = request(New(opts), "GET", "/manual-review", "owner", "", nil)
	expect(t, page, 200, "Audio tracks could not be verified.")
	if strings.Contains(page.Body.String(), "Import the matching file into the library") {
		t.Fatal("A cached filename match bypassed the audio decision")
	}
}
