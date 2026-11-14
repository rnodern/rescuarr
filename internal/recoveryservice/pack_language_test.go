package recoveryservice

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
)

func TestPackTitleLanguages(t *testing.T) {
	for _, tt := range []struct {
		title string
		want  []string
	}{
		{"Example.Show.S01.720p.HMAX.WEB-DL.HebDub.DDP2.0.H.264-FIXTURE", []string{"HEBREW"}},
		{"Example.Show.S01.720p.HMAX.WEB-DL.DDP2.0.H.264-FIXTURE", nil},
		{"Example.Show.S01.720p.HMAX.WEB-DL.HebDubbed.DDP2.0.H.264-FIXTURE", nil},
	} {
		if got := packTitleLanguages(tt.title); !slices.Equal(got, tt.want) {
			t.Errorf("packTitleLanguages(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}

func TestPackLanguageReviewUsesExactGrabAndCurrentLibraryFile(t *testing.T) {
	const title = "Example.Show.S01.720p.HMAX.WEB-DL.HebDub.DDP2.0.H.264-FIXTURE"
	const basename = "Example.Show.S01E01.HebDub.mkv"
	const pack = "fixture-pack"
	instance := domain.Instance{Ref: domain.InstanceRef{ID: "test", Epoch: 1}, Product: domain.ProductSonarr,
		BaseURL: "http://sonarr.invalid", SecretRef: "fixture", ConnectionRevision: 1, UpstreamVersion: arrwire.SonarrIDOnlyImportVersion}
	episode := domain.TargetRef{Instance: instance.Ref, Kind: domain.TargetEpisode, SeriesID: 4, ID: 1}
	proof := observeservice.RecoveryQueue{
		Instance: instance, Members: []observe.QueueRecord{{Release: title, Target: episode, DownloadID: pack}},
		Queue: observe.QueueSnapshot{Evidence: domain.Observation{SnapshotGeneration: 1}},
	}
	plan := &app.ReplacementPreview{Episodes: []app.ReplacementEpisode{
		{Target: episode, Label: "S01E01", Status: app.ReplacementInLibrary},
		{Label: "S01E02", Status: app.ReplacementSearch},
	}}
	for _, scenario := range []struct {
		name, grabTitle, grabLanguage, importDownload string
		wantAgreement                                 bool
		wantOrigin                                    string
	}{
		{"both agree", title, "Hebrew", pack, true, "same_release"},
		{"another file", title, "Hebrew", "other-download", true, "other_release"},
		{"English release metadata", title, "English", pack, false, ""},
		{"different release", "Different.Show.S01.HebDub.720p", "Hebrew", pack, false, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			packReads, fileReads := 0, 0
			arr, err := observe.NewArr(observe.Config{Instance: instance, APIKey: "example"}, fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatalf("preview sent %s", r.Method)
				}
				var body string
				switch r.URL.Path {
				case "/api/v3/history":
					if r.URL.Query().Get("downloadId") == pack {
						packReads++
						body = fmt.Sprintf(`{"page":1,"pageSize":50,"totalRecords":1,"records":[{"id":8,"seriesId":4,"episodeId":1,"downloadId":%q,"eventType":"grabbed","sourceTitle":%q,"languages":[{"id":23,"name":%q}]}]}`, pack, scenario.grabTitle, scenario.grabLanguage)
					} else if r.URL.Query().Get("episodeId") == "1" {
						body = fmt.Sprintf(`{"page":1,"pageSize":50,"totalRecords":1,"records":[{"id":9,"seriesId":4,"episodeId":1,"downloadId":%q,"eventType":"downloadFolderImported","date":"2025-12-30T11:00:00Z","data":{"fileId":"5"}}]}`, scenario.importDownload)
					}
				case "/api/v3/episode/1":
					body = `{"id":1,"seriesId":4,"episodeFileId":5,"hasFile":true}`
				case "/api/v3/episodefile/5":
					fileReads++
					body = fmt.Sprintf(`{"id":5,"seriesId":4,"relativePath":%q,"mediaInfo":{"audioLanguages":"heb/eng","audioStreamCount":2}}`, basename)
				}
				if body == "" {
					t.Fatalf("preview made unexpected request %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			got := reviewPackLanguage(t.Context(), arr, proof, plan, time.Now())
			if got == nil || got.Agreement != scenario.wantAgreement || packReads != 1 {
				t.Fatalf("unexpected release evidence: %+v, %d grabs", got, packReads)
			}
			if scenario.wantOrigin == "" && (len(got.Files) > 0 || fileReads > 0) {
				t.Fatalf("unconfirmed release reached a file: %+v", got)
			}
			if scenario.wantOrigin != "" && (len(got.Files) != 1 || got.Files[0].Origin != scenario.wantOrigin) {
				t.Fatalf("unexpected file origin: %+v", got.Files)
			}
			if scenario.wantOrigin == "same_release" && (fileReads != 1 || got.Files[0].Filename != basename ||
				!got.Files[0].AudioLanguagesComplete || !slices.Equal(got.Files[0].AudioLanguages, []string{"heb", "eng"})) {
				t.Fatalf("lost file details including the English audio track: %+v", got.Files[0])
			}
		})
	}
}
