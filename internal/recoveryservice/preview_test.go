package recoveryservice

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestImportPreviewUsesExactSavedCaseAndReadOnlyRequests(t *testing.T) {
	for _, test := range []struct {
		name           string
		product        domain.Product
		filenameReview bool
		trackedState   string
		wantRead       bool
	}{
		{"sonarr import pending", domain.ProductSonarr, false, "importPending", true},
		{"radarr import pending", domain.ProductRadarr, false, "importPending", true},
		{"sonarr filename import pending", domain.ProductSonarr, true, "importPending", true},
		{"sonarr filename import blocked", domain.ProductSonarr, true, "importBlocked", true},
		{"sonarr filename without a tracked state", domain.ProductSonarr, true, "", false},
		{"sonarr ordinary preview import blocked", domain.ProductSonarr, false, "importBlocked", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			product := test.product
			ctx := t.Context()
			dir := filepath.Join(t.TempDir(), "state")
			local, err := store.Open(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			secrets := filepath.Join(dir, "secrets")
			if err := os.Mkdir(secrets, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(secrets, "fixture"), []byte("fixture-key\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			version, queueTarget := "4.0.20.3014", `"seriesId":12,"episodeId":41`
			media := `"series":{"id":12,"qualityProfileId":3},"episodes":[{"id":41,"seriesId":12,"episodeFileId":0}],"releaseType":"singleEpisode"`
			source, modifier, language := "web", "", ""
			if product == domain.ProductRadarr {
				version, queueTarget = "6.4.4.10685", `"movieId":41`
				media = `"movie":{"id":41,"qualityProfileId":3,"movieFileId":0}`
				source, modifier, language = "webdl", `,"modifier":"none"`, `,"language":{"id":1}`
			}
			const folder = "Captain.Wattle.Animated.S02E11.Captain.Wattle.Animated.720p.FIXTURE.WEB-DL.AAC2.0.x264-Lab"
			output := "/private/downloads/original"
			if test.filenameReview {
				version, output = "4.0.20.3012", "/private/downloads/"+folder
				queueTarget += `,"series":{"id":12,"title":"Captain Wattle: The Animated Series"},"episode":{"id":41,"seriesId":12,"seasonNumber":2,"episodeNumber":11,"title":"Signal Lost"}` +
					`,"statusMessages":[{"title":"Fixture","messages":["Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible. See the FAQ for details."]}]`
			}
			var calls, previews int
			transport := fixtureTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != "fixture-key" || r.Body != nil {
					t.Fatal("unexpected Arr request")
				}
				var body string
				switch r.URL.Path {
				case "/api/v3/system/status":
					body = fmt.Sprintf(`{"appName":%q,"version":%q,"osName":"Alpine Linux","isLinux":true}`, product, version)
				case "/api/v3/queue":
					body = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":4,` + queueTarget + `,"downloadId":"private-original","downloadClient":"fixture-sab","protocol":"usenet","status":"completed",` +
						fmt.Sprintf(`"trackedDownloadState":%q,"outputPath":%q}]}`, test.trackedState, output)
				case "/api/v3/rootfolder":
					body = `[{"id":1,"path":"/private/library"}]`
				case "/api/v3/episode/41":
					if test.filenameReview {
						t.Fatal("filename review reached the import eligibility contract")
					}
					body = `{"id":41,"seriesId":12,"seasonNumber":1,"episodeFileId":0,"hasFile":false,"monitored":true}`
				case "/api/v3/series/12":
					body = `{"id":12,"qualityProfileId":3,"monitored":true,"path":"/private/library/series","rootFolderPath":"/private/library"}`
				case "/api/v3/movie/41":
					body = `{"id":41,"qualityProfileId":3,"monitored":true,"movieFileId":0,"hasFile":false,"isAvailable":true,"path":"/private/library/movie","rootFolderPath":"/private/library"}`
				case "/api/v3/qualityprofile/3":
					body = `{"id":3,"upgradeAllowed":false,"cutoff":5,"minFormatScore":0,"cutoffFormatScore":0,"minUpgradeFormatScore":0,"items":[{"quality":{"id":5},"allowed":true,"items":[]}],"formatItems":[]` + language + "}"
				case "/api/v3/manualimport":
					if r.URL.Query().Get("downloadId") != "private-original" || r.URL.Query().Get("filterExistingFiles") != "false" {
						t.Fatal("preview did not use the saved scoped selector")
					}
					previews++
					if test.filenameReview {
						body = fmt.Sprintf(`[{"path":%q,"size":4096,"rejections":[{"type":"permanent"}]}]`, output+"/"+folder+".mkv")
						break
					}
					body = `[{"path":"/private/downloads/original/media.mkv","size":4096,"folderName":"original",` + media +
						`,"quality":{"quality":{"id":5,"name":"WEBDL-720p","source":"` + source + `","resolution":720` + modifier +
						`},"revision":{"version":1,"real":0,"isRepack":false}},"languages":[{"id":1,"name":"English"}],"customFormats":[],"customFormatScore":0,"indexerFlags":0,"releaseGroup":"fixture","rejections":[]}]`
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
			cases := observeservice.New(local, clock, transport)
			instance, err := cases.AddInstance(ctx, observeservice.InstanceInput{Product: product, DisplayName: "Test Arr",
				BaseURL: "http://arr.invalid", SecretRef: "fixture", Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			if err := cases.Collect(ctx, instance.Ref.ID); err != nil {
				t.Fatal(err)
			}
			page, err := cases.Cases(ctx, app.CaseQuery{Instance: instance.Ref.ID})
			if err != nil || len(page.Cases) != 1 {
				t.Fatalf("saved case unavailable: %v", err)
			}
			item := page.Cases[0]
			svc := NewPreviewService(cases, local, transport, clock)
			if _, err := svc.ImportPreview(ctx, item.ID, item.Revision+1); err != app.ErrConflict || calls != 2 {
				t.Fatalf("stale request reached Arr: %d calls, %v", calls, err)
			}
			got, err := svc.ImportPreview(ctx, item.ID, item.Revision)
			if !test.wantRead {
				if previews != 0 {
					t.Fatal("an unsupported queue state reached Sonarr’s file preview")
				}
				if test.filenameReview {
					if err != nil || got.Eligible != 0 || got.Episode == nil || got.Episode.Matched || got.Episode.Reason == "" {
						t.Fatalf("expected a filename-specific hold: %+v, %+v, %v", got, got.Episode, err)
					}
				} else if err != app.ErrHeld || got.Episode != nil || got.Hold == "" {
					t.Fatalf("expected an import preview hold: %+v, %v", got, err)
				}
				return
			}
			if test.filenameReview {
				if err != nil || previews != 1 || got.Eligible != 0 || got.Episode == nil || !got.Episode.Matched ||
					got.Episode.Folder != folder || got.Episode.Filename != folder+".mkv" || got.Episode.EpisodeTitle != "Signal Lost" {
					t.Fatalf("unexpected episode review: %+v, %+v with %d preview requests: %v", got, got.Episode, previews, err)
				}
				return
			}
			if err != nil || got.Candidates != 1 || got.Eligible != 1 || previews != 1 {
				t.Fatalf("unexpected import assessment: %+v with %d preview requests, %v", got, previews, err)
			}
		})
	}
}
