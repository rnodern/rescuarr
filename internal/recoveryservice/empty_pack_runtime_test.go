package recoveryservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/arrwire"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestRunEmptyPackRecoveryDispatchesThroughQualifiedAdapter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	local, err := store.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if err := local.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE app_state SET mode = 'autonomous' WHERE singleton = 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	fixture := &emptyPackRuntimeFixture{now: clock.At}
	transport := fixtureTransport(fixture.roundTrip)
	observations := observeservice.New(local, clock, transport)
	instance, err := observations.AddInstance(ctx, observeservice.InstanceInput{Product: domain.ProductSonarr,
		DisplayName: "Fixture Sonarr", BaseURL: "https://sonarr.invalid", APIKey: "fixture-key", Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := observations.Collect(ctx, instance.Ref.ID); err != nil {
		t.Fatal(err)
	}
	cases, err := observations.AutomaticEmptyPackCasesFor(ctx, instance.Ref.ID, "", 1)
	if err != nil || len(cases) != 1 {
		t.Fatalf("eligible case: got %d, error %v", len(cases), err)
	}
	key := EmptyPackKey{CaseID: cases[0].ID, Job: cases[0].Job.Ref}
	journal := NewEmptyPackJournal(local)
	factory := func(current domain.Instance) (*arradapter.EmptyPackAdapter, error) {
		key, err := local.ReadSecret(current.SecretRef)
		if err != nil {
			return nil, err
		}
		return arradapter.NewEmptyPackAdapter(arradapter.Config{Instance: current,
			APIKey: key, AdapterRevision: "fixture-pack-v1", SettingsFingerprint: sha256.Sum256([]byte("fixture settings"))},
			transport.RoundTrip, func(_ context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
				return arradapter.Qualification{Binding: binding, Verified: true, Evidence: "fixture-only",
					ObservedAt: clock.At.Add(-time.Second), ExpiresAt: clock.At.Add(time.Minute)}, nil
			}, clock.Now)
	}

	workerContext, stopWorker := context.WithCancel(ctx)
	done := make(chan struct{})
	var workerErr error
	failures := make(chan error, 1)
	go func() {
		defer close(done)
		workerErr = RunEmptyPackRecovery(workerContext, observations, local, clock, instance.Ref, transport, factory,
			func(err error) {
				select {
				case failures <- err:
				default:
				}
			})
	}()
	defer func() {
		stopWorker()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker failed to stop after cancellation")
		}
	}()

	ticks := time.NewTicker(20 * time.Millisecond)
	defer ticks.Stop()
	for {
		select {
		case failure := <-failures:
			t.Fatalf("automatic recovery failed: %v", failure)
		case <-done:
			t.Fatalf("worker stopped early: %v", workerErr)
		case <-ctx.Done():
			t.Fatal("automatic recovery did not complete before the deadline")
		case <-ticks.C:
			record, found, err := journal.Load(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if !found || record.Stage != EmptyPackDone {
				continue
			}
			stopWorker()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("worker failed to stop after cancellation")
			}
			if workerErr != nil || fixture.removals != 1 || fixture.searches != 1 ||
				record.CommandID != 81 || !slices.Equal(record.SearchIDs, []int64{12}) {
				t.Fatalf("got %+v, %d removals, %d searches, worker error %v",
					record, fixture.removals, fixture.searches, workerErr)
			}
			return
		}
	}
}

// The fixture advances its Sonarr state only when the worker sends a correctly
// scoped request. The test reads the resulting durable checkpoint independently.
type emptyPackRuntimeFixture struct {
	now                time.Time
	removals, searches int
}

func (f *emptyPackRuntimeFixture) roundTrip(req *http.Request) (*http.Response, error) {
	const release = "Fixture.Show.S01"
	const downloadID = "fixture-original"
	const indexer = "fixture-indexer"
	if req.URL.Host != "sonarr.invalid" || req.Header.Get("X-Api-Key") != "fixture-key" {
		return nil, fmt.Errorf("request escaped the Sonarr fixture")
	}
	method, path := req.Method, req.URL.Path
	status, payload := http.StatusOK, any(nil)
	switch method + " " + path {
	case "GET /api/v3/system/status":
		payload = map[string]any{"appName": "Sonarr", "version": arrwire.SonarrIDOnlyImportVersion, "isWindows": true}
	case "GET /api/v3/queue":
		rows := []map[string]any{}
		if f.removals == 0 {
			for _, id := range []int{11, 12} {
				rows = append(rows, map[string]any{"id": id + 10, "seriesId": 5, "episodeId": id,
					"title": release, "downloadId": downloadID, "downloadClient": "Fixture SAB",
					"protocol": "usenet", "status": "completed", "trackedDownloadState": "importPending",
					"outputPath": `C:\fixture\original`, "statusMessages": []map[string][]string{
						{"messages": {`No files found are eligible for import in C:\fixture\original`}},
					}})
			}
		}
		payload = runtimeFixturePage(rows, 100)
	case "GET /api/v3/series/5":
		payload = map[string]any{"id": 5, "monitored": true, "runtime": 22}
	case "GET /api/v3/episode":
		rows := []map[string]any{}
		for _, id := range req.URL.Query()["episodeIds"] {
			switch id {
			case "11":
				rows = append(rows, map[string]any{"id": 11, "seriesId": 5, "monitored": true, "hasFile": true, "episodeFileId": 91})
			case "12":
				rows = append(rows, map[string]any{"id": 12, "seriesId": 5, "monitored": true, "hasFile": false, "episodeFileId": 0,
					"airDateUtc": f.now.Add(-24 * time.Hour)})
			}
		}
		payload = rows
	case "GET /api/v3/manualimport":
		payload = []any{}
	case "GET /api/v3/history":
		rows := []map[string]any{}
		for _, id := range []int{11, 12} {
			rows = append(rows, map[string]any{"id": id, "seriesId": 5, "episodeId": id, "downloadId": downloadID,
				"eventType": "grabbed", "date": f.now.Add(-time.Hour), "sourceTitle": release,
				"data": map[string]any{"indexer": indexer}})
			if f.removals == 1 {
				rows = append(rows, map[string]any{"id": id + 100, "seriesId": 5, "episodeId": id, "downloadId": downloadID,
					"eventType": "downloadFailed", "date": f.now, "sourceTitle": release})
			}
		}
		payload = runtimeFixturePage(rows, 400)
	case "GET /api/v3/blocklist":
		rows := []map[string]any{}
		if f.removals == 1 {
			rows = append(rows, map[string]any{"id": 1, "seriesId": 5, "episodeIds": []int{11, 12},
				"sourceTitle": release, "indexer": indexer, "protocol": "usenet", "date": f.now})
		}
		payload = runtimeFixturePage(rows, 400)
	case "DELETE /api/v3/queue/21":
		if f.removals != 0 || req.URL.RawQuery != "blocklist=true&changeCategory=false&removeFromClient=false&skipRedownload=true" {
			return nil, fmt.Errorf("unsafe or repeated removal")
		}
		f.removals++
	case "POST /api/v3/command":
		body, err := io.ReadAll(req.Body)
		if f.removals != 1 || f.searches != 0 || err != nil || string(body) != `{"name":"EpisodeSearch","episodeIds":[12]}` {
			return nil, fmt.Errorf("unexpected search: %s, %v", body, err)
		}
		f.searches++
		status = http.StatusCreated
		payload = map[string]any{"id": 81, "name": "EpisodeSearch", "status": "queued",
			"body": map[string]any{"episodeIds": []int{12}}}
	default:
		return nil, fmt.Errorf("unexpected fixture route %s %s", method, path)
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
}

func runtimeFixturePage(rows []map[string]any, size int) map[string]any {
	return map[string]any{"page": 1, "pageSize": size, "totalRecords": len(rows), "records": rows}
}
