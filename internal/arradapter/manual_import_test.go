package arradapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
)

func TestManualSonarrCopyReconcilesOnlyExactImportAndRetainedSource(t *testing.T) {
	const output = `C:\Downloads\Fixture`
	const path = output + `\fixture.mkv`
	const libraryPath = `D:\Library\Fixture\fixture.mkv`
	fixture := probeFixtureInstance()
	cfg := Config{Instance: fixture, AdapterRevision: ManualSonarrV5CopyRevision, SettingsFingerprint: [32]byte{1}, APIKey: "fixture-key"}
	target := domain.TargetRef{Instance: fixture.Ref, Kind: domain.TargetEpisode, ID: 31, SeriesID: 17}
	source := importflow.SonarrV5Source{Instance: fixture, Job: domain.JobRef{Instance: fixture.Ref, ID: "job", Generation: 1},
		Target: target, DownloadID: "download-a", Client: "LAB SAB", OutputPath: output, Path: path, Size: 4096,
		QueueID: 19, SelectionDigest: [32]byte{1}}
	op := domain.Operation{Key: "copy-fixture", Job: source.Job, Targets: []domain.TargetRef{target}, Action: domain.ActionImport,
		Effects: []domain.Effect{{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained}}}
	prepared := domain.PreparedAction{Product: fixture.Product, Version: arrwire.SonarrIDOnlyImportVersion,
		AdapterRevision: ManualSonarrV5CopyRevision, ConnectionRevision: fixture.ConnectionRevision, OperationKey: op.Key,
		Import: &domain.PreparedImport{DownloadID: source.DownloadID, Files: []domain.PreparedImportFile{{
			Path: path, FolderName: "fixture", Targets: []domain.TargetRef{target}, QualityID: 3, QualityName: "WEBDL-1080p", QualitySource: "web",
			QualityResolution: new(int64(1080)), IndexerFlags: new(uint64), ReleaseType: "singleEpisode",
			Quality: domain.ImportQualityRevision{Version: 1, Observed: true}, Languages: []domain.ImportLanguage{{ID: 1, Name: "English"}},
		}}}}
	now := time.Date(2026, 11, 14, 10, 10, 10, 0, time.UTC)
	const imported = "2026-11-14T10:10:00Z"
	const emptyPage = `{"page":1,"pageSize":50,"totalRecords":0,"records":[]}`
	for _, test := range []struct {
		name           string
		command        string
		library        bool
		retained       bool
		correctHistory bool
		verification   domain.Verification
		cleanup        bool
		libraryOnDisk  bool
		overlap        bool
		readError      bool
	}{
		{"verified", `"status":"completed","result":"successful"`, true, true, true, domain.VerificationEffect, false, true, false, false},
		{"source missing", `"status":"completed","result":"successful"`, true, false, true, domain.VerificationPartial, false, true, false, false},
		{"failed before effect", `"status":"aborted","result":"unsuccessful"`, false, true, false, domain.VerificationNoEffect, false, false, false, false},
		{"failed but linked", `"status":"failed","result":"unsuccessful"`, true, true, true, domain.VerificationUnresolved, false, true, false, false},
		{"foreign source", `"status":"completed","result":"successful"`, true, true, false, "", false, true, false, true},
		{"cleanup removes source", `"status":"completed","result":"successful"`, true, false, true, domain.VerificationEffect, true, true, false, false},
		{"cleanup library file missing", `"status":"completed","result":"successful"`, true, false, true, domain.VerificationPartial, true, false, false, false},
		{"cleanup overlaps another job", "", false, true, false, "", true, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var writes int
			cfg, op, prepared := cfg, op, prepared
			if test.cleanup {
				cfg.AdapterRevision = arrwire.SonarrV5CleanupCopyRevision
				prepared.AdapterRevision = cfg.AdapterRevision
				selection := *prepared.Import
				selection.SonarrCleanup = true
				prepared.Import = &selection
				op.Effects = op.Effects[:1]
			}
			roundTrip := integrationTransport(func(req *http.Request) (*http.Response, error) {
				route := strings.TrimPrefix(req.URL.Path, "/sonarr")
				if req.Header.Get("X-Api-Key") != cfg.APIKey {
					t.Fatal("missing fixture authentication")
				}
				switch req.Method + " " + route {
				case "GET /api/v3/manualimport":
					if req.URL.Query().Get("folder") != path || req.URL.Query().Get("filterExistingFiles") != "false" {
						t.Fatal("read did not select the exact source file")
					}
					if writes != 0 && !test.retained {
						return packResponse(http.StatusOK, `[]`), nil
					}
					return packResponse(http.StatusOK, fmt.Sprintf(`[{"path":%q,"size":4096}]`, path)), nil
				case "GET /api/v3/queue":
					rows := fmt.Sprintf(`{"id":19,"seriesId":17,"episodeId":31,"protocol":"usenet","downloadId":"download-a","downloadClient":"LAB SAB","outputPath":%q,"status":"completed","trackedDownloadState":"importBlocked","statusMessages":[{"messages":["Found matching series via grab history, but release was matched to series by ID. Automatic import is not possible."]}]}`, output)
					count := 1
					if test.overlap {
						rows += fmt.Sprintf(`,{"id":20,"seriesId":17,"episodeId":32,"protocol":"usenet","downloadId":"other","outputPath":%q,"status":"downloading"}`, `c:\downloads\fixture\Nested`)
						count++
					}
					return packResponse(http.StatusOK, fmt.Sprintf(`{"page":1,"pageSize":100,"totalRecords":%d,"records":[%s]}`, count, rows)), nil
				case "GET /api/v3/filesystem/type":
					if !test.cleanup || req.URL.Query().Get("path") != libraryPath {
						t.Fatal("unexpected or unbound library disk read")
					}
					if test.libraryOnDisk {
						return packResponse(http.StatusOK, `{"type":"file"}`), nil
					}
					return packResponse(http.StatusOK, `{"type":"folder"}`), nil
				case "GET /api/v3/episode/31":
					if writes != 0 && test.library {
						return packResponse(http.StatusOK, `{"id":31,"seriesId":17,"episodeFileId":42,"hasFile":true}`), nil
					}
					return packResponse(http.StatusOK, `{"id":31,"seriesId":17,"episodeFileId":0,"hasFile":false}`), nil
				case "POST /api/v3/command":
					writes++
					return packResponse(http.StatusCreated, `{"id":8,"name":"ManualImport","status":"queued"}`), nil
				case "GET /api/v3/command/8":
					return packResponse(http.StatusOK, `{"id":8,"name":"ManualImport",`+test.command+`}`), nil
				case "GET /api/v3/history":
					if req.URL.Query().Get("episodeId") != "31" || req.URL.Query().Get("seriesIds") != "17" {
						t.Fatal("history read escaped scoped episode")
					}
					if !test.library {
						return packResponse(http.StatusOK, emptyPage), nil
					}
					dropped := path
					if !test.correctHistory {
						dropped += ".foreign"
					}
					body := fmt.Sprintf(`{"page":1,"pageSize":50,"totalRecords":1,"records":[{"id":32,"seriesId":17,"episodeId":31,"downloadId":"download-a","eventType":"downloadFolderImported","date":%q,"data":{"fileId":"42","droppedPath":%q,"importedPath":%q}}]}`, imported, dropped, libraryPath)
					return packResponse(http.StatusOK, body), nil
				case "GET /api/v3/episodefile/42":
					return packResponse(http.StatusOK, fmt.Sprintf(`{"id":42,"seriesId":17,"path":%q,"size":4096,"dateAdded":%q}`, libraryPath, imported)), nil
				default:
					t.Fatalf("unexpected request %s %s", req.Method, route)
					return nil, ErrConfiguration
				}
			})
			qualify := func(_ context.Context, binding Binding) (Qualification, error) {
				return Qualification{Binding: binding, Verified: true, Evidence: "fixture-copy", ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, nil
			}
			adapter, err := NewManualSonarrV5Copy(cfg, source, roundTrip, qualify, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.DispatchManualImport(t.Context(), op, prepared, func(context.Context) error {
				return errors.New("fixture approval withdrawn")
			}); !errors.Is(err, ErrQualification) || writes != 0 {
				t.Fatalf("final approval fence permitted a send: %d writes, %v", writes, err)
			}
			ack, err := adapter.DispatchManualImport(t.Context(), op, prepared, func(context.Context) error { return nil })
			if test.overlap {
				if !errors.Is(err, ErrQualification) || writes != 0 {
					t.Fatalf("overlap reached dispatch: %d writes, %v", writes, err)
				}
				return
			}
			if err != nil || ack.UpstreamReference != "8" {
				t.Fatalf("dispatch: %+v %v", ack, err)
			}
			outcome, err := adapter.Reconcile(t.Context(), domain.Attempt{ID: "manual-fixture", Operation: op,
				Prepared: &prepared, SentAt: now.Add(-time.Minute), Acknowledgement: ack})
			if writes != 1 || test.readError != errors.Is(err, ErrReadback) ||
				!test.readError && (err != nil || outcome.Verification != test.verification) {
				t.Fatalf("readback: writes=%d, outcome=%+v, error=%v", writes, outcome, err)
			}
		})
	}
}

func TestManualImportProfileFencesMediaManagementDrift(t *testing.T) {
	const route = "/api/v3/config/mediamanagement"
	const settings = `{"copyUsingHardlinks":false,"importExtraFiles":false,"useScriptImport":false,"enableMediaInfo":true}`
	instance := probeFixtureInstance()
	base, _ := packProfileFixture(t, map[string]string{route: settings})
	profile, err := ProbeManualImportProfile(t.Context(), instance, "private-sonarr-key", base)
	if err != nil || profile.Fingerprint == ([32]byte{}) || profile.Fingerprint == profile.EmptyPackProfile.Fingerprint {
		t.Fatalf("missing separate import profile: %v", err)
	}
	for _, setting := range []string{"copyUsingHardlinks", "importExtraFiles", "useScriptImport", "enableMediaInfo"} {
		t.Run(setting, func(t *testing.T) {
			value := "false"
			other := "true"
			if setting == "enableMediaInfo" {
				value, other = other, value
			}
			settings := strings.Replace(settings, fmt.Sprintf(`%q:%s`, setting, value), fmt.Sprintf(`%q:%s`, setting, other), 1)
			transport, _ := packProfileFixture(t, map[string]string{route: settings})
			changed, err := ProbeManualImportProfile(t.Context(), instance, "private-sonarr-key", transport)
			if err != nil || changed.Fingerprint == profile.Fingerprint {
				t.Fatalf("setting change did not invalidate the fingerprint: %v", err)
			}
		})
	}
}
