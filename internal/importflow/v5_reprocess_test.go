package importflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

const v5FixtureMetadata = `"quality":{"quality":{"id":5,"name":"WEBDL-720p","source":"web","resolution":720},"revision":{"version":1,"real":0,"isRepack":false}},"languages":[{"id":1,"name":"English"}],"releaseGroup":"fixture","indexerFlags":256,"releaseType":"singleEpisode"`
const v5FixtureFolder = "Fixture.Series.S01E13.720p"
const v5FixturePath = "/downloads/" + v5FixtureFolder + "/" + v5FixtureFolder + ".mkv"

func TestSonarrV5ReprocessRequiresOneQualifiedScopedCandidate(t *testing.T) {
	for _, tt := range []struct {
		name      string
		alterJob  func(*Job)
		alterGET  func(string) string
		alterPOST func(string) string
		refuse    bool
		wantGet   int
		wantPost  int
		wantFile  bool
	}{
		{name: "qualified", wantGet: 1, wantPost: 1, wantFile: true},
		{name: "qualification declined", refuse: true, wantGet: 1},
		{name: "overlapping queue item", alterJob: func(j *Job) {
			alternative := j.Queue.Records[0]
			alternative.DownloadID = "another-job"
			j.Queue.Records = append(j.Queue.Records, alternative)
		}},
		{name: "missing complete coverage", alterJob: func(j *Job) { j.Queue.Evidence.Complete = false }},
		{name: "target already has a file", alterJob: func(j *Job) { j.Targets[0].FileID = 9 }},
		{name: "shape supported only for a single candidate", alterGET: func(s string) string {
			return "[" + s[1:len(s)-1] + "," + s[1:]
		}, wantGet: 1},
		{name: "target ID disagrees in GET", alterGET: func(s string) string {
			return strings.Replace(s, `"series":null`, `"series":{"id":99}`, 1)
		}, wantGet: 1},
		{name: "reprocess changed path", alterPOST: func(s string) string {
			return strings.Replace(s, v5FixturePath, "/downloads/unrelated.mkv", 1)
		}, wantGet: 1, wantPost: 1},
		{name: "reprocess returned an unwanted episode", alterPOST: func(s string) string {
			return strings.Replace(s, `"id":41`, `"id":42`, 1)
		}, wantGet: 1, wantPost: 1},
		{name: "reprocess returned a second episode", alterPOST: func(s string) string {
			return strings.Replace(s, `"episodes":[`, `"episodes":[{"id":42},`, 1)
		}, wantGet: 1, wantPost: 1},
		{name: "reprocess returned a changed download", alterPOST: func(s string) string {
			return strings.Replace(s, `"downloadId":"`, `"downloadId":"other-`, 1)
		}, wantGet: 1, wantPost: 1},
		{name: "reprocess returned a different season", alterPOST: func(s string) string {
			return strings.Replace(s, `"seasonNumber":1`, `"seasonNumber":2`, 1)
		}, wantGet: 1, wantPost: 1},
		{name: "native rejection remains", alterPOST: func(s string) string {
			return strings.Replace(s, `"rejections":[]`, `"rejections":[{"type":"permanent","reason":"Sample"}]`, 1)
		}, wantGet: 1, wantPost: 1},
		{name: "omitted rejections is not an approval", alterPOST: func(s string) string {
			return strings.Replace(s, `,"rejections":[]`, "", 1)
		}, wantGet: 1, wantPost: 1},
		{name: "omitted quality fails closed", alterPOST: func(s string) string {
			return strings.Replace(s, ","+v5FixtureMetadata, "", 1)
		}, wantGet: 1, wantPost: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, job := sample(domain.ProductSonarr, 41)
			cfg.Instance.UpstreamVersion, cfg.Instance.ConnectionRevision = arrwire.SonarrIDOnlyImportVersion, 7
			job.Ref.Generation = 1
			row := &job.Queue.Records[0]
			job.OutputPath = "/downloads/" + v5FixtureFolder
			row.ClientLabel, row.Protocol, row.OutputPath = "SAB fixture", domain.ProtocolUsenet, job.OutputPath
			row.TrackedState, row.Findings = "importBlocked", []observe.QueueFinding{observe.FindingSeriesIDOnly}
			row.MediaTitle, row.Episode = "Fixture Series", "S01E13"
			if tt.alterJob != nil {
				tt.alterJob(&job)
			}
			get, post := 0, 0
			client, err := New(cfg, fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "fixture.invalid" || r.URL.Path != "/prefix/api/v3/manualimport" ||
					r.Header.Get("X-Api-Key") != fixtureKey {
					t.Fatal("unexpected request")
				}
				switch r.Method {
				case http.MethodGet:
					get++
					if r.URL.Query().Get("downloadId") != job.DownloadID || r.URL.Query().Get("filterExistingFiles") != "false" {
						t.Fatal("GET must use exact download")
					}
					data := fmt.Sprintf(`[{"path":%q,"downloadId":%q,"folderName":%q,"size":4096,"series":null,"rejections":[{"type":"permanent","reason":"Unknown episode"}],%s}]`,
						v5FixturePath, job.DownloadID, v5FixtureFolder, v5FixtureMetadata)
					if tt.alterGET != nil {
						data = tt.alterGET(data)
					}
					return response(http.StatusOK, "application/json", data), nil
				case http.MethodPost:
					post++
					payload, err := io.ReadAll(r.Body)
					if err != nil || r.URL.RawQuery != "" || r.GetBody != nil || r.Header.Get("Content-Type") != "application/json" {
						t.Fatal("unexpected reprocess request options")
					}
					var req []sonarrV5ReprocessRequest
					if err := json.Unmarshal(payload, &req); err != nil || len(req) != 1 || req[0].Path != v5FixturePath ||
						req[0].DownloadID != job.DownloadID || req[0].SeasonNumber != 1 || req[0].SeriesID != 12 ||
						len(req[0].EpisodeIDs) != 1 || req[0].EpisodeIDs[0] != 41 || req[0].IndexerFlags != 256 ||
						req[0].Quality.Quality.ID != 5 || req[0].ReleaseType != ReleaseSingle || len(req[0].Languages) != 1 {
						t.Fatal("request must select one exact native file and episode")
					}
					data := fmt.Sprintf(`[{"path":%q,"downloadId":%q,"seriesId":12,"seasonNumber":1,"episodeIds":null,"episodes":[{"id":41,"seriesId":12,"seasonNumber":1,"episodeNumber":13,"episodeFileId":0}],"customFormats":[],"customFormatScore":0,"rejections":[],%s}]`,
						v5FixturePath, job.DownloadID, v5FixtureMetadata)
					if tt.alterPOST != nil {
						data = tt.alterPOST(data)
					}
					return response(http.StatusOK, "application/json; charset=utf-8", data), nil
				default:
					t.Fatal("unexpected method")
					return nil, nil
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			qualified := false
			prepared, err := client.ReprocessSonarrV5(t.Context(), job, fixtureTime, func(_ context.Context, s SonarrV5Source) error {
				qualified = true
				if s.Job != job.Ref || s.Target != row.Target || s.DownloadID != job.DownloadID ||
					s.OutputPath != job.OutputPath || s.Path != v5FixturePath || s.Size != 4096 ||
					s.Client != row.ClientLabel || s.Instance != cfg.Instance {
					t.Fatal("qualification did not get the exact selected source")
				}
				if tt.refuse {
					return ErrSonarrV5Qualification
				}
				return nil
			})
			if (err == nil) != tt.wantFile || get != tt.wantGet || post != tt.wantPost || qualified != (tt.wantPost != 0 || tt.refuse) {
				t.Fatalf("unexpected outcome (file=%t, got=%d/%d and qualified=%t): %v", len(prepared.Files) != 0, get, post, qualified, err)
			}
			if !tt.wantFile {
				if len(prepared.Files) != 0 {
					t.Fatal("invalid candidate prepared a write")
				}
				return
			}
			op := domain.Operation{Key: "fixture-op", Job: job.Ref, Action: domain.ActionImport, Targets: []domain.TargetRef{row.Target},
				Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: row.Target}, {Kind: domain.EffectSourceRetained}}}
			action := domain.PreparedAction{Product: cfg.Instance.Product, Version: cfg.Instance.UpstreamVersion,
				AdapterRevision: "fixture", ConnectionRevision: cfg.Instance.ConnectionRevision, OperationKey: op.Key, Import: &prepared}
			wire, err := arrwire.Encode(cfg.Instance, "fixture", op, action)
			if err != nil || wire.Method() != http.MethodPost || wire.Path() != "/api/v3/command" ||
				!strings.Contains(string(wire.Body()), `"importMode":"copy"`) || !strings.Contains(string(wire.Body()), `"episodeIds":[41]`) {
				t.Fatal("eligible reprocess did not yield one prepared Sonarr Copy")
			}
		})
	}
}

func TestSonarrV5ReprocessHasNoQualificationFallback(t *testing.T) {
	cfg, job := sample(domain.ProductSonarr, 41)
	cfg.Instance.UpstreamVersion = arrwire.SonarrIDOnlyImportVersion
	client, err := New(cfg, fixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("qualification absence reached transport")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReprocessSonarrV5(t.Context(), job, time.Now(), nil); err != ErrSonarrV5Qualification {
		t.Fatalf("expected qualification requirement, got %v", err)
	}
}

func TestSonarrV5InspectionReadsExactWindowsSource(t *testing.T) {
	cfg, job := sample(domain.ProductSonarr, 41)
	cfg.Instance.UpstreamVersion, cfg.Instance.ConnectionRevision = arrwire.SonarrIDOnlyImportVersion, 7
	job.Ref.Generation, job.PathStyle = 1, WindowsPath
	job.LibraryRoots = []string{`Z:\library`}
	const folder = `C:\downloads\` + v5FixtureFolder
	const file = folder + `\` + v5FixtureFolder + ".mkv"
	job.OutputPath = folder
	row := &job.Queue.Records[0]
	row.ClientLabel, row.Protocol, row.OutputPath = "SAB fixture", domain.ProtocolUsenet, job.OutputPath
	row.TrackedState, row.Findings = "importBlocked", []observe.QueueFinding{observe.FindingSeriesIDOnly}
	row.MediaTitle, row.Episode = "Fixture Series", "S01E13"
	size, calls := 4096, 0
	client, err := New(cfg, fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/prefix/api/v3/manualimport" ||
			r.URL.Query().Get("downloadId") != job.DownloadID || r.URL.Query().Get("filterExistingFiles") != "false" {
			t.Fatalf("unexpected read %s %s", r.Method, r.URL.Path)
		}
		name := fmt.Sprintf("%q", v5FixtureFolder)
		if job.OutputPath == file {
			name = "null"
		}
		return response(http.StatusOK, "application/json", fmt.Sprintf(
			`[{"path":%q,"downloadId":%q,"folderName":%s,"size":%d,"series":null,"rejections":[{"type":"permanent","reason":"Unknown episode"}],%s}]`,
			file, job.DownloadID, name, size, v5FixtureMetadata)), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	read := func() SonarrV5Source {
		t.Helper()
		result, err := client.InspectSonarrV5Import(t.Context(), job, fixtureTime)
		if err != nil || !result.Episode.Matched || result.Episode.Folder != v5FixtureFolder ||
			result.Source.Path != file || result.Source.OutputPath != job.OutputPath || result.Source.Size != int64(size) ||
			result.Source.QueueID != row.ID || result.Source.SelectionDigest == [32]byte{} ||
			len(result.Languages) != 1 || result.Languages[0].Name != "English" {
			t.Fatalf("inspection %v with %+v", err, result.Episode)
		}
		return result.Source
	}
	folderSource := read()
	job.OutputPath, row.OutputPath = file, file
	direct := read()
	if direct.SelectionDigest == folderSource.SelectionDigest {
		t.Fatal("changed queue path did not invalidate the approval")
	}
	size++
	if changed := read(); changed.SelectionDigest == direct.SelectionDigest || calls != 3 {
		t.Fatal("changed file size did not invalidate the approval")
	}
}
