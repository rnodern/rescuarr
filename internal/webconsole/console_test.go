package webconsole

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
	"github.com/rnodern/rescuarr/internal/domain"
)

const (
	prefix = "/tools"
	origin = "https://console.example.test"
	csrf   = "synthetic-form-token"
)

func fixture(options ...func(*apptest.Console)) Options {
	now := time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)
	instance := domain.InstanceRef{ID: "sonarr-hd", Epoch: 2}
	job := domain.JobRef{Instance: instance, ID: "download-1", Generation: 1}
	target := domain.TargetRef{Instance: instance, Kind: domain.TargetEpisode, ID: 42, SeriesID: 21}
	first := domain.Case{
		ID: "case-1", Revision: 5, State: domain.CaseProposed,
		Job: domain.Job{Ref: job}, Targets: []domain.Target{{Ref: target}},
		Reason: "unusual_filename", Evidence: []domain.EvidenceID{"source-evidence"},
		Display: domain.CaseDisplay{Service: "Sonarr HD", Product: domain.ProductSonarr, Title: "Fixture Series",
			Release: "Fixture.Series.S02E03", Media: []domain.CaseMedia{{Target: target, Episode: "S02E03", Title: "Fixture Episode"}}},
	}
	other := domain.Case{
		ID: "case-2", Revision: 2, State: domain.CaseHeld, Reason: "capacity_unknown",
		Job: domain.Job{Ref: domain.JobRef{Instance: domain.InstanceRef{ID: "sonarr-uhd", Epoch: 6}}},
	}
	guard := domain.Guard{Passed: true, Reason: "exact_target_match", Evidence: []domain.EvidenceID{"verified-mapping"}}
	proposal := domain.Proposal{
		ID: "proposal-1", CaseID: first.ID, CaseRevision: first.Revision, PolicyRevision: 7, CapabilityRevision: 11,
		Operation: domain.Operation{
			Job: job, Action: domain.ActionImport, Targets: []domain.TargetRef{target},
			Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}},
		},
		Guards: []domain.Guard{guard}, ExpiresAt: now.Add(5 * time.Minute),
	}
	intent := domain.Intent{ID: "intent-1", Proposal: proposal, State: domain.IntentUncertain, CreatedAt: now}
	fake := &apptest.Console{
		Dashboard: app.Overview{Mode: domain.ModeAssist, Revision: 6, Cases: 2, HeldCases: 1, OpenIncidents: 1},
		Page:      app.CasePage{Cases: []domain.Case{first, other}, After: "next-fixture"},
		Details:   map[domain.CaseID]app.CaseDetail{first.ID: {Case: first, Proposals: []domain.Proposal{proposal}}},
		Intents:   map[domain.IntentID]domain.Intent{intent.ID: intent},
	}
	for _, apply := range options {
		apply(fake)
	}
	return Options{
		BasePath: prefix, Read: fake, Commands: fake,
		Authorize: func(r *http.Request) Session {
			switch r.Header.Get("X-Session-Fixture") {
			case "owner":
				return Session{Actor: "owner", CSRF: csrf, Origin: origin}
			case "other":
				return Session{Actor: "owner", CSRF: "another-session-token", Origin: origin}
			default:
				return Session{}
			}
		},
		Clock: app.FixedClock{At: now},
	}
}

func request(h http.Handler, method, path, session, from string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, origin+prefix+path, strings.NewReader(form.Encode()))
	r.Header.Set("X-Session-Fixture", session)
	if from != "" {
		r.Header.Set("Origin", from)
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func expect(t *testing.T, response *httptest.ResponseRecorder, code int, contents ...string) {
	t.Helper()
	if response.Code != code {
		t.Fatalf("status %d, want %d: %s", response.Code, code, response.Body.String())
	}
	for _, content := range contents {
		if !strings.Contains(response.Body.String(), content) {
			t.Fatalf("missing %q: %s", content, response.Body.String())
		}
	}
}

func TestConsoleViewsAndAssets(t *testing.T) {
	t.Parallel()
	h := New(fixture())
	expect(t, request(h, "GET", "/", "owner", "", nil), 200, "<h1>Overview</h1>", "Held cases", "Open incidents")
	expect(t, request(h, "GET", "/cases?state=held&q=fixture&limit=10", "owner", "", nil), 200,
		"Sonarr HD", "Fixture Series", "case-1", "case-2",
		"after=next-fixture&amp;limit=10&amp;q=fixture&amp;state=held")
	expect(t, request(h, "GET", "/cases/case-1", "owner", "", nil), 200,
		"<h1>Fixture Series · S02E03</h1>", "Fixture Episode", "Review import", "source-evidence")
	expect(t, request(h, "GET", "/actions/intent-1", "owner", "", nil), 200,
		"Checking whether Arr accepted the request", "Import into the library")
	expect(t, request(h, "GET", "/cases?limit=101", "owner", "", nil), 400, "Invalid page size")
	expect(t, request(h, "GET", "/cases/does-not-exist", "owner", "", nil), 404, "Record unavailable")
	expect(t, request(h, "GET", "/assets/console.css", "owner", "", nil), 200, "@media")
	expect(t, request(h, "GET", "/assets/console.js", "owner", "", nil), 200, "visibilitychange")
}

func TestProjectionTextIsEscaped(t *testing.T) {
	t.Parallel()
	h := New(fixture(func(data *apptest.Console) {
		detail := data.Details["case-1"]
		detail.Case.Reason = "<script>alert(1)</script>"
		data.Details["case-1"] = detail
	}))
	page := request(h, "GET", "/cases/case-1", "owner", "", nil)
	expect(t, page, 200, "&lt;script&gt;alert(1)&lt;/script&gt;")
	if strings.Contains(page.Body.String(), "<script>alert(") {
		t.Fatal("projection data executed as markup")
	}
}

func TestProtectedRoutesAndSessionScopedFragment(t *testing.T) {
	t.Parallel()
	h := New(fixture())
	for _, path := range []string{"/", "/cases", "/cases/case-1", "/cases/case-1/review/proposal-1", "/actions/intent-1", "/fragments/status"} {
		w := request(h, "GET", path, "", "", nil)
		expect(t, w, 401, "Sign in to continue")
		if strings.Contains(w.Body.String(), "sonarr-hd") {
			t.Fatalf("anonymous response leaked case data for %s", path)
		}
	}
	w := request(h, "GET", "/fragments/status", "owner", "", nil)
	expect(t, w, 200, "Assist")
	tag := w.Header().Get("ETag")
	if tag == "" {
		t.Fatal("missing ETag")
	}
	for session, want := range map[string]int{"owner": 304, "other": 200, "": 401} {
		r := httptest.NewRequest("GET", origin+prefix+"/fragments/status", nil)
		r.Header.Set("X-Session-Fixture", session)
		r.Header.Set("If-None-Match", tag)
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		expect(t, out, want)
	}
}

func TestNoJSApprovalConflictsAndRepeatSubmission(t *testing.T) {
	t.Parallel()
	var approved []app.ApproveProposal
	result := error(app.ErrConflict)
	h := New(fixture(func(data *apptest.Console) {
		data.OnApprove = func(_ context.Context, command app.ApproveProposal) (domain.Intent, error) {
			approved = append(approved, command)
			return data.Intents["intent-1"], result
		}
	}))
	review := request(h, "GET", "/cases/case-1/review/proposal-1", "owner", "", nil)
	expect(t, review, 200, "Review import", "Queue import", "Import into the library", "verified-mapping")
	form := extractForm(t, review)
	actions := "/cases/case-1/actions"
	expect(t, request(h, "POST", actions, "", origin, form), 401)
	expect(t, request(h, "POST", actions, "owner", "", form), 403, "Request origin rejected")
	expect(t, request(h, "POST", actions, "owner", origin+".invalid", form), 403, "Request origin rejected")
	expect(t, request(h, "POST", actions, "other", origin, form), 403, "Approval token rejected")
	form.Set("revision", "8")
	expect(t, request(h, "POST", actions, "owner", origin, form), 400, "Invalid approval form")
	form.Set("revision", "5")
	form.Set("operation", "forged upstream body")
	expect(t, request(h, "POST", actions, "owner", origin, form), 400, "Invalid approval form")
	form.Del("operation")
	if len(approved) != 0 {
		t.Fatal("unchecked request reached the command boundary")
	}
	expect(t, request(h, "POST", actions, "owner", origin, form), 409, "This proposal changed", "Return to case")
	if len(approved) != 1 || approved[0].Actor != "owner" || approved[0].CaseID != "case-1" ||
		approved[0].ProposalID != "proposal-1" || approved[0].Revision != 5 || approved[0].RequestKey == "" {
		t.Fatalf("unexpected typed command: %+v", approved)
	}
	result = nil
	for range 2 {
		submitted := request(h, "POST", actions, "owner", origin, form)
		expect(t, submitted, 303)
		if submitted.Header().Get("Location") != prefix+"/actions/intent-1" {
			t.Fatalf("unexpected redirect: %s", submitted.Header().Get("Location"))
		}
	}
	if len(approved) != 3 || approved[1] != approved[2] {
		t.Fatalf("repeat form did not preserve the exact idempotency request: %+v", approved)
	}
}

func TestHeldAndDestructiveProposalsRemainReadOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, hold string
		edit       func(*apptest.Console, *domain.Proposal)
	}{
		{"observe", "while the service is observing", func(c *apptest.Console, _ *domain.Proposal) { c.Dashboard.Mode = domain.ModeObserve }},
		{"paused", "while recovery is paused", func(c *apptest.Console, _ *domain.Proposal) { c.Dashboard.Paused = true }},
		{"stale revision", "the case has changed", func(_ *apptest.Console, p *domain.Proposal) { p.CaseRevision-- }},
		{"failed guard", "An eligibility check is holding", func(_ *apptest.Console, p *domain.Proposal) { p.Guards[0].Passed = false }},
		{"unqualified removal", "Review of client removal", func(_ *apptest.Console, p *domain.Proposal) {
			p.Operation.Effects = append(p.Operation.Effects, domain.Effect{Kind: domain.EffectDataRemoval})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(fixture(func(c *apptest.Console) {
				detail := c.Details["case-1"]
				tc.edit(c, &detail.Proposals[0])
				c.Details["case-1"] = detail
			}))
			w := request(h, "GET", "/cases/case-1/review/proposal-1", "owner", "", nil)
			expect(t, w, 200, tc.hold)
			if strings.Contains(w.Body.String(), "request_key") {
				t.Fatal("held proposal rendered a submission form")
			}
		})
	}
}

var hiddenField = regexp.MustCompile(`name="([^"]+)" value="([^"]*)"`)

func extractForm(t *testing.T, page *httptest.ResponseRecorder) url.Values {
	t.Helper()
	form := url.Values{}
	for _, pair := range hiddenField.FindAllStringSubmatch(page.Body.String(), -1) {
		form.Set(pair[1], pair[2])
	}
	if len(form) != 4 {
		t.Fatalf("expected four approval fields, got %d", len(form))
	}
	return form
}
