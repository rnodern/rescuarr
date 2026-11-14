package observe

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rnodern/rescuarr/internal/domain"
)

// These minimal fixture responses project the tagged Resource and Controller
// contracts for Sonarr v4.0.20.3014 and Radarr v6.4.4.10685.
const (
	fixtureEpisode = `{"id":41,"seriesId":12,"seasonNumber":1,"episodeFileId":62,"hasFile":true,"monitored":true,
		"airDateUtc":"2025-11-23T22:00:00Z"}`
	fixtureSeries = `{"id":12,"qualityProfileId":3,"monitored":true,"path":"/private/library/series",
		"rootFolderPath":"/private/library","originalLanguage":{"id":8,"name":"Japanese"}}`
	fixtureMovie = `{"id":41,"qualityProfileId":3,"movieFileId":62,"hasFile":true,"monitored":true,"isAvailable":true,
		"path":"/private/library/movie","rootFolderPath":"/private/library","movieFile":{"id":62},
		"originalLanguage":{"id":1,"name":"English"}}`
	fixtureProfile = `{"id":3,"upgradeAllowed":true,"cutoff":1000,"minFormatScore":-5,"cutoffFormatScore":8,
		"minUpgradeFormatScore":1,"items":[{"quality":{"id":0},"allowed":false,"items":[]},
		{"id":1000,"quality":null,"allowed":true,"items":[{"quality":{"id":3},"allowed":true,"items":[]},
		{"quality":{"id":5},"allowed":false,"items":[]}]}],
		"formatItems":[{"format":8,"score":-5},{"format":7,"score":10}]`
	fixtureFile = `{"id":62,"relativePath":"fixture/media.mkv","path":"/private/library/fixture/media.mkv",
		"size":4321,"dateAdded":"2025-11-24T00:00:00Z","languages":[{"id":1,"name":"English"}],
		"customFormats":[{"id":7,"name":"fixture"}],"customFormatScore":10,"qualityCutoffNotMet":true,
		"originalFilePath":"/private/original/media.mkv",`
	fixtureQuality = `"revision":{"version":2,"real":0,"isRepack":true}}`
)

type targetRequest struct {
	path string
	body string
	code int
}

func targetFixture(product domain.Product, linked bool) []targetRequest {
	profile := targetRequest{"qualityprofile/3", fixtureProfile + "}", http.StatusOK}
	file := targetRequest{"episodefile/62", fixtureFile + `"seriesId":12,"seasonNumber":1,
		"quality":{"quality":{"id":3,"source":"web","resolution":1080},` + fixtureQuality + "}", http.StatusOK}
	first := []targetRequest{{"episode/41", fixtureEpisode, http.StatusOK}, {"series/12", fixtureSeries, http.StatusOK}}
	status := targetRequest{"system/status", `{"appName":"Sonarr","version":"4.0.20.3014"}`, http.StatusOK}
	if product == domain.ProductRadarr {
		profile.body = fixtureProfile + `,"language":{"id":-2,"name":"Original"}}`
		file.path = "moviefile/62"
		file.body = fixtureFile + `"movieId":41,"edition":"Private fixture edition",
			"quality":{"quality":{"id":3,"source":"webdl","resolution":1080,"modifier":"none"},` + fixtureQuality + "}"
		first = []targetRequest{{"movie/41", fixtureMovie, http.StatusOK}}
		status.body = `{"appName":"Radarr","version":"6.4.4.10685"}`
	}
	if !linked {
		first[0].body = strings.NewReplacer(
			`FileId":62`, `FileId":0`,
			`"hasFile":true`, `"hasFile":false`,
			`"movieFile":{"id":62}`, `"movieFile":null`,
		).Replace(first[0].body)
	}
	requests := append([]targetRequest{status}, first...)
	requests = append(requests, profile)
	if linked {
		requests = append(requests, file)
	}
	requests = append(requests, first...)
	return append(requests, profile)
}

func fixtureTarget(product domain.Product) domain.TargetRef {
	ref := domain.TargetRef{Instance: configuration(product).Instance.Ref, Kind: domain.TargetEpisode, ID: 41, SeriesID: 12}
	if product == domain.ProductRadarr {
		ref.Kind, ref.SeriesID = domain.TargetMovie, 0
	}
	return ref
}

func readTargetFixture(t *testing.T, product domain.Product, requests []targetRequest) (TargetSnapshot, error) {
	t.Helper()
	requestCount := 0
	cfg := configuration(product)
	cfg.Instance.ConnectionRevision = 9
	arr, err := NewArr(cfg, roundTrip(func(r *http.Request) (*http.Response, error) {
		if requestCount == len(requests) {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		next := requests[requestCount]
		requestCount++
		if r.Method != http.MethodGet || r.URL.Host != "fixture.invalid" || r.URL.Path != "/namespace/api/v3/"+next.path ||
			r.URL.RawQuery != "" || r.Header.Get("X-Api-Key") != "synthetic-api-key" || r.Body != nil {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return response(next.code, next.body), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := arr.Target(t.Context(), fixtureTarget(product), 4, observedAt)
	if requestCount != len(requests) {
		t.Fatalf("made %d requests, expected %d: %v", requestCount, len(requests), err)
	}
	return got, err
}

func TestTargetLinkedFileAndProfile(t *testing.T) {
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		t.Run(string(product), func(t *testing.T) {
			got, err := readTargetFixture(t, product, targetFixture(product, true))
			if err != nil || got.Ref != fixtureTarget(product) || got.ConnectionRevision != 9 ||
				got.Evidence.Kind != TargetProfileEvidence || got.Evidence.SourceRecord != "41" || got.Evidence.SnapshotGeneration != 4 ||
				got.Evidence.Scope.Instance != got.Ref.Instance || !got.Evidence.Fresh(observedAt) ||
				!got.Monitored.Value || !got.Monitored.Observation.Fresh(observedAt) {
				t.Fatalf("target %+v: %v", got, err)
			}
			if product == domain.ProductSonarr {
				if got.Version != SonarrTargetVersion || got.AirDateUTC == nil || got.Available.Observation.State != domain.ObservationUnknown ||
					got.MediaPath != "/private/library/series" || got.OriginalLanguageID == nil || *got.OriginalLanguageID != 8 {
					t.Fatalf("Sonarr target %+v", got)
				}
			} else if got.Version != RadarrTargetVersion || !got.Available.Observation.Fresh(observedAt) || !got.Available.Value ||
				got.MediaPath != "/private/library/movie" || got.OriginalLanguageID == nil || *got.OriginalLanguageID != 1 {
				t.Fatalf("Radarr target %+v", got)
			}
			p := got.Profile
			if p == nil || got.ProfileID != 3 || p.ID != got.ProfileID || got.RootPath != "/private/library" ||
				len(p.Digest) != 64 || p.Revision == 0 || p.Revision != got.ProfileRevision || !p.UpgradeAllowed ||
				p.Cutoff != 1000 || len(p.Items) != 2 || p.Items[1].GroupID != p.Cutoff || len(p.Items[1].Items) != 2 ||
				*p.Items[1].Items[0].QualityID != 3 || p.Items[1].Items[1].Allowed ||
				!slices.Equal(p.Formats, []ProfileFormat{{ID: 7, Score: 10}, {ID: 8, Score: -5}}) ||
				(product == domain.ProductRadarr && (p.LanguageID == nil || *p.LanguageID != -2)) {
				t.Fatalf("profile %+v", p)
			}
			f := got.File
			if f == nil || got.FileID.Value != 62 || !got.FileID.Observation.Fresh(observedAt) ||
				f.ID != got.FileID.Value || f.Quality == nil || f.Quality.ID != 3 || f.Quality.Revision.Version != 2 ||
				!f.Quality.Revision.IsRepack || f.QualityCutoffNotMet == nil || !*f.QualityCutoffNotMet ||
				!slices.Equal(f.FormatIDs, []int32{7}) || !slices.Equal(f.LanguageIDs, []int32{1}) ||
				f.SizeBytes == nil || *f.SizeBytes != 4321 || f.DateAdded == nil || f.RelativePath != "fixture/media.mkv" ||
				(product == domain.ProductRadarr && f.Edition != "Private fixture edition") {
				t.Fatalf("file %+v", f)
			}
			encoded, err := json.Marshal(got)
			if err != nil || strings.Contains(string(encoded), "/private/") || strings.Contains(string(encoded), "fixture/media.mkv") ||
				strings.Contains(string(encoded), "Private fixture edition") {
				t.Fatalf("private paths escaped projection: %v", err)
			}
		})
	}
}

func TestTargetAbsentAndUnmonitored(t *testing.T) {
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		t.Run(string(product), func(t *testing.T) {
			requests := targetFixture(product, false)
			for i, req := range requests {
				if req.path == "series/12" || req.path == "movie/41" {
					requests[i].body = strings.ReplaceAll(req.body, `"monitored":true`, `"monitored":false`)
					requests[i].body = strings.ReplaceAll(requests[i].body, `"isAvailable":true`, `"isAvailable":false`)
				}
			}
			got, err := readTargetFixture(t, product, requests)
			if err != nil || !got.Evidence.Fresh(observedAt) || got.Monitored.Value || !got.Monitored.Observation.Fresh(observedAt) ||
				got.File != nil || got.FileID.Value != 0 || got.FileID.Observation.State != domain.ObservationAbsent ||
				!got.FileID.Observation.Fresh(observedAt) ||
				(product == domain.ProductRadarr && (got.Available.Value || !got.Available.Observation.Fresh(observedAt))) {
				t.Fatalf("exact absent file %+v: %v", got, err)
			}
		})
	}
}

func TestTargetUnknownOptionalFileMetadata(t *testing.T) {
	requests := targetFixture(domain.ProductRadarr, true)
	requests[3].body = strings.NewReplacer(
		`"source":"webdl"`, `"source":"future-source"`,
		`"languages":[{"id":1,"name":"English"}]`, `"languages":null`,
		`"qualityCutoffNotMet":true,`, "",
		`"size":4321`, `"size":-1`,
	).Replace(requests[3].body)
	got, err := readTargetFixture(t, domain.ProductRadarr, requests)
	if err != nil || got.FileID.Value != 62 || !got.FileID.Observation.Fresh(observedAt) || got.File == nil ||
		got.File.Quality != nil || got.File.LanguageIDs != nil || got.File.QualityCutoffNotMet != nil || got.File.SizeBytes != nil {
		t.Fatalf("optional metadata %+v: %v", got.File, err)
	}
}

func TestTargetKeepsUncertainReadsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		product domain.Product
		index   int
		from    string
		to      string
		status  int
		want    error
	}{
		{"version drift", domain.ProductSonarr, 0, "4.0.20.3014", "4.0.20.3015", http.StatusOK, ErrTargetVersion},
		{"file link omitted", domain.ProductRadarr, 1, `"movieFileId":62,`, "", http.StatusOK, ErrResponse},
		{"file link null", domain.ProductSonarr, 1, `"episodeFileId":62`, `"episodeFileId":null`, http.StatusOK, ErrResponse},
		{"file flag contradicts link", domain.ProductRadarr, 1, `"hasFile":true`, `"hasFile":false`, http.StatusOK, ErrResponse},
		{"wrong embedded link", domain.ProductRadarr, 1, `"movieFile":{"id":62}`, `"movieFile":{"id":63}`, http.StatusOK, ErrResponse},
		{"episode belongs to another series", domain.ProductSonarr, 1, `"seriesId":12`, `"seriesId":13`, http.StatusOK, ErrResponse},
		{"wrong profile identity", domain.ProductSonarr, 3, `"id":3,"upgradeAllowed"`, `"id":4,"upgradeAllowed"`, http.StatusOK, ErrResponse},
		{"missing native profile setting", domain.ProductRadarr, 2, `"minUpgradeFormatScore":1,`, "", http.StatusOK, ErrResponse},
		{"wrong file identity", domain.ProductRadarr, 3, `"id":62`, `"id":63`, http.StatusOK, ErrResponse},
		{"wrong file owner", domain.ProductRadarr, 3, `"movieId":41`, `"movieId":42`, http.StatusOK, ErrResponse},
		{"wrong file season", domain.ProductSonarr, 4, `"seasonNumber":1`, `"seasonNumber":2`, http.StatusOK, ErrResponse},
		{"positive link returns 404", domain.ProductSonarr, 4, "", "", http.StatusNotFound, ErrNotFound},
		{"profile read denied", domain.ProductRadarr, 2, "", "", http.StatusForbidden, ErrAccess},
		{"link drift", domain.ProductRadarr, 4, "62", "63", http.StatusOK, ErrPartial},
		{"profile content drift", domain.ProductRadarr, 5, `"score":10`, `"score":11`, http.StatusOK, ErrPartial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := targetFixture(tc.product, true)[:tc.index+1]
			requests[tc.index].body = strings.ReplaceAll(requests[tc.index].body, tc.from, tc.to)
			requests[tc.index].code = tc.status
			got, err := readTargetFixture(t, tc.product, requests)
			state := domain.ObservationUnknown
			if tc.want == ErrTargetVersion {
				state = domain.ObservationUnsupported
			}
			if !errors.Is(err, tc.want) || got.Evidence.State != state ||
				got.Evidence.Fresh(observedAt) || got.FileID.Observation.State != state ||
				got.File != nil || got.Profile != nil || got.ProfileRevision != 0 {
				t.Fatalf("wanted %v and %s, got %+v with %v", tc.want, state, got, err)
			}
		})
	}
}

func TestTargetRefMustBindToConfiguredInstanceAndProduct(t *testing.T) {
	a := arrFor(t, domain.ProductSonarr, func(r *http.Request) (*http.Response, error) {
		t.Fatalf("invalid target triggered a request to %s", r.URL.Path)
		return nil, nil
	})
	for _, mutate := range []func(*domain.TargetRef){
		func(r *domain.TargetRef) { r.ID = math.MaxInt32 + 1 },
		func(r *domain.TargetRef) { r.Instance.Epoch++ },
		func(r *domain.TargetRef) { r.Kind, r.SeriesID = domain.TargetMovie, 0 },
	} {
		ref := fixtureTarget(domain.ProductSonarr)
		mutate(&ref)
		if _, err := a.Target(t.Context(), ref, 4, observedAt); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("invalid reference had error %v", err)
		}
	}
}
