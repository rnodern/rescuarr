package importflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/policy"
)

const (
	fixtureKey  = "synthetic-secret"
	sonarrMedia = `"series":{"id":12,"qualityProfileId":3},"episodes":[{"id":41,"seriesId":12,"episodeFileId":0}]`
	radarrMedia = `"movie":{"id":41,"qualityProfileId":3,"movieFileId":0}`
)

var fixtureTime = time.Date(2026, 1, 1, 0, 0, 40, 0, time.UTC)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sample(product domain.Product, ids ...int64) (observe.Config, Job) {
	instance := domain.Instance{Ref: domain.InstanceRef{ID: "synthetic-owner", Epoch: 3},
		Product: product, BaseURL: "http://fixture.invalid/prefix/"}
	kind, seriesID := domain.TargetEpisode, int64(12)
	instance.UpstreamVersion = SonarrVersion
	if product == domain.ProductRadarr {
		kind, seriesID, instance.UpstreamVersion = domain.TargetMovie, 0, RadarrVersion
	}
	job := Job{Ref: domain.JobRef{Instance: instance.Ref, ID: "opaque-job", Generation: 12},
		DownloadID: "SAB_Lab+original & exact", PathStyle: UnixPath, OutputPath: "/downloads/job", LibraryRoots: []string{"/library"}, TrackingComplete: true}
	job.Queue.Evidence = sampleEvidence(instance, "queue", "")
	for _, id := range ids {
		ref := domain.TargetRef{Instance: instance.Ref, Kind: kind, SeriesID: seriesID, ID: id}
		job.Targets = append(job.Targets, Target{Ref: ref, ProfileID: 3, ProfileRevision: 7, Monitored: true,
			Intent: policy.NeedFirstImport, Evidence: sampleEvidence(instance, TargetProfileEvidence, strconv.FormatInt(id, 10))})
		job.Queue.Records = append(job.Queue.Records, observe.QueueRecord{ID: id + 100, Target: ref,
			DownloadID: job.DownloadID, OutputPath: job.OutputPath, Status: "completed", TrackedState: "importPending"})
	}
	return observe.Config{Instance: instance, APIKey: fixtureKey}, job
}

func sampleEvidence(instance domain.Instance, kind domain.ObservationKind, selector string) domain.Observation {
	return domain.Observation{ID: domain.EvidenceID(string(kind) + "-" + selector), Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: instance.Ref},
		Source: domain.Source(instance.Product), Kind: kind, SourceRecord: selector, State: domain.ObservationKnown,
		Complete: true, SnapshotGeneration: 5, ObservedAt: fixtureTime.Add(-30 * time.Second), ExpiresAt: fixtureTime.Add(time.Minute)}
}

func candidate(path, media string, detail ...string) string {
	modifier := ""
	releaseType := []string{`"releaseType":"multiEpisode"`}
	source := "web"
	if strings.HasPrefix(media, `"movie"`) {
		modifier = `,"modifier":"none"`
		releaseType, source = nil, "webdl"
	}
	fields := []string{fmt.Sprintf(`"path":%q`, path), media, `"size":4096`, `"folderName":"private-folder"`,
		`"quality":{"quality":{"id":5,"name":"WEBDL-720p","source":"` + source + `","resolution":720` + modifier + `},"revision":{"version":1,"real":0,"isRepack":false}}`,
		`"languages":[{"id":1,"name":"English"}]`, `"customFormats":[{"id":9}]`, `"customFormatScore":3`,
		`"indexerFlags":0`, `"releaseGroup":"private-release"`}
	fields = append(fields, releaseType...)
	fields = append(fields, detail...)
	return "{" + strings.Join(fields, ",") + "}"
}

func response(status int, contentType, raw string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}},
		Body: io.NopCloser(strings.NewReader(raw)), ContentLength: -1}
}

func mockClient(t *testing.T, cfg observe.Config, job Job, status int, contentType, body string) (*Client, *int) {
	t.Helper()
	calls := 0
	client, err := New(cfg, fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		wantQuery := url.Values{"downloadId": {job.DownloadID}, "filterExistingFiles": {"false"}}
		if r.Method != http.MethodGet || r.URL.Host != "fixture.invalid" || r.URL.Path != "/prefix/api/v3/manualimport" ||
			!reflect.DeepEqual(r.URL.Query(), wantQuery) || r.Header.Get("X-Api-Key") != fixtureKey || r.Body != nil {
			t.Fatal("unexpected preview request")
		}
		res := response(status, contentType, body)
		res.Header.Set("Location", "https://unexpected.invalid/")
		return res, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

func TestDiscoverSonarrAndRadarr(t *testing.T) {
	for _, tc := range []struct {
		product domain.Product
		media   string
		source  string
	}{
		{domain.ProductSonarr, `"series":{"id":12,"qualityProfileId":3},"seriesId":12,"episodeIds":[42,41],"episodes":[{"id":42,"episodeFileId":0},{"id":41,"seriesId":12,"episodeFileId":0}]`, "web"},
		{domain.ProductRadarr, radarrMedia, "webdl"},
	} {
		t.Run(string(tc.product), func(t *testing.T) {
			ids := []int64{41}
			if tc.product == domain.ProductSonarr {
				ids = append(ids, 42)
			}
			cfg, job := sample(tc.product, ids...)
			job.Queue.Records = append(job.Queue.Records, observe.QueueRecord{DownloadID: "other-job", OutputPath: "/downloads/job-other"})
			row := candidate("/downloads/job/nested/fixture.mkv", tc.media, `"rejections":[]`)
			client, calls := mockClient(t, cfg, job, 200, "application/json; charset=utf-8", "["+row+"]")
			got, err := client.Discover(context.Background(), job, fixtureTime)
			if err != nil || *calls != 1 || got.Assessment != CompleteCandidates || !got.NativeReview || !got.Evidence.Fresh(fixtureTime.Add(time.Second)) {
				t.Fatalf("unexpected preview status %s (%s, %v)", got.Assessment, got.Reason, err)
			}
			if !got.Evidence.ExpiresAt.Equal(fixtureTime.Add(30*time.Second)) || got.Evidence.SnapshotGeneration != job.Queue.Evidence.SnapshotGeneration {
				t.Fatal("preview evidence outlives the source snapshot")
			}
			c := got.Candidates[0]
			if c.State != Eligible || len(c.Targets) != len(job.Targets) || c.Key == "" || c.Quality == nil || c.Quality.Real != 0 || c.Quality.Name != "WEBDL-720p" || c.Quality.Source != tc.source || c.Quality.Resolution != 720 || c.Quality.IsRepack == nil || *c.Quality.IsRepack || c.Size != 4096 ||
				!reflect.DeepEqual(c.Languages, []Language{{ID: 1, Name: "English"}}) || !reflect.DeepEqual(c.FormatIDs, []int64{9}) || *c.CustomFormatScore != 3 || c.IndexerFlags == nil || *c.IndexerFlags != 0 {
				t.Fatal("native candidate metadata or target scope was lost")
			}
			if (tc.product == domain.ProductSonarr && (c.ReleaseType == nil || *c.ReleaseType != ReleaseMulti)) ||
				(tc.product == domain.ProductRadarr && c.ReleaseType != nil) {
				t.Fatal("native release type presence was lost")
			}
			for i, target := range job.Targets {
				if c.Targets[i] != target.Ref || c.Profiles[target.Ref] != target.ProfileRevision {
					t.Fatal("candidate did not bind to the current target profile")
				}
			}
			encoded, err := json.Marshal(got)
			if err != nil || strings.Contains(string(encoded), "/downloads/") || strings.Contains(string(encoded), job.DownloadID) ||
				strings.Contains(string(encoded), fixtureKey) || strings.Contains(string(encoded), "private-") {
				t.Fatal("preview serialisation exposes private upstream data")
			}
		})
	}
}

func TestDiscoverRequiresCurrentJobScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*observe.Config, *Job)
		want error
	}{
		{"unsupported version", func(c *observe.Config, _ *Job) { c.Instance.UpstreamVersion = "4.0.20.3013" }, ErrUnsupported},
		{"changed owner", func(_ *observe.Config, j *Job) { j.Ref.Instance.Epoch++ }, ErrInvalid},
		{"missing target", func(_ *observe.Config, j *Job) { j.Targets = j.Targets[:1] }, ErrStale},
		{"untracked download", func(_ *observe.Config, j *Job) { j.DownloadID = "other" }, ErrStale},
		{"duplicate target", func(_ *observe.Config, j *Job) {
			duplicate := j.Queue.Records[0]
			duplicate.ID += 100
			j.Queue.Records = append(j.Queue.Records, duplicate)
		}, ErrStale},
		{"wrong output", func(_ *observe.Config, j *Job) { j.OutputPath = "/downloads/replaced" }, ErrStale},
		{"overlapping output", func(_ *observe.Config, j *Job) {
			j.Queue.Records = append(j.Queue.Records, observe.QueueRecord{DownloadID: "another-job", OutputPath: "/downloads/job/shared"})
		}, ErrStale},
		{"already importing", func(_ *observe.Config, j *Job) { j.Queue.Records[0].TrackedState = "importing" }, ErrStale},
		{"library overlap", func(_ *observe.Config, j *Job) { j.LibraryRoots[0] = "/downloads" }, ErrInvalid},
		{"stale queue", func(_ *observe.Config, j *Job) { j.Queue.Evidence.ObservedAt = fixtureTime.Add(-time.Minute) }, ErrStale},
		{"partial queue", func(_ *observe.Config, j *Job) { j.Queue.Evidence.Complete = false }, ErrStale},
		{"stale profile", func(_ *observe.Config, j *Job) { j.Targets[0].Evidence.ExpiresAt = fixtureTime }, ErrStale},
		{"other target evidence", func(_ *observe.Config, j *Job) { j.Targets[0].Evidence.SourceRecord = "42" }, ErrStale},
		{"contradictory intent", func(_ *observe.Config, j *Job) { j.Targets[0].FileID = 6 }, ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, job := sample(domain.ProductSonarr, 41, 42)
			tc.edit(&cfg, &job)
			client, calls := mockClient(t, cfg, job, 200, "application/json", "[]")
			got, err := client.Discover(context.Background(), job, fixtureTime)
			if !errors.Is(err, tc.want) || *calls != 0 || got.Assessment != Uncertain || !got.NativeReview {
				t.Fatalf("got %v and %d upstream calls", err, *calls)
			}
		})
	}
}

func TestCandidateMatching(t *testing.T) {
	first := candidate("/downloads/job/episode-41.mkv", sonarrMedia, `"rejections":[]`)
	secondMedia := strings.Replace(sonarrMedia, `"id":41`, `"id":42`, 1)
	second := candidate("/downloads/job/episode-42.mkv", secondMedia, `"rejections":[]`)
	for _, tc := range []struct {
		name   string
		rows   string
		edit   func(*Job)
		want   Assessment
		state  CandidateState
		reason domain.ReasonCode
	}{
		{"partial pack", first, nil, PartialCandidates, Eligible, ""},
		{"two files", first + "," + second, nil, CompleteCandidates, Eligible, ""},
		{"duplicate path", first + "," + strings.Replace(second, "episode-42.mkv", "episode-41.mkv", 1), nil, Ambiguous, Held, candidatesAmbiguous},
		{"overlapping target", first + "," + strings.Replace(first, "episode-41.mkv", "alternate.mkv", 1), nil, Ambiguous, Held, candidatesAmbiguous},
		{"wrong series", strings.Replace(first, `"id":12`, `"id":13`, 1), nil, Ambiguous, Held, identityAmbiguous},
		{"wrong episode", strings.Replace(first, `"id":41`, `"id":43`, 1), nil, Ambiguous, Held, identityAmbiguous},
		{"duplicate episode", strings.Replace(first, `"episodes":[{`, `"episodes":[{"id":41},{`, 1), nil, Ambiguous, Held, identityAmbiguous},
		{"contradictory ids", strings.Replace(first, `"rejections":[]`, `"episodeIds":[42],"rejections":[]`, 1), nil, Ambiguous, Held, identityAmbiguous},
		{"wrong download", strings.Replace(first, `"rejections":[]`, `"downloadId":"other","rejections":[]`, 1), nil, Ambiguous, Held, downloadMismatch},
		{"sibling path", strings.Replace(first, `/job/`, `/job-other/`, 1), nil, Ambiguous, Held, pathOutOfScope},
		{"traversal", strings.Replace(first, `/job/`, `/job/../`, 1), nil, Ambiguous, Held, pathOutOfScope},
		{"out of scope library file", strings.Replace(first, `/downloads/job/`, `/library/`, 1), nil, Ambiguous, Held, pathOutOfScope},
		{"profile changed", strings.Replace(first, `"qualityProfileId":3`, `"qualityProfileId":4`, 1), nil, Ambiguous, Held, targetChanged},
		{"file changed", strings.Replace(first, `"episodeFileId":0`, `"episodeFileId":5`, 1), nil, Ambiguous, Held, targetChanged},
		{"missing revision real", strings.Replace(first, `"real":0,`, "", 1), nil, Ambiguous, Held, metadataUnknown},
		{"unknown release type", strings.Replace(first, `"releaseType":"multiEpisode"`, `"releaseType":"bogus"`, 1), nil, Ambiguous, Held, metadataUnknown},
		{"invalid flags", strings.Replace(first, `"indexerFlags":0`, `"indexerFlags":"freeleech"`, 1), nil, Ambiguous, Held, metadataUnknown},
		{"absent flags", strings.Replace(first, `"indexerFlags":0,`, "", 1), nil, Ambiguous, Held, metadataUnknown},
		{"absent target file", strings.Replace(first, `,"episodeFileId":0`, "", 1), nil, Ambiguous, Held, targetChanged},
		{"scoped path with library file association", strings.Replace(first, `"rejections":[]`, `"episodeFileId":7,"rejections":[]`, 1), nil, Ambiguous, Held, pathOutOfScope},
		{"candidate equals directory", strings.Replace(first, "/job/episode-41.mkv", "/job", 1), nil, Ambiguous, Held, pathOutOfScope},
		{"native rejection", strings.Replace(first, `"rejections":[]`, `"rejections":[{"type":"permanent","reason":"Private sample path"}]`, 1), nil, NoUsableCandidate, Rejected, nativeRejected},
		{"already satisfied", strings.Replace(first, `"episodeFileId":0`, `"episodeFileId":5`, 1), func(j *Job) { j.Targets[0].Intent, j.Targets[0].FileID = policy.NeedSatisfied, 5 }, NoUsableCandidate, Held, targetNotNeeded},
		{"upgrade", strings.Replace(first, `"episodeFileId":0`, `"episodeFileId":5`, 1), func(j *Job) { j.Targets[0].Intent, j.Targets[0].FileID = policy.NeedUpgrade, 5 }, PartialCandidates, Eligible, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, job := sample(domain.ProductSonarr, 41, 42)
			if tc.edit != nil {
				tc.edit(&job)
			}
			client, _ := mockClient(t, cfg, job, 200, "application/json", "["+tc.rows+"]")
			got, err := client.Discover(context.Background(), job, fixtureTime)
			if err != nil || got.Assessment != tc.want || len(got.Candidates) == 0 || got.Candidates[0].State != tc.state || got.Candidates[0].Reason != tc.reason {
				t.Fatalf("preview = %#v, err = %v", got, err)
			}
		})
	}
}

func TestDiscoverAcceptsExactSingleFileOutput(t *testing.T) {
	for _, product := range []domain.Product{domain.ProductSonarr, domain.ProductRadarr} {
		t.Run(string(product), func(t *testing.T) {
			cfg, job := sample(product, 41)
			job.OutputPath = "/downloads/fixture.mkv"
			job.Queue.Records[0].OutputPath = job.OutputPath
			media := sonarrMedia
			if product == domain.ProductRadarr {
				media = radarrMedia
			}
			row := candidate(job.OutputPath, media, `"relativePath":"fixture.mkv"`, `"rejections":[]`)
			client, _ := mockClient(t, cfg, job, 200, "application/json", "["+row+"]")
			got, err := client.Discover(context.Background(), job, fixtureTime)
			if err != nil || got.Assessment != CompleteCandidates {
				t.Fatalf("preview = %s (%s), err = %v", got.Assessment, got.Reason, err)
			}
		})
	}
}

func TestEmptyPreviewDistinguishesSourceAccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		known  bool
		access bool
		want   Assessment
		reason domain.ReasonCode
	}{
		{"unknown", false, false, Uncertain, sourceUnknown},
		{"inaccessible", true, false, Uncertain, sourceInaccessible},
		{"accessible", true, true, NoUsableCandidate, noneUsable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, job := sample(domain.ProductRadarr, 41)
			if tc.known {
				job.SourceAccessible = domain.Fact[bool]{Observation: sampleEvidence(cfg.Instance, SourceAccessEvidence, job.DownloadID), Value: tc.access}
			}
			client, _ := mockClient(t, cfg, job, 200, "application/json", "[]")
			got, err := client.Discover(context.Background(), job, fixtureTime)
			if err != nil || got.Assessment != tc.want || got.Reason != tc.reason || got.Evidence.Complete != (tc.want != Uncertain) {
				t.Fatalf("preview = %s (%s), evidence = %s, err = %v", got.Assessment, got.Reason, got.Evidence.State, err)
			}
		})
	}
}

func TestDiscoverBoundsAndRejectsFailedResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		media  string
		body   string
		want   error
	}{
		{"redirect", 302, "text/plain", "", observe.ErrUnavailable},
		{"access denied", 403, "text/plain", "", observe.ErrAccess},
		{"missing route", 404, "text/plain", "", observe.ErrNotFound},
		{"rate limited", 429, "text/plain", "", observe.ErrRateLimited},
		{"wrong content type", 200, "text/html", "[]", observe.ErrResponse},
		{"invalid text", 200, "application/json", "[\"\xff\"]", observe.ErrResponse},
		{"null", 200, "application/json", "null", observe.ErrResponse},
		{"oversize body", 200, "application/json", strings.Repeat(" ", maxBody+1), observe.ErrResponse},
		{"too many candidates", 200, "application/json", "[" + strings.Repeat("{},", maxCandidates) + "{}]", observe.ErrPartial},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, job := sample(domain.ProductRadarr, 41)
			client, calls := mockClient(t, cfg, job, tc.status, tc.media, tc.body)
			got, err := client.Discover(context.Background(), job, fixtureTime)
			if !errors.Is(err, tc.want) || *calls != 1 || got.Assessment != Uncertain || got.Evidence.Complete {
				t.Fatalf("got %v and %d upstream calls", err, *calls)
			}
		})
	}
}

func TestPathWithinUsesArrPathRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		style  PathStyle
		root   string
		file   string
		within bool
	}{
		{"unix nested", UnixPath, "/downloads/job", "/downloads/job/nested/movie.mkv", true},
		{"unix case sensitive", UnixPath, "/downloads/job", "/downloads/JOB/movie.mkv", false},
		{"unix sibling", UnixPath, "/downloads/job", "/downloads/job2/movie.mkv", false},
		{"unix duplicate separator", UnixPath, "/downloads/job", "/downloads/job//movie.mkv", false},
		{"windows case and separator", WindowsPath, `D:\Downloads\Job`, `d:/downloads/JOB/movie.mkv`, true},
		{"windows drive", WindowsPath, `D:\Downloads\Job`, `C:\Downloads\Job\movie.mkv`, false},
		{"windows drive relative", WindowsPath, `D:\Downloads\Job`, `D:Downloads\Job\movie.mkv`, false},
		{"windows parent escape", WindowsPath, `D:\Downloads\Job`, `D:\Downloads\Job\..\movie.mkv`, false},
		{"windows device path", WindowsPath, `D:\Downloads\Job`, `\\?\D:\Downloads\Job\movie.mkv`, false},
		{"windows alternate stream", WindowsPath, `D:\Downloads\Job`, `D:\Downloads\Job\movie.mkv:stream`, false},
		{"unc scope", WindowsPath, `\\server\share\Job`, `//SERVER/SHARE/JOB/movie.mkv`, true},
		{"unc other share", WindowsPath, `\\server\share\Job`, `\\server\other\Job\movie.mkv`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if pathWithin(tc.style, tc.root, tc.file, false) != tc.within {
				t.Fatal("unexpected path scope")
			}
		})
	}
}
