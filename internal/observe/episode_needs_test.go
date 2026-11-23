package observe

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
)

func TestSonarrEpisodeNeedsUsesExactBatchAndSeriesMonitoring(t *testing.T) {
	for _, version := range []string{SonarrTargetVersion, arrwire.SonarrIDOnlyImportVersion} {
		t.Run(version, func(t *testing.T) {
			now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
			ids := make([]int64, 26)
			query := make(url.Values)
			var rows []string
			for i := range ids {
				ids[i] = int64(41 + i)
				query.Add("episodeIds", strconv.FormatInt(ids[i], 10))
				switch ids[i] {
				case 42:
					rows = append(rows, needEpisode(ids[i], 12, true, true, 80, "2026-01-01T00:00:00Z"))
				case 43:
					rows = append(rows, needEpisode(ids[i], 12, false, false, 0, "2026-01-01T00:00:00Z"))
				case 44:
					rows = append(rows, needEpisode(ids[i], 12, true, false, 0, "2026-11-14T08:51:00Z"))
				case 45:
					rows = append(rows, needEpisode(ids[i], 12, true, false, 0, ""))
				default:
					rows = append(rows, needEpisode(ids[i], 12, true, false, 0, "2026-11-14T08:49:00Z"))
				}
			}
			seriesMonitored := true
			var paths []string
			a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Host != "fixture.invalid" || r.Header.Get("X-Api-Key") != "synthetic-api-key" || r.Body != nil {
					t.Fatal("expected authenticated read-only request to Sonarr")
				}
				paths = append(paths, r.URL.Path)
				switch r.URL.Path {
				case "/namespace/api/v3/system/status":
					return response(http.StatusOK, fmt.Sprintf(`{"appName":"Sonarr","version":%q}`, version)), nil
				case "/namespace/api/v3/series/12":
					return response(http.StatusOK, fmt.Sprintf(`{"id":12,"monitored":%t,"runtime":11}`, seriesMonitored)), nil
				case "/namespace/api/v3/episode":
					if !reflect.DeepEqual(r.URL.Query(), query) {
						t.Fatalf("wrong episode scope: %v", r.URL.Query())
					}
					return response(http.StatusOK, "["+strings.Join(rows, ",")+"]"), nil
				default:
					t.Fatalf("unexpected request %s", r.URL)
					return nil, nil
				}
			})
			got, err := a.SonarrEpisodeNeeds(t.Context(), 12, ids, now)
			if err != nil || len(got) != 26 || got[41] != EpisodeMissing || got[42] != EpisodeInLibrary ||
				got[43] != EpisodeUnmonitored || got[44] != EpisodeUnaired || got[45] != EpisodeUnaired || got[66] != EpisodeMissing {
				t.Fatalf("episode needs %+v: %v", got, err)
			}
			if len(paths) != 3 {
				t.Fatalf("wanted three bounded requests, got %v", paths)
			}
			seriesMonitored = false
			got, err = a.SonarrEpisodeNeeds(t.Context(), 12, ids, now)
			if err != nil || got[41] != EpisodeUnmonitored || got[42] != EpisodeInLibrary {
				t.Fatalf("ignored series monitoring: %+v, %v", got, err)
			}
		})
	}
}

func TestSonarrEpisodeNeedsRejectsAmbiguousReadback(t *testing.T) {
	valid := needEpisode(41, 12, true, false, 0, "2026-01-01T00:00:00Z")
	const goodSeries = `{"id":12,"monitored":true,"runtime":11}`
	for _, tc := range []struct {
		name, series, episodes string
		want                   error
	}{
		{"partial", goodSeries, "[]", ErrPartial},
		{"duplicate", goodSeries, "[" + valid + "," + valid + "]", ErrPartial},
		{"wrong episode", goodSeries, "[" + needEpisode(42, 12, true, false, 0, "") + "]", ErrResponse},
		{"wrong series", goodSeries, "[" + needEpisode(41, 13, true, false, 0, "") + "]", ErrResponse},
		{"wrong series resource", `{"id":13,"monitored":true,"runtime":11}`, "[" + valid + "]", ErrResponse},
		{"missing series monitoring", `{"id":12,"runtime":11}`, "[" + valid + "]", ErrResponse},
		{"missing series runtime", `{"id":12,"monitored":true}`, "[" + valid + "]", ErrResponse},
		{"missing episode monitoring", goodSeries, `[{"id":41,"seriesId":12,"hasFile":false,"episodeFileId":0}]`, ErrResponse},
		{"missing file link", goodSeries, `[{"id":41,"seriesId":12,"monitored":true,"hasFile":false}]`, ErrResponse},
		{"conflicting file link", goodSeries, "[" + needEpisode(41, 12, true, false, 80, "") + "]", ErrResponse},
		{"unknown version", "", "", ErrTargetVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/namespace/api/v3/system/status":
					version := arrwire.SonarrIDOnlyImportVersion
					if tc.name == "unknown version" {
						version = "6.0.0.1" // A future major version has no qualified contract.
					}
					return response(http.StatusOK, fmt.Sprintf(`{"appName":"Sonarr","version":%q}`, version)), nil
				case "/namespace/api/v3/series/12":
					if tc.series == "" {
						t.Fatal("queried a target after detecting an unsupported contract")
					}
					return response(http.StatusOK, tc.series), nil
				case "/namespace/api/v3/episode":
					return response(http.StatusOK, tc.episodes), nil
				default:
					t.Fatalf("unexpected request %s", r.URL)
					return nil, nil
				}
			})
			got, err := a.SonarrEpisodeNeeds(t.Context(), 12, []int64{41}, observedAt)
			if !errors.Is(err, tc.want) || got != nil {
				t.Fatalf("got %+v, %v, want %v", got, err, tc.want)
			}
		})
	}
	a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
		t.Fatalf("invalid target IDs emitted request %s", r.URL)
		return nil, nil
	})
	for _, ids := range [][]int64{nil, {0}, {41, 41}, make([]int64, 129)} {
		if _, err := a.SonarrEpisodeNeeds(t.Context(), 12, ids, observedAt); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("bad episode request %v: %v", ids, err)
		}
	}
}

func needEpisode(id, seriesID int64, monitored, hasFile bool, fileID int, airDate string) string {
	date := "null"
	if airDate != "" {
		date = strconv.Quote(airDate)
	}
	return fmt.Sprintf(`{"id":%d,"seriesId":%d,"monitored":%t,"hasFile":%t,"episodeFileId":%d,"airDateUtc":%s}`,
		id, seriesID, monitored, hasFile, fileID, date)
}
