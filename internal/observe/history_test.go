package observe

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
)

// History keeps every event type Sonarr and Radarr send and reports anything
// else as unknown, rather than inventing an event Arr never emits.
func TestHistoryReadsArrEventTypes(t *testing.T) {
	events := []string{"grabbed", "downloadFolderImported", "downloadFailed", "downloadIgnored",
		"seriesFolderImported", "movieFolderImported", "episodeFileDeleted", "movieFileDeleted",
		"episodeFileRenamed", "movieFileRenamed", "downloadImported", "futureEvent"}
	var rows []string
	for i, event := range events {
		rows = append(rows, fmt.Sprintf(`{"id":%d,"seriesId":12,"episodeId":41,"downloadId":"SABnzbd_nzo_x","eventType":%q,"date":"2026-11-20T10:00:00Z"}`, 100-i, event))
	}
	a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/namespace/api/v3/history" || r.URL.Query().Get("downloadId") != "SABnzbd_nzo_x" {
			t.Fatalf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		return response(http.StatusOK, fmt.Sprintf(`{"page":1,"pageSize":50,"totalRecords":%d,"records":[%s]}`,
			len(rows), strings.Join(rows, ","))), nil
	})
	got, err := a.History(t.Context(), "SABnzbd_nzo_x", 3, observedAt)
	if err != nil || len(got.Records) != len(events) {
		t.Fatalf("history %+v: %v", got, err)
	}
	for i, record := range got.Records {
		want := events[i]
		if want == "downloadImported" || want == "futureEvent" {
			want = "unknown"
		}
		if record.Event != want {
			t.Errorf("event %q read as %q", events[i], record.Event)
		}
	}
}
