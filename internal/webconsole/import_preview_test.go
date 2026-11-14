package webconsole

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/app/apptest"
	"github.com/rnodern/rescuarr/internal/domain"
)

type previewFixture func(context.Context, domain.CaseID, domain.Revision) (app.ImportPreview, error)

func (p previewFixture) ImportPreview(ctx context.Context, id domain.CaseID, revision domain.Revision) (app.ImportPreview, error) {
	return p(ctx, id, revision)
}

func TestOwnerImportPreview(t *testing.T) {
	opts := fixture(func(c *apptest.Console) {
		detail := c.Details["case-1"]
		detail.Case.Job.CompleteSet = true
		c.Details["case-1"] = detail
	})
	calls := 0
	opts.ImportPreview = previewFixture(func(_ context.Context, id domain.CaseID, revision domain.Revision) (app.ImportPreview, error) {
		calls++
		if id != "case-1" || revision != 5 {
			t.Fatal("request was not scoped to the reviewed local case")
		}
		return app.ImportPreview{Reason: "import_preview_complete", Candidates: 1, Eligible: 1}, nil
	})
	h := New(opts)
	form := url.Values{"csrf": {csrf}, "revision": {"5"}}
	path := "/cases/case-1/import-preview"
	expect(t, request(h, http.MethodGet, "/cases/case-1", "owner", "", nil), 200, "Check Arr import candidates")
	expect(t, request(h, http.MethodPost, path, "", origin, form), 401)
	expect(t, request(h, http.MethodPost, path, "owner", "https://another.invalid", form), 403)
	form.Set("path", "/injected")
	expect(t, request(h, http.MethodPost, path, "owner", origin, form), 400)
	form.Del("path")
	expect(t, request(h, http.MethodPost, path, "owner", origin, form), 200,
		"Arr found a candidate for every target.", "Candidates returned by Arr", "Review any import in the Arr application.")
	if calls != 1 {
		t.Fatalf("unexpected number of preview calls: %d", calls)
	}
}

func TestOwnerEpisodeFilenameReviewOnSonarrV5(t *testing.T) {
	opts := fixture(func(c *apptest.Console) {
		detail := c.Details["case-1"]
		detail.Case.Reason = "series_matched_by_id"
		detail.Case.Display.Product = domain.ProductSonarr
		detail.Case.Display.Version = "5.0.0.2941"
		detail.Case.Targets = []domain.Target{{Ref: domain.TargetRef{Kind: domain.TargetEpisode, ID: 41, SeriesID: 12}}}
		detail.Case.Job.CompleteSet = true
		c.Details["case-1"] = detail
	})
	var held bool
	opts.ImportPreview = previewFixture(func(_ context.Context, _ domain.CaseID, _ domain.Revision) (app.ImportPreview, error) {
		if held {
			return app.ImportPreview{Episode: &domain.EpisodeNameReview{Reason: "Sonarr returned too many files."}}, app.ErrHeld
		}
		return app.ImportPreview{Episode: &domain.EpisodeNameReview{
			Series: "Captain Wattle: The Animated Series", Episode: "S02E11", EpisodeTitle: "Signal Lost",
			Filename: "Captain.Wattle.Animated.S02E11.mkv", Matched: true, ArrRejection: true,
		}}, nil
	})
	h := New(opts)
	expect(t, request(h, http.MethodGet, "/cases/case-1", "owner", "", nil), 200, "Check episode filename")
	form := url.Values{"csrf": {csrf}, "revision": {"5"}}
	expect(t, request(h, http.MethodPost, "/cases/case-1/import-preview", "owner", origin, form), 200,
		"Likely episode match", "S02E11 · Signal Lost", "Captain.Wattle.Animated.S02E11.mkv", "Sonarr still reports an import rejection", "Activity → Queue → Manual Import")
	held = true
	expect(t, request(h, http.MethodPost, "/cases/case-1/import-preview", "owner", origin, form), 409,
		"Episode filename check", "Match needs review", "Sonarr returned too many files.", "Activity → Queue → Manual Import")
}

func TestSonarrPackReplacementReview(t *testing.T) {
	opts := fixture(func(c *apptest.Console) {
		detail := c.Details["case-1"]
		detail.Proposals = nil
		detail.Case.Reason = "no_eligible_files"
		detail.Case.Display.Product = domain.ProductSonarr
		detail.Case.Display.Version = "5.0.0.2941"
		detail.Case.State = domain.CaseHeld
		detail.Case.Job.CompleteSet = true
		c.Details["case-1"] = detail
	})
	result := &app.ReplacementPreview{SearchCount: 1, Episodes: []app.ReplacementEpisode{
		{Label: "S01E01 · Fixture episode", Status: app.ReplacementSearch},
		{Label: "S01E02", Status: app.ReplacementInLibrary},
		{Label: "S01E03", Status: app.ReplacementOtherDownload},
	}}
	opts.ImportPreview = previewFixture(func(context.Context, domain.CaseID, domain.Revision) (app.ImportPreview, error) {
		return app.ImportPreview{Replacement: result}, nil
	})
	h := New(opts)
	expect(t, request(h, http.MethodGet, "/cases/case-1", "owner", "", nil), 200,
		"Arr found no files eligible for import", "Check which episodes still need a search")
	form := url.Values{"csrf": {csrf}, "revision": {"5"}}
	path := "/cases/case-1/import-preview"
	expect(t, request(h, http.MethodPost, path, "owner", origin, form), 200, "Replacement preview",
		"1 episode still needs a search", "blocklist the release", "Already in library", "Another download in queue", "Preview only.")
	result.SearchCount = 0
	result.Episodes = result.Episodes[1:]
	expect(t, request(h, http.MethodPost, path, "owner", origin, form), 200, "No episodes need a new search")
	result.UnclearCount = 1
	result.Episodes[0].Status = app.ReplacementUnclear
	expect(t, request(h, http.MethodPost, path, "owner", origin, form), 200,
		"Confirm what they cover before starting a replacement search", "Needs review")
}
