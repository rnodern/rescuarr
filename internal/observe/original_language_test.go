package observe

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

func TestOriginalAudioLanguage(t *testing.T) {
	for _, tc := range []struct {
		name, version, resource, body string
		product                       domain.Product
		want                          domain.ImportLanguage
		wantErr                       error
	}{
		{"Sonarr stable", SonarrTargetVersion, "series/12", `{"id":12,"originalLanguage":{"id":2,"name":"French"}}`, domain.ProductSonarr, domain.ImportLanguage{ID: 2, Name: "French"}, nil},
		{"Sonarr develop", arrwire.SonarrIDOnlyImportVersion, "series/12", `{"id":12,"originalLanguage":{"id":1,"name":"English"}}`, domain.ProductSonarr, domain.ImportLanguage{ID: 1, Name: "English"}, nil},
		{"Radarr", RadarrTargetVersion, "movie/41", `{"id":41,"originalLanguage":{"id":21,"name":"Korean"}}`, domain.ProductRadarr, domain.ImportLanguage{ID: 21, Name: "Korean"}, nil},
		{"metadata omitted", arrwire.SonarrIDOnlyImportVersion, "series/12", `{"id":12,"title":"Obviously French"}`, domain.ProductSonarr, domain.ImportLanguage{}, nil},
		{"metadata unknown", RadarrTargetVersion, "movie/41", `{"id":41,"originalLanguage":{"id":0,"name":"Unknown"}}`, domain.ProductRadarr, domain.ImportLanguage{}, nil},
		{"invalid language shape", RadarrTargetVersion, "movie/41", `{"id":41,"originalLanguage":"Korean"}`, domain.ProductRadarr, domain.ImportLanguage{}, nil},
		{"mismatched series", arrwire.SonarrIDOnlyImportVersion, "series/12", `{"id":42,"originalLanguage":{"id":21,"name":"Korean"}}`, domain.ProductSonarr, domain.ImportLanguage{}, ErrResponse},
		{"unqualified version", "6.0.0.1", "", "", domain.ProductSonarr, domain.ImportLanguage{}, ErrTargetVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := []string{"system/status"}
			if tc.resource != "" {
				reads = append(reads, tc.resource)
			}
			count := 0
			arr := arrFor(t, tc.product, func(r *http.Request) (*http.Response, error) {
				if count >= len(reads) || r.Method != http.MethodGet || r.URL.Path != "/namespace/api/v3/"+reads[count] ||
					r.URL.Host != "fixture.invalid" || r.Header.Get("X-Api-Key") != "synthetic-api-key" {
					t.Fatalf("unexpected read %s %s", r.Method, r.URL.Path)
				}
				count++
				if count == 1 {
					return response(http.StatusOK, fmt.Sprintf(`{"appName":%q,"version":%q}`, tc.product, tc.version)), nil
				}
				return response(http.StatusOK, tc.body), nil
			})
			got, err := arr.OriginalAudioLanguage(t.Context(), fixtureTarget(tc.product))
			if got != tc.want || !errors.Is(err, tc.wantErr) || count != len(reads) {
				t.Fatalf("original language = %+v, reads %d, error %v", got, count, err)
			}
		})
	}
}

func TestRadarrLibraryFileAudioTracks(t *testing.T) {
	requests := targetFixture(domain.ProductRadarr, true)
	for i := range requests {
		if requests[i].path == "moviefile/62" {
			requests[i].body = strings.TrimSuffix(requests[i].body, "}") +
				`,"mediaInfo":{"audioLanguages":"kor/eng","audioStreamCount":2,"subtitles":"ita"}}`
		}
	}
	snapshot, err := readTargetFixture(t, domain.ProductRadarr, requests)
	if err != nil || snapshot.File == nil {
		t.Fatalf("reading Radarr library file: %v", err)
	}
	first, complete := snapshot.File.AudioTracks()
	if !complete || !slices.Equal(first, []string{"kor", "eng"}) {
		t.Fatalf("unexpected tracks: %v, complete=%t", first, complete)
	}
	first[0] = "unrelated"
	tracks, _ := snapshot.File.AudioTracks()
	if !slices.Equal(tracks, []string{"kor", "eng"}) {
		t.Fatalf("caller changed the source projection: %v", tracks)
	}
}
