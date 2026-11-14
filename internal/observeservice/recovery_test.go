package observeservice

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observe"
)

func TestFreshRecoveryQueueChecksSavedCaseAndWholeSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows string
		size int
		err  error
	}{
		{"exact", radarrMovie, 1, nil},
		{"row reused", strings.Replace(radarrMovie, `"123"`, `"456"`, 1), 1, app.ErrConflict},
		{"shared job", radarrMovie + "," + strings.Replace(strings.Replace(radarrMovie, `"id":4`, `"id":5`, 1), `"movieId":11`, `"movieId":12`, 1), 2, app.ErrConflict},
		{"new disposition", strings.Replace(radarrMovie, `"failed"`, `"completed"`, 1), 1, app.ErrConflict},
		{"partial", radarrMovie, 2, observe.ErrPartial},
		{"missing", "", 0, app.ErrConflict},
		{"connection changed during read", radarrMovie, 1, app.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := testStore(t)
			clock := app.FixedClock{At: time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC)}
			first := true
			svc := New(db, clock, fixture(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet {
					t.Fatal("recovery queue read attempted a mutation")
				}
				if req.URL.Path == "/arr/api/v3/system/status" {
					return response(200, `{"appName":"Radarr","version":"6.4.4.10685"}`)
				}
				if first {
					first = false
					return response(200, queue(radarrMovie, 1))
				}
				if tc.name == "connection changed during read" {
					err := db.Write(t.Context(), func(tx *sql.Tx) error {
						_, err := tx.ExecContext(t.Context(), "UPDATE observe_instances SET connection_revision = connection_revision + 1")
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				return response(200, queue(tc.rows, tc.size))
			}))
			instance := add(t, svc, domain.ProductRadarr)
			if err := svc.Collect(t.Context(), instance.Ref.ID); err != nil {
				t.Fatal(err)
			}
			page, err := svc.Cases(t.Context(), app.CaseQuery{Instance: instance.Ref.ID})
			if err != nil || len(page.Cases) != 1 {
				t.Fatalf("saved case unavailable: %v", err)
			}
			item := page.Cases[0]
			if _, err := svc.FreshRecoveryQueue(t.Context(), item.ID, item.Revision+1); !errors.Is(err, app.ErrConflict) {
				t.Fatalf("stale revision returned %v", err)
			}
			result, err := svc.FreshRecoveryQueue(t.Context(), item.ID, item.Revision)
			if !errors.Is(err, tc.err) {
				t.Fatalf("wanted %v, got %v", tc.err, err)
			}
			if err != nil {
				return
			}
			if result.Case.ID != item.ID || result.Instance.Ref != instance.Ref ||
				len(result.Members) != 1 || result.Members[0].DownloadID != "123" ||
				result.Members[0].OutputPath != "/private/not-for-console" ||
				result.Case.Job.Members[0].SnapshotGeneration != result.Queue.Evidence.SnapshotGeneration {
				t.Fatal("private queue was not bound to saved case and selector")
			}
			body, err := json.Marshal(result)
			if err != nil || string(body) != "{}" {
				t.Fatal("recovery queue serialised private selectors")
			}
		})
	}
}
