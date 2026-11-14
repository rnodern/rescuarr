package recoveryservice

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

type packEffectsFixture struct {
	t           *testing.T
	queue       bool
	verified    bool
	removeError error
	searchError error
	preflights  int
	rejects     int
	searches    [][]int64
}

func (f *packEffectsFixture) Preflight(_ context.Context, scope arradapter.EmptyPackScope) error {
	f.preflights++
	if scope.DownloadID != "fixture-original" || scope.SeriesID != 11 || len(scope.EpisodeIDs) != 26 || len(scope.QueueIDs) != 26 {
		f.t.Fatalf("preflight lost pack scope: %+v", scope)
	}
	return nil
}

func (f *packEffectsFixture) Reject(context.Context, arradapter.EmptyPackScope) error {
	f.rejects++
	f.queue = false
	return f.removeError
}

func (f *packEffectsFixture) VerifyRejected(_ context.Context, scope arradapter.EmptyPackScope, started time.Time) (bool, error) {
	if started.IsZero() || len(scope.EpisodeIDs) != 26 {
		f.t.Fatalf("checkpoint lost proof for readback")
	}
	return f.verified && !f.queue, nil
}

func (f *packEffectsFixture) Search(_ context.Context, _ arradapter.EmptyPackScope, ids []int64) (int64, error) {
	f.searches = append(f.searches, slices.Clone(ids))
	return 97, f.searchError
}

func TestEmptyPackWorkerSearchesOnlyTheEpisodeStillWantedAfterRemoval(t *testing.T) {
	for _, test := range []struct {
		name        string
		alternative bool
		lostSearch  bool
	}{
		{name: "only episode 26"},
		{name: "alternate download covers episode 26", alternative: true},
		{name: "uncertain search never resent", lostSearch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
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
			effects := &packEffectsFixture{t: t, queue: true, removeError: arradapter.ErrUncertain}
			if test.lostSearch {
				effects.searchError = arradapter.ErrUncertain
			}
			transport := fixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != "fixture-key" {
					t.Fatal("read-only fixture received an unexpected request")
				}
				var body any
				switch r.URL.Path {
				case "/api/v3/system/status":
					body = map[string]any{"appName": "Sonarr", "version": "5.0.0.2941", "isWindows": true}
				case "/api/v3/queue":
					rows := make([]map[string]any, 0, 26)
					if effects.queue {
						for id := 1; id <= 26; id++ {
							rows = append(rows, map[string]any{"id": 100 + id, "seriesId": 11, "episodeId": id,
								"downloadId": "fixture-original", "downloadClient": "Fixture SAB", "protocol": "usenet",
								"title": "Fixture.Show.S01", "status": "completed", "trackedDownloadState": "importPending",
								"statusMessages": []map[string]any{{"messages": []string{"No files found are eligible for import"}}}})
						}
					} else if test.alternative {
						rows = append(rows, map[string]any{"id": 200, "seriesId": 11, "episodeId": 26,
							"downloadId": "fixture-alternate", "status": "downloading"})
					}
					body = map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(rows), "records": rows}
				case "/api/v3/series/11":
					body = map[string]any{"id": 11, "monitored": true, "runtime": 11}
				case "/api/v3/episode":
					rows := make([]map[string]any, 0, 26)
					for id := 1; id <= 26; id++ {
						if r.URL.Query()["episodeIds"][id-1] != fmt.Sprint(id) {
							t.Fatal("episode lookup lost pack scope")
						}
						fileID := 200 + id
						if id == 26 || (id == 25 && effects.queue) {
							fileID = 0
						}
						rows = append(rows, map[string]any{"id": id, "seriesId": 11, "monitored": true,
							"episodeFileId": fileID, "hasFile": fileID != 0, "airDateUtc": "2025-01-01T00:00:00Z"})
					}
					body = rows
				default:
					t.Fatalf("unexpected fixture endpoint %s", r.URL.Path)
				}
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})
			observations := observeservice.New(local, clock, transport)
			instance, err := observations.AddInstance(ctx, observeservice.InstanceInput{Product: domain.ProductSonarr,
				DisplayName: "Fixture", BaseURL: "http://sonarr.invalid", APIKey: "fixture-key", Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			if err := observations.Collect(ctx, instance.Ref.ID); err != nil {
				t.Fatal(err)
			}
			items, err := observations.AutomaticEmptyPackCases(ctx, "", 1)
			if err != nil || len(items) != 1 {
				t.Fatalf("expected one eligible case: %d %v", len(items), err)
			}
			key := EmptyPackKey{CaseID: items[0].ID, Job: items[0].Job.Ref}
			factory := func(inst domain.Instance) (emptyPackEffects, emptyPackReader, error) {
				reader, err := observe.NewArr(observe.Config{Instance: inst, APIKey: "fixture-key"}, transport)
				return effects, reader, err
			}
			worker := &emptyPackWorker{cases: observations, journal: NewEmptyPackJournal(local), clock: clock, connection: factory}
			if err := local.Write(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE action_pause SET paused = 1 WHERE singleton = 1`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := worker.step(ctx); err != nil || effects.preflights != 0 || effects.rejects != 0 {
				t.Fatalf("paused worker tried to recover a pack: %v", err)
			}
			if err := local.Write(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `UPDATE action_pause SET paused = 0 WHERE singleton = 1`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := worker.step(ctx); !errors.Is(err, arradapter.ErrUncertain) {
				t.Fatalf("expected lost removal response: %v", err)
			}
			if err := observations.RemoveInstance(ctx, instance.Ref.ID, "owner"); !errors.Is(err, app.ErrConflict) {
				t.Fatalf("connection could be retired while a pack send is unresolved: %v", err)
			}
			// The queue collector can close the old local case before reconciliation.
			if err := observations.Collect(ctx, instance.Ref.ID); err != nil {
				t.Fatal(err)
			}
			worker = &emptyPackWorker{cases: observations, journal: NewEmptyPackJournal(local), clock: clock, connection: factory}
			if err := worker.step(ctx); err != nil || len(effects.searches) != 0 {
				t.Fatal("search ran before blocklist and removal were verified", err)
			}
			effects.verified = true
			if err := worker.step(ctx); test.lostSearch != errors.Is(err, arradapter.ErrUncertain) || (!test.lostSearch && err != nil) {
				t.Fatalf("unexpected search outcome: %v", err)
			}
			if err := worker.step(ctx); err != nil {
				t.Fatal(err)
			}
			wantStage := EmptyPackDone
			if test.lostSearch {
				wantStage = EmptyPackSearchSending
			}
			record, found, err := worker.journal.Load(ctx, key)
			if err != nil || !found || record.Stage != wantStage || effects.rejects != 1 || effects.preflights != 1 {
				t.Fatalf("recovery was missing or repeated: %+v %v, %d removals", record, err, effects.rejects)
			}
			if test.alternative {
				if len(effects.searches) != 0 {
					t.Fatal("searched for an episode covered by another job")
				}
			} else if len(effects.searches) != 1 || !slices.Equal(effects.searches[0], []int64{26}) {
				t.Fatalf("search must include just episode 26 exactly once: %v", effects.searches)
			}
		})
	}
}
