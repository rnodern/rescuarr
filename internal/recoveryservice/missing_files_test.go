package recoveryservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestCompletedSonarrPackReplacementPreview(t *testing.T) {
	const release = "Fixture.Show.S01.720p"
	for _, test := range []struct {
		name            string
		expected        []app.ReplacementStatus
		unknownJob      bool
		changed         bool
		partial         bool
		everyoneHasFile bool
	}{
		{name: "mixed", expected: []app.ReplacementStatus{
			app.ReplacementSearch, app.ReplacementInLibrary, app.ReplacementOtherDownload,
			app.ReplacementUnmonitored, app.ReplacementUnaired, app.ReplacementSearch,
			app.ReplacementSearch, app.ReplacementSearch, app.ReplacementSearch,
		}},
		{name: "all already imported", everyoneHasFile: true, expected: []app.ReplacementStatus{
			app.ReplacementInLibrary, app.ReplacementInLibrary, app.ReplacementInLibrary,
			app.ReplacementInLibrary, app.ReplacementInLibrary, app.ReplacementInLibrary,
			app.ReplacementInLibrary, app.ReplacementInLibrary, app.ReplacementInLibrary,
		}},
		{name: "unmapped concurrent download", unknownJob: true, expected: []app.ReplacementStatus{
			app.ReplacementUnclear, app.ReplacementInLibrary, app.ReplacementOtherDownload,
			app.ReplacementUnmonitored, app.ReplacementUnaired, app.ReplacementUnclear,
			app.ReplacementUnclear, app.ReplacementUnclear, app.ReplacementUnclear,
		}},
		{name: "download changed", changed: true},
		{name: "episode list incomplete", partial: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			local, err := store.Open(ctx, filepath.Join(t.TempDir(), "data"))
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			var queueReads int
			transport := fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != "fixture-key" {
					t.Fatalf("unexpected Sonarr request %s", r.Method)
				}
				var body any
				switch r.URL.Path {
				case "/api/v3/system/status":
					body = map[string]any{"appName": "Sonarr", "version": "5.0.0.2941", "isWindows": true}
				case "/api/v3/queue":
					queueReads++
					rows := make([]map[string]any, 0, 11)
					for episode := 1; episode <= 9; episode++ {
						title := release
						if test.changed && queueReads == 3 {
							title = "Different.Release"
						}
						rows = append(rows, map[string]any{
							"id": episode + 300, "seriesId": 41, "episodeId": episode, "downloadId": "original",
							"title": title, "downloadClient": "Fixture", "protocol": "usenet", "status": "completed",
							"trackedDownloadState": "importPending", "series": map[string]any{"id": 41, "title": "Fixture Show"},
							"episode":        map[string]any{"id": episode, "seriesId": 41, "seasonNumber": 1, "episodeNumber": episode},
							"statusMessages": []map[string]any{{"messages": []string{`No files found are eligible for import in C:\Synthetic\downloads\pack`}}},
						})
					}
					rows = append(rows, map[string]any{"id": 401, "seriesId": 41, "episodeId": 3, "downloadId": "different",
						"status": "downloading", "statusMessages": []map[string]any{{"messages": []string{"No files found are eligible for import"}}}})
					if test.unknownJob {
						rows = append(rows, map[string]any{"id": 402, "seriesId": 41, "downloadId": "unmapped", "status": "downloading"})
					}
					body = map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(rows), "records": rows}
				case "/api/v3/series/41":
					body = map[string]any{"id": 41, "monitored": true, "runtime": 11}
				case "/api/v3/history":
					if r.URL.Query().Get("downloadId") != "original" {
						t.Fatalf("read history for the wrong release: %s", r.URL.RawQuery)
					}
					body = map[string]any{"page": 1, "pageSize": 50, "totalRecords": 0, "records": []any{}}
				case "/api/v3/episode":
					ids := r.URL.Query()["episodeIds"]
					if !reflect.DeepEqual(ids, []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"}) {
						t.Fatalf("checked episodes outside exact pack: %v", ids)
					}
					rows := make([]map[string]any, 0, 9)
					for episode := 1; episode <= 9; episode++ {
						if test.partial && episode == 9 {
							continue
						}
						file := 0
						if episode == 2 || test.everyoneHasFile {
							file = 100 + episode
						}
						air := "2025-01-01T00:00:00Z"
						if episode == 5 {
							air = "2027-01-01T00:00:00Z"
						}
						rows = append(rows, map[string]any{"id": episode, "seriesId": 41, "monitored": episode != 4,
							"episodeFileId": file, "hasFile": file > 0, "airDateUtc": air})
					}
					body = rows
				default:
					t.Fatalf("unexpected Sonarr route %s", r.URL.Path)
				}
				payload, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
			})
			clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
			observations := observeservice.New(local, clock, transport)
			instance, err := observations.AddInstance(ctx, observeservice.InstanceInput{Product: domain.ProductSonarr,
				DisplayName: "Fixture", BaseURL: "http://sonarr.invalid", APIKey: "fixture-key", Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			if err := observations.Collect(ctx, instance.Ref.ID); err != nil {
				t.Fatal(err)
			}
			page, err := observations.Cases(ctx, app.CaseQuery{Reason: domain.ReasonCode(observe.FindingNoEligibleFiles)})
			if err != nil || len(page.Cases) != 1 || len(page.Cases[0].Targets) != 9 {
				t.Fatalf("missing exact season pack: cases=%d error=%v", len(page.Cases), err)
			}
			item := page.Cases[0]
			svc := NewPreviewService(observations, local, transport, clock, nil)
			got, err := svc.ImportPreview(ctx, item.ID, item.Revision)
			if test.changed {
				if !errors.Is(err, app.ErrConflict) {
					t.Fatalf("expected stale release hold: %v", err)
				}
				return
			}
			if test.partial {
				if !errors.Is(err, app.ErrHeld) || got.Replacement == nil || got.Replacement.SearchCount != 0 {
					t.Fatalf("expected incomplete read hold: %+v %v", got, err)
				}
				return
			}
			if err != nil || queueReads != 3 || got.Replacement == nil || len(got.Replacement.Episodes) != 9 {
				t.Fatalf("wanted preview did not recheck the pack: %+v, queue reads %d, %v", got, queueReads, err)
			}
			var searches, unclear int
			for i, episode := range got.Replacement.Episodes {
				if episode.Label != fmt.Sprintf("S01E%02d", i+1) || episode.Status != test.expected[i] || episode.Target.ID != int64(i+1) {
					t.Fatalf("unexpected recommendation for episode %d: %+v", i+1, episode)
				}
				if episode.Status == app.ReplacementSearch {
					searches++
				}
				if episode.Status == app.ReplacementUnclear {
					unclear++
				}
			}
			if got.Replacement.SearchCount != searches || got.Replacement.UnclearCount != unclear {
				t.Fatalf("summary does not match recommendations: %+v", got.Replacement)
			}
		})
	}
}
