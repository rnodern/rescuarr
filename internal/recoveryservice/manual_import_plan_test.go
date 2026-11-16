package recoveryservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestManualImportPlanBindsSourceProfileAndLanguagePolicy(t *testing.T) {
	ctx := t.Context()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	const folder = "Captain.Wattle.Animated.S02E11.Signal.Lost.720p.WEB-DL-Lab"
	const output = "/private/downloads/" + folder
	const original = "fixture-download-id"
	const queue = `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":4,"seriesId":12,"episodeId":41,` +
		`"series":{"id":12,"title":"Captain Wattle: The Animated Series"},"episode":{"id":41,"seriesId":12,"seasonNumber":2,"episodeNumber":11,"title":"Signal Lost"},` +
		`"statusMessages":[{"title":"Fixture","messages":["Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible. See the FAQ for details."]}],` +
		`"downloadId":"` + original + `","downloadClient":"fixture-sab","protocol":"usenet","status":"completed",` +
		`"trackedDownloadState":"importBlocked","title":"` + folder + `","outputPath":"` + output + `"}]}`
	const episode = `{"id":41,"seriesId":12,"seasonNumber":2,"episodeNumber":%d,"episodeFileId":0,"hasFile":false,"monitored":true}`
	const series = `{"id":12,"qualityProfileId":3,"monitored":true,"path":"/private/library/series","rootFolderPath":"/private/library","originalLanguage":{"id":1,"name":"English"}}`
	size, profileScore, episodeNumber, candidateReads := 4096, 0, 11, 0
	transport := fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != "fixture-key" || r.Body != nil {
			t.Fatalf("plan used a non-read-only request %s %s", r.Method, r.URL.Path)
		}
		var body string
		switch r.URL.Path {
		case "/api/v3/system/status":
			body = `{"appName":"Sonarr","version":"5.0.0.2941","isLinux":true}`
		case "/api/v3/queue":
			body = queue
		case "/api/v3/episode/41":
			body = fmt.Sprintf(episode, episodeNumber)
		case "/api/v3/series/12":
			body = series
		case "/api/v3/qualityprofile/3":
			body = fmt.Sprintf(`{"id":3,"upgradeAllowed":false,"cutoff":5,"minFormatScore":%d,"cutoffFormatScore":0,"minUpgradeFormatScore":0,"items":[{"quality":{"id":5},"allowed":true,"items":[]}],"formatItems":[]}`, profileScore)
		case "/api/v3/rootfolder":
			body = `[{"id":1,"path":"/private/library"}]`
		case "/api/v3/history":
			body = `{"page":1,"pageSize":100,"totalRecords":0,"records":[]}`
		case "/api/v3/manualimport":
			candidateReads++
			if r.URL.Query().Get("downloadId") != original || r.URL.Query().Get("filterExistingFiles") != "false" {
				t.Fatal("plan must query the exact download")
			}
			body = fmt.Sprintf(`[{"path":%q,"size":%d,"folderName":%q,"downloadId":%q,"series":null,`+
				`"quality":{"quality":{"id":5,"name":"WEBDL-720p","source":"web","resolution":720},"revision":{"version":1,"real":0,"isRepack":false}},`+
				`"languages":[{"id":1,"name":"English"}],"indexerFlags":0,"releaseGroup":"fixture","releaseType":"singleEpisode",`+
				`"rejections":[{"type":"permanent","reason":"Unknown episode"}]}]`, output+"/"+folder+".mkv", size, folder, original)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	cases := observeservice.New(local, clock, transport)
	instance, err := cases.AddInstance(ctx, observeservice.InstanceInput{Product: domain.ProductSonarr, DisplayName: "Test Sonarr",
		BaseURL: "http://sonarr.invalid", APIKey: "fixture-key", Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cases.Collect(ctx, instance.Ref.ID); err != nil {
		t.Fatal(err)
	}
	page, err := cases.Cases(ctx, app.CaseQuery{Instance: instance.Ref.ID})
	if err != nil || len(page.Cases) != 1 {
		t.Fatalf("expected one saved case: %v", err)
	}
	item := page.Cases[0]
	read := func() manualImportPlan {
		t.Helper()
		p, err := prepareManualImport(ctx, cases, local, clock, transport, item.ID, item.Revision, nil)
		if err != nil || p.scopeHash == "" || p.release != folder || p.preview.Episode == nil ||
			!p.preview.Episode.Matched || !p.preview.Episode.TitleInNames ||
			p.preview.Episode.Filename != folder+".mkv" || len(p.preview.Audio) != 1 ||
			p.preview.Audio[0].Decision != domain.AudioReview || p.preview.Audio[0].OriginalLanguage != "English" ||
			len(p.preview.Audio[0].ReleaseLanguages) != 1 || len(p.preview.Audio[0].TrackLanguages) != 0 {
			t.Fatalf("unexpected plan and assessment %+v: %v", p.preview, err)
		}
		return p
	}
	first := read()
	if unchanged := read(); unchanged.scopeHash != first.scopeHash {
		t.Fatal("identical evidence changed the approval hash")
	}
	size++
	second := read()
	if second.scopeHash == first.scopeHash {
		t.Fatal("source size changed without invalidating the approval")
	}
	profileScore++
	third := read()
	if third.scopeHash == second.scopeHash {
		t.Fatal("profile evidence changed without invalidating the approval")
	}
	data, err := json.Marshal(third.preview)
	if err != nil || strings.Contains(string(data), "/private") || strings.Contains(string(data), original) ||
		strings.Contains(string(data), "fixture-key") {
		t.Fatal("read-only preview exposed private details")
	}
	priorCandidateReads := candidateReads
	episodeNumber++
	changed, err := prepareManualImport(ctx, cases, local, clock, transport, item.ID, item.Revision, nil)
	if !errors.Is(err, app.ErrHeld) || changed.scopeHash != "" || candidateReads != priorCandidateReads {
		t.Fatalf("an episode renumbered after the queue snapshot reached candidate inspection: %v", err)
	}
	episodeNumber--
	prefs := app.DefaultLanguagePreferences()
	prefs.UnknownAudio = domain.UnknownAudioReject
	if _, err := app.NewLanguageSettings(local, clock).SaveLanguageSettings(ctx, prefs, "owner"); err != nil {
		t.Fatal(err)
	}
	rejected, err := prepareManualImport(ctx, cases, local, clock, transport, item.ID, item.Revision, nil)
	if !errors.Is(err, app.ErrHeld) || rejected.scopeHash != "" || rejected.preview.Hold == "" ||
		rejected.preview.Episode == nil || len(rejected.preview.Audio) != 1 || rejected.preview.Audio[0].Decision != domain.AudioReject {
		t.Fatalf("explicit audio rejection did not hold approval: %+v, %v", rejected.preview, err)
	}

	tagged, err := prepareManualImport(ctx, cases, local, clock, transport, item.ID, item.Revision,
		func(_ context.Context, style importflow.PathStyle, source importflow.SonarrV5Source) ([]string, bool) {
			if style != importflow.UnixPath || source.DownloadID != original || source.Path != output+"/"+folder+".mkv" || source.Size != int64(size) {
				t.Fatalf("the audio reader did not receive the exact candidate: %+v", source)
			}
			return []string{"eng"}, true
		})
	if err != nil || tagged.scopeHash == "" || len(tagged.preview.Audio) != 1 ||
		tagged.preview.Audio[0].Reason != "original_audio_present" {
		t.Fatalf("tagged audio did not support an eligible approval: %+v, %v", tagged.preview, err)
	}

	wrongLanguage, err := prepareManualImport(ctx, cases, local, clock, transport, item.ID, item.Revision,
		func(context.Context, importflow.PathStyle, importflow.SonarrV5Source) ([]string, bool) {
			return []string{"heb"}, true
		})
	if !errors.Is(err, app.ErrHeld) || wrongLanguage.scopeHash != "" || len(wrongLanguage.preview.Audio) != 1 ||
		wrongLanguage.preview.Audio[0].Reason != "original_audio_absent" {
		t.Fatalf("wrong-language file was eligible: %+v, %v", wrongLanguage.preview, err)
	}
}
