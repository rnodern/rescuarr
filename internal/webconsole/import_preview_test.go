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

func TestOwnerEpisodeFilenameReviewOnSonarr3012(t *testing.T) {
	opts := fixture(func(c *apptest.Console) {
		detail := c.Details["case-1"]
		detail.Case.Reason = "series_matched_by_id"
		detail.Case.Display.Product = domain.ProductSonarr
		detail.Case.Display.Version = "4.0.20.3012"
		detail.Case.Targets = []domain.Target{{Ref: domain.TargetRef{Kind: domain.TargetEpisode, ID: 41, SeriesID: 12}}}
		detail.Case.Job.CompleteSet = true
		c.Details["case-1"] = detail
	})
	opts.ImportPreview = previewFixture(func(_ context.Context, _ domain.CaseID, _ domain.Revision) (app.ImportPreview, error) {
		return app.ImportPreview{Episode: &app.EpisodeNameReview{
			Series: "Captain Wattle: The Animated Series", Episode: "S02E11", EpisodeTitle: "Signal Lost",
			Filename: "Captain.Wattle.Animated.S02E11.mkv", Matched: true, ArrRejection: true,
		}}, nil
	})
	h := New(opts)
	expect(t, request(h, http.MethodGet, "/cases/case-1", "owner", "", nil), 200, "Check episode filename")
	form := url.Values{"csrf": {csrf}, "revision": {"5"}}
	expect(t, request(h, http.MethodPost, "/cases/case-1/import-preview", "owner", origin, form), 200,
		"Likely episode match", "S02E11 · Signal Lost", "Captain.Wattle.Animated.S02E11.mkv", "Sonarr still reports an import rejection", "Activity → Queue → Manual Import")
}
