package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
)

func TestSonarrPackEpisodeFile(t *testing.T) {
	const current = `{"id":41,"seriesId":12,"episodeFileId":62,"hasFile":true}`
	const swapped = `{"id":41,"seriesId":12,"episodeFileId":83,"hasFile":true}`
	const absent = `{"id":41,"seriesId":12,"episodeFileId":0,"hasFile":false}`
	const imported = `{"id":55,"seriesId":12,"episodeId":41,"downloadId":"exact-private-download",
		"eventType":"downloadFolderImported","date":"2025-12-30T11:00:00Z","data":{"fileId":"62"}}`
	const older = `{"id":53,"seriesId":12,"episodeId":41,"downloadId":"exact-private-download",
		"eventType":"downloadFolderImported","date":"2025-11-30T11:00:00Z","data":{"fileId":"60"}}`
	const replacement = `{"id":58,"seriesId":12,"episodeId":41,"downloadId":"different-download",
		"eventType":"downloadFolderImported","date":"2025-12-31T11:00:00Z","data":{"fileId":"62"}}`
	const file = `{"id":62,"seriesId":12,"relativePath":"Season 01\\Synthetic.S01E01.HebDub.mkv",
		"path":"C:\\private-library\\Season 01\\Synthetic.S01E01.HebDub.mkv","languages":[{"id":23,"name":"Hebrew"}],
		"mediaInfo":{"audioLanguages":"heb/eng","audioStreamCount":2,"subtitles":"spa"}}`

	for _, fixture := range []struct {
		name       string
		version    string
		initial    string
		final      string
		records    []string
		file       string
		want       PackEpisodeFileState
		wantErr    error
		wantTracks []string
	}{
		{"linked", sonarrV5Needs, current, current, []string{imported, older}, file, PackFileLinked, nil, []string{"heb", "eng"}},
		{"missing", sonarrV5Needs, absent, "", nil, "", PackFileMissing, nil, nil},
		{"other download", sonarrV5Needs, current, "", []string{replacement, older}, "", PackFileDifferentDownload, nil, nil},
		{"metadata missing a track", sonarrV5Needs, current, current, []string{imported},
			strings.Replace(file, "heb/eng", "eng", 1), PackFileLinked, nil, []string{"eng"}},
		{"unknown import", sonarrV5Needs, current, "", []string{older}, "", PackFileUnverified, nil, nil},
		{"ambiguous current import", sonarrV5Needs, current, "", []string{replacement, imported}, "", PackFileUnverified, nil, nil},
		{"wrong series in history", sonarrV5Needs, current, "", []string{strings.Replace(imported, `"seriesId":12`, `"seriesId":13`, 1)},
			"", PackFileUnverified, ErrPartial, nil},
		{"wrong series in file", sonarrV5Needs, current, "", []string{imported},
			strings.Replace(file, `"seriesId":12`, `"seriesId":13`, 1), PackFileUnverified, ErrResponse, nil},
		{"link changed during read", sonarrV5Needs, current, swapped, []string{imported}, file, PackFileUnverified, ErrPartial, nil},
		{"uncertified version", SonarrTargetVersion, "", "", nil, "", PackFileUnverified, ErrTargetVersion, nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cfg := configuration(domain.ProductSonarr)
			cfg.Instance.UpstreamVersion = fixture.version
			links, history, files := 0, 0, 0
			arr, err := NewArr(cfg, roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Host != "fixture.invalid" ||
					r.Header.Get("X-Api-Key") != "synthetic-api-key" || r.Body != nil {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				switch r.URL.Path {
				case "/namespace/api/v3/episode/41":
					links++
					body := fixture.initial
					if links == 2 {
						body = fixture.final
					}
					if body != "" && links <= 2 {
						return response(http.StatusOK, body), nil
					}
				case "/namespace/api/v3/history":
					history++
					q := r.URL.Query()
					if q.Get("episodeId") != "41" || q.Get("seriesIds") != "12" || q.Get("eventType") != "3" {
						t.Fatalf("import history was not scoped: %v", q)
					}
					if history == 1 && fixture.records != nil {
						return response(http.StatusOK, fmt.Sprintf(`{"page":1,"pageSize":50,"totalRecords":%d,"records":[%s]}`,
							len(fixture.records), strings.Join(fixture.records, ","))), nil
					}
				case "/namespace/api/v3/episodefile/62":
					files++
					if files == 1 && fixture.file != "" {
						return response(http.StatusOK, fixture.file), nil
					}
				}
				t.Fatalf("unexpected request: %s", r.URL.Path)
				return nil, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			got, err := arr.SonarrPackEpisodeFile(t.Context(), fixtureTarget(domain.ProductSonarr), "exact-private-download")
			if got.State != fixture.want || !errors.Is(err, fixture.wantErr) || !slices.Equal(got.AudioLanguages, fixture.wantTracks) ||
				got.AudioLanguagesComplete != (fixture.name == "linked") {
				t.Fatalf("got %+v, %v", got, err)
			}
			if got.State == PackFileLinked {
				if got.FileID != 62 || got.ImportHistoryID != 55 || got.Filename != "Synthetic.S01E01.HebDub.mkv" ||
					!slices.Equal(got.Languages, []domain.ImportLanguage{{ID: 23, Name: "Hebrew"}}) {
					t.Fatalf("incomplete linked-file evidence: %+v", got)
				}
				encoded, err := json.Marshal(got)
				for _, private := range []string{"private-library", "exact-private-download", "FileID", "ImportHistoryID", "spa"} {
					if err != nil || strings.Contains(string(encoded), private) {
						t.Fatalf("private data escaped projection: %s, %v", encoded, err)
					}
				}
			} else if got.FileID != 0 || got.ImportHistoryID != 0 || got.Filename != "" || got.Languages != nil {
				t.Fatalf("unlinked file gained provenance data: %+v", got)
			}
		})
	}
}

func TestReadPackGrabTitleAndLanguages(t *testing.T) {
	arr := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/namespace/api/v3/history" || r.URL.Query().Get("downloadId") != "exact-pack" {
			t.Fatalf("unscoped request: %s %s", r.Method, r.URL.Path)
		}
		return response(http.StatusOK, `{"page":1,"pageSize":50,"totalRecords":2,"records":[
			{"id":55,"seriesId":12,"episodeId":41,"downloadId":"exact-pack","eventType":"grabbed",
			"sourceTitle":"Synthetic.S01.720p.WEB-DL.HebDub-GROUP","languages":[{"id":23,"name":"Hebrew"}]},
			{"id":54,"seriesId":12,"episodeId":42,"downloadId":"exact-pack","eventType":"grabbed",
			"sourceTitle":{},"languages":"malformed optional field"}]}`), nil
	})
	got, err := arr.History(t.Context(), "exact-pack", 1, observedAt)
	if err != nil || len(got.Records) != 2 || got.Records[0].SourceTitle != "Synthetic.S01.720p.WEB-DL.HebDub-GROUP" ||
		!slices.Equal(got.Records[0].Languages, []domain.ImportLanguage{{ID: 23, Name: "Hebrew"}}) ||
		got.Records[1].SourceTitle != "" || got.Records[1].Languages != nil {
		t.Fatalf("unexpected history projection %+v: %v", got, err)
	}
	body, err := json.Marshal(got.Records)
	if err != nil || strings.Contains(string(body), "HebDub") || strings.Contains(string(body), "Hebrew") {
		t.Fatalf("private grab escaped the generic history projection: %s, %v", body, err)
	}
}
