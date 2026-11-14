package webconsole

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
)

type activityReader func(context.Context, int, int64) ([]app.RecoveryActivityEntry, error)

func (f activityReader) RecoveryActivity(ctx context.Context, limit int, before int64) ([]app.RecoveryActivityEntry, error) {
	return f(ctx, limit, before)
}

func TestActivityShowsRecordedResultsAndPagesOlderEvents(t *testing.T) {
	t.Parallel()
	options := fixture()
	const title = "Fixture & Series <script> S02E03"
	now := options.Clock.Now()
	latest := make([]app.RecoveryActivityEntry, activityPageSize+1)
	for index := range latest {
		latest[index] = app.RecoveryActivityEntry{
			Sequence: int64(100 - index), CaseID: "case-1", CaseTitle: title, Kind: "recovery_queued",
			OccurredAt: now.Add(-time.Duration(index) * time.Minute),
		}
	}
	latest[0].Kind, latest[0].Reason = "action_readback_recorded", domain.ReasonCode(domain.VerificationEffect)
	latest[1].Kind, latest[1].Reason = "recovery_waiting_for_qualification", "sonarr_configuration_unverified"
	latest[2].Kind, latest[2].Reason = "action_acknowledged", ""
	latest[activityPageSize].CaseTitle = "Older persistent entry"
	calls := 0
	options.RecoveryActivity = activityReader(func(_ context.Context, limit int, before int64) ([]app.RecoveryActivityEntry, error) {
		calls++
		if limit != activityPageSize+1 {
			t.Fatalf("activity query limit = %d", limit)
		}
		switch before {
		case 0:
			return latest, nil
		case 51:
			return latest[activityPageSize:], nil
		default:
			t.Fatalf("unexpected activity cursor %d", before)
			return nil, nil
		}
	})
	handler := New(options)
	expect(t, request(handler, "GET", "/", "owner", "", nil), 200, `href="/tools/activity"`)
	expect(t, request(handler, "GET", "/activity", "", "", nil), http.StatusUnauthorized, "Sign in to continue")
	expect(t, request(handler, "GET", "/activity?before=-1", "owner", "", nil), http.StatusBadRequest, "Invalid activity page")
	if calls != 0 {
		t.Fatalf("invalid and anonymous requests called activity reader %d times", calls)
	}
	page := request(handler, "GET", "/activity", "owner", "", nil)
	expect(t, page, http.StatusOK, "<h1>Activity</h1>", `/tools/activity" aria-current="page"`,
		`href="/tools/cases/case-1"`, "Fixture &amp; Series &lt;script&gt; S02E03",
		">Verified</span>", "Recovery verified", ">Held</span>", "Automatic import is unavailable for this setup",
		"sonarr configuration unverified", ">Pending</span>", "Arr accepted the request. Checking the result.",
		`datetime="2026-11-14T11:00:00Z"`, `href="/tools/activity?before=51"`)
	body := page.Body.String()
	if strings.Contains(body, "Older persistent entry") || strings.Contains(body, "Fixture & Series <script>") {
		t.Fatalf("unexpected activity rows or raw markup: %s", body)
	}
	if strings.Index(body, "Recovery verified") > strings.Index(body, "Automatic import is unavailable") {
		t.Fatal("verified event should precede the older held event")
	}
	expect(t, request(handler, "GET", "/activity?before=51", "owner", "", nil), http.StatusOK,
		"Older persistent entry", "Latest activity")
}

func TestActivityHandlesEmptyAndUnavailableHistory(t *testing.T) {
	t.Parallel()
	options := fixture()
	options.RecoveryActivity = activityReader(func(context.Context, int, int64) ([]app.RecoveryActivityEntry, error) {
		return nil, nil
	})
	handler := New(options)
	expect(t, request(handler, "GET", "/activity", "owner", "", nil), http.StatusOK, "Recovery activity will appear here")
	expect(t, request(handler, "GET", "/activity?before=2", "owner", "", nil), http.StatusOK, "No earlier recovery activity")
	options.RecoveryActivity = activityReader(func(context.Context, int, int64) ([]app.RecoveryActivityEntry, error) {
		return nil, errors.New("private sqlite error")
	})
	page := request(New(options), "GET", "/activity", "owner", "", nil)
	expect(t, page, http.StatusServiceUnavailable, "Local data is unavailable")
	if strings.Contains(page.Body.String(), "private sqlite error") {
		t.Fatal("activity page exposed implementation error")
	}
}
