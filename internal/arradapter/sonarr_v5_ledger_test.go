package arradapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/importflow"
	"github.com/rnodern/rescuarr/internal/ledger"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/policy"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestSonarrV5AutomaticReprocessFencesCopyAndReadsBack(t *testing.T) {
	const (
		folder   = "Fixture.Series.S01E13.1080p"
		output   = "/downloads/" + folder
		path     = output + "/" + folder + ".mkv"
		metadata = `"quality":{"quality":{"id":3,"name":"WEBDL-1080p","source":"web","resolution":1080},"revision":{"version":1,"real":0,"isRepack":false}},"languages":[{"id":1,"name":"English"}],"releaseGroup":"fixture","indexerFlags":0,"releaseType":"singleEpisode"`
	)
	ctx := t.Context()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	journal, err := ledger.NewJournal(local, func(context.Context, *sql.Tx, ledger.Approval, time.Time) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE app_state SET mode = 'autonomous' WHERE singleton = 1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ready, err := journal.CanPlanAutomaticImport(ctx); err != nil || !ready {
		t.Fatalf("cannot plan autonomous Copy: %v %v", ready, err)
	}

	fixtureTime := time.Date(2026, 1, 21, 10, 11, 12, 0, time.UTC)
	instance := domain.Instance{Ref: domain.InstanceRef{ID: "fixture-sonarr-v5", Epoch: 2}, Product: domain.ProductSonarr,
		BaseURL: "https://arr.example.invalid", UpstreamVersion: arrwire.SonarrIDOnlyImportVersion, ConnectionRevision: 3}
	cfg := Config{Instance: instance, AdapterRevision: "fixture-v5-adapter", APIKey: "synthetic-key", SettingsFingerprint: [32]byte{1}}
	target := domain.TargetRef{Instance: instance.Ref, Kind: domain.TargetEpisode, ID: 31, SeriesID: 17}
	operation := domain.Operation{Key: "fixture-v5-operation", Job: domain.JobRef{Instance: instance.Ref, ID: "fixture-v5-job", Generation: 1},
		Targets: []domain.TargetRef{target}, Action: domain.ActionImport, Effects: []domain.Effect{
			{Kind: domain.EffectLibraryImport, Target: target}, {Kind: domain.EffectSourceRetained},
		}}
	prepared := domain.PreparedAction{Product: instance.Product, Version: instance.UpstreamVersion,
		AdapterRevision: cfg.AdapterRevision, ConnectionRevision: instance.ConnectionRevision, OperationKey: operation.Key}
	downloadID := "fixture-sonarr-v5-download"
	observation := domain.Observation{
		ID: "fixture-queue", Scope: domain.Scope{Kind: domain.ScopeInstance, Instance: cfg.Instance.Ref},
		Source: domain.Source(domain.ProductSonarr), Kind: "queue", State: domain.ObservationKnown, Complete: true,
		SnapshotGeneration: 2, ObservedAt: fixtureTime.Add(-time.Second), ExpiresAt: fixtureTime.Add(time.Minute),
	}
	profile := observation
	profile.ID, profile.Kind, profile.SourceRecord = "fixture-profile", importflow.TargetProfileEvidence, "31"
	job := importflow.Job{Ref: operation.Job, DownloadID: downloadID, OutputPath: output, PathStyle: importflow.UnixPath,
		LibraryRoots: []string{"/library"}, TrackingComplete: true,
		Targets: []importflow.Target{{Ref: target, ProfileID: 2, ProfileRevision: 3,
			Monitored: true, Intent: policy.NeedFirstImport, Evidence: profile}},
		Queue: observe.QueueSnapshot{Evidence: observation, Records: []observe.QueueRecord{{
			ID: 19, Target: target, DownloadID: downloadID, OutputPath: output, ClientLabel: "SAB fixture",
			Protocol: domain.ProtocolUsenet, Status: "completed", TrackedState: "importBlocked",
			Findings: []observe.QueueFinding{observe.FindingSeriesIDOnly}, MediaTitle: "Fixture Series", Episode: "S01E13",
		}}},
	}

	var intentID domain.IntentID
	gets, reprocesses, copies := 0, 0, 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fail := func(message string) {
			t.Error(message)
			http.Error(w, "unexpected fixture request", http.StatusBadRequest)
		}
		if req.Header.Get("X-Api-Key") != cfg.APIKey {
			fail("incorrect API key")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method + " " + req.URL.Path {
		case "GET /api/v3/manualimport":
			gets++
			if req.URL.Query().Get("downloadId") != downloadID || req.URL.Query().Get("filterExistingFiles") != "false" {
				fail("GET did not select the queued download")
				return
			}
			fmt.Fprintf(w, `[{"path":%q,"downloadId":%q,"folderName":%q,"size":4096,"rejections":[{"type":"permanent","reason":"Unknown episode"}],%s}]`, path, downloadID, folder, metadata)
		case "POST /api/v3/manualimport":
			reprocesses++
			var inputs []struct {
				Path         string  `json:"path"`
				DownloadID   string  `json:"downloadId"`
				SeriesID     int64   `json:"seriesId"`
				SeasonNumber int     `json:"seasonNumber"`
				EpisodeIDs   []int64 `json:"episodeIds"`
			}
			if err := json.NewDecoder(req.Body).Decode(&inputs); err != nil || len(inputs) != 1 ||
				inputs[0].Path != path || inputs[0].DownloadID != downloadID || inputs[0].SeriesID != target.SeriesID ||
				inputs[0].SeasonNumber != 1 || len(inputs[0].EpisodeIDs) != 1 || inputs[0].EpisodeIDs[0] != target.ID ||
				intentID != "" || copies != 0 {
				fail("reprocess did not select exactly one native file and episode before journal admission")
				return
			}
			fmt.Fprintf(w, `[{"path":%q,"downloadId":%q,"seriesId":%d,"seasonNumber":1,"episodeIds":null,"episodes":[{"id":%d,"seriesId":%d,"seasonNumber":1,"episodeNumber":13,"episodeFileId":0}],"customFormats":[],"customFormatScore":0,"rejections":[],%s}]`, path, downloadID, target.SeriesID, target.ID, target.SeriesID, metadata)
		case "POST /api/v3/command":
			copies++
			var state, preflight string
			var markers int
			if err := local.Read(req.Context(), func(tx *sql.Tx) error {
				if err := tx.QueryRowContext(req.Context(), `SELECT i.state, a.preflight FROM action_intents AS i
					JOIN action_attempts AS a ON a.intent_id = i.id WHERE i.id = ?`, intentID).Scan(&state, &preflight); err != nil {
					return err
				}
				return tx.QueryRowContext(req.Context(), `SELECT count(*) FROM audit_events
					WHERE intent_id = ? AND kind = 'action_sending_recorded'`, intentID).Scan(&markers)
			}); err != nil || state != string(domain.IntentSending) || !strings.Contains(preflight, path) || markers != 1 {
				fail("Copy arrived before the exact private selector and send marker were committed")
				return
			}
			payload, err := io.ReadAll(req.Body)
			if err != nil || gets != 1 || reprocesses != 1 || copies != 1 ||
				!strings.Contains(string(payload), `"name":"ManualImport","importMode":"copy"`) ||
				!strings.Contains(string(payload), fmt.Sprintf(`"episodeIds":[%d]`, target.ID)) ||
				!strings.Contains(string(payload), fmt.Sprintf(`"path":%q`, path)) {
				fail("expected one Copy of the independently selected file and episode")
				return
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":402,"name":"ManualImport","status":"queued"}`)
		default:
			fail("unexpected upstream fixture request")
		}
	})
	transport := integrationTransport(func(req *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Result(), nil
	})

	client, err := importflow.New(observe.Config{Instance: cfg.Instance, APIKey: cfg.APIKey}, transport)
	if err != nil {
		t.Fatal(err)
	}
	sourceQualified := false
	selector, err := client.ReprocessSonarrV5(ctx, job, fixtureTime, func(_ context.Context, source importflow.SonarrV5Source) error {
		sourceQualified = source.Job == job.Ref && source.DownloadID == downloadID && source.Path == path &&
			source.Target == target && source.Size == 4096 && source.Client == "SAB fixture"
		if !sourceQualified {
			return importflow.ErrSonarrV5Qualification
		}
		return nil
	})
	if err != nil || !sourceQualified {
		t.Fatalf("qualified source could not be reprocessed: %v", err)
	}
	prepared.Import = &selector
	proposal := domain.Proposal{
		ID: "fixture-sonarr-v5-import", CaseID: "fixture-sonarr-v5-case", CaseRevision: 5, PolicyRevision: 3, CapabilityRevision: 2,
		DecisionHash: "fixture-sonarr-v5-decision", Operation: operation,
		Guards: []domain.Guard{{Reason: "fixture_qualified_source", Passed: true}}, ExpiresAt: fixtureTime.Add(time.Minute),
	}
	intent, err := journal.AuthorizeAutomaticImport(ctx, ledger.Approval{Proposal: proposal,
		PhysicalJob: "fixture-sonarr-v5-download", Client: "fixture-sab"}, fixtureTime)
	if err != nil {
		t.Fatal(err)
	}
	intentID = intent.ID
	gate := integrationGate(func(_ context.Context, current domain.Intent) (ledger.Preflight, error) {
		p := current.Proposal
		return ledger.Preflight{DecisionHash: p.DecisionHash, CaseRevision: p.CaseRevision, PolicyRevision: p.PolicyRevision,
			CapabilityRevision: p.CapabilityRevision, Evidence: []domain.EvidenceID{"fixture-fresh-source"}, Prepared: &prepared,
			ObservedAt: fixtureTime, ExpiresAt: fixtureTime.Add(15 * time.Second)}, nil
	})
	now := fixtureTime
	var expectedAttempt domain.AttemptID
	readbacks := 0
	adapter, err := newAdapter(cfg, transport.RoundTrip, func(_ context.Context, binding Binding) (Qualification, error) {
		return Qualification{Binding: binding, Verified: true, Evidence: "fixture-v5-qualified-settings",
			ObservedAt: fixtureTime.Add(-time.Second), ExpiresAt: fixtureTime.Add(time.Minute)}, nil
	}, func(_ context.Context, route Route, attempt domain.Attempt) (domain.Outcome, error) {
		readbacks++
		if route != RouteCopyImport || attempt.ID != expectedAttempt || attempt.Prepared == nil ||
			attempt.Prepared.Import.Files[0].Path != path || attempt.Acknowledgement.UpstreamReference != "402" {
			t.Fatal("readback did not receive the durable scoped attempt and native command reference")
		}
		return domain.Outcome{AttemptID: attempt.ID, ObservedAt: now, Effects: []domain.EffectResult{
			{Effect: operation.Effects[0], Verification: domain.VerificationEffect,
				Evidence: []domain.EvidenceID{"fixture-library-file", "fixture-native-history"}},
			{Effect: operation.Effects[1], Verification: domain.VerificationEffect, Evidence: []domain.EvidenceID{"fixture-source-preserved"}},
		}}, nil
	}, func() time.Time { return now }, RouteCopyImport)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := ledger.NewExecutor(journal, adapter, gate, integrationClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	expectedAttempt, err = executor.Dispatch(ctx, intent.ID)
	if err != nil || expectedAttempt == "" {
		t.Fatalf("qualified Copy failed to dispatch: %q %v", expectedAttempt, err)
	}
	if ready, err := journal.CanPlanAutomaticImport(ctx); err != nil || ready {
		t.Fatalf("native acknowledgement must not release the automatic slot: %v %v", ready, err)
	}
	now = fixtureTime.Add(2 * time.Second)
	outcome, err := executor.Reconcile(ctx, expectedAttempt)
	if err != nil || outcome.Verification != domain.VerificationEffect || readbacks != 1 {
		t.Fatalf("independent readback did not verify effects: %+v %v", outcome, err)
	}
	if ready, err := journal.CanPlanAutomaticImport(ctx); err != nil || !ready {
		t.Fatalf("verified readback must release the slot: %v %v", ready, err)
	}
	if _, err := executor.Dispatch(ctx, intent.ID); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("same intent attempted a second Copy: %v", err)
	}
	if gets != 1 || reprocesses != 1 || copies != 1 {
		t.Fatalf("unexpected transport calls: %d GETs, %d reprocesses, %d Copies", gets, reprocesses, copies)
	}
}

type integrationClock func() time.Time

func (f integrationClock) Now() time.Time { return f() }

type integrationGate func(context.Context, domain.Intent) (ledger.Preflight, error)

func (f integrationGate) Check(ctx context.Context, intent domain.Intent) (ledger.Preflight, error) {
	return f(ctx, intent)
}

type integrationTransport func(*http.Request) (*http.Response, error)

func (f integrationTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
