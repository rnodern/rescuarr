package recoveryservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rnodern/rescuarr/internal/app"
	"github.com/rnodern/rescuarr/internal/arradapter"
	"github.com/rnodern/rescuarr/internal/domain"
	"github.com/rnodern/rescuarr/internal/observeservice"
	"github.com/rnodern/rescuarr/internal/store"
)

func TestManualEmptyPackExecutesOnlyApprovedSearchAfterRestart(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "state")
	local, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	clock := app.FixedClock{At: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	fixture := &emptyPackRuntimeFixture{now: clock.At}
	transport := fixtureTransport(func(r *http.Request) (*http.Response, error) {
		response, err := fixture.roundTrip(r)
		if err != nil || r.Method != http.MethodGet || r.URL.Path != "/api/v3/episode" || fixture.removals == 0 {
			return response, err
		}
		// Episode 11 was in the library at approval. It becomes missing after
		// removing the empty pack. The worker must never add it to the search.
		defer response.Body.Close()
		var rows []map[string]any
		if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row["id"] == float64(11) {
				row["hasFile"], row["episodeFileId"], row["airDateUtc"] = false, 0, clock.At.Add(-time.Hour)
			}
		}
		data, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		response.ContentLength = int64(len(data))
		response.Body = io.NopCloser(bytes.NewReader(data))
		return response, nil
	})
	observations := observeservice.New(local, clock, transport)
	instance, err := observations.AddInstance(ctx, observeservice.InstanceInput{
		Product: domain.ProductSonarr, DisplayName: "Fixture Sonarr",
		BaseURL: "https://sonarr.invalid", APIKey: "fixture-key", Actor: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := observations.Collect(ctx, instance.Ref.ID); err != nil {
		t.Fatal(err)
	}
	factory := func(_ context.Context, current domain.Instance) (*arradapter.EmptyPackAdapter, error) {
		key, err := local.ReadSecret(current.SecretRef)
		if err != nil {
			return nil, err
		}
		return arradapter.NewEmptyPackAdapter(arradapter.Config{
			Instance: current, APIKey: key, AdapterRevision: "manual-fixture",
			SettingsFingerprint: sha256.Sum256([]byte("fixture settings")),
		}, transport.RoundTrip, func(_ context.Context, binding arradapter.Binding) (arradapter.Qualification, error) {
			return arradapter.Qualification{Binding: binding, Verified: true, Evidence: "fixture-only",
				ObservedAt: clock.At.Add(-time.Second), ExpiresAt: clock.At.Add(time.Minute)}, nil
		}, clock.Now)
	}
	reviews := NewManualReviews(local, observations, NewPreviewService(observations, local, transport, clock),
		transport, clock, factory)
	if err := reviews.SetManualMode(ctx, true, "owner"); err != nil {
		t.Fatal(err)
	}
	view, err := reviews.ManualReview(ctx)
	if err != nil || !view.CanApprove || view.Case == nil || view.Folder != "original" ||
		view.Preview.Replacement == nil || view.Preview.Replacement.SearchCount != 1 {
		t.Fatalf("unexpected review: %+v, %v", view, err)
	}
	worker := newEmptyPackWorker(ctx, observations, local, clock, transport, factory)
	if err := worker.manualStep(ctx, transport); err != nil || fixture.removals != 0 {
		t.Fatalf("unapproved review reached removal: count %d, error %v", fixture.removals, err)
	}
	if err := reviews.DecideManualReview(ctx, app.ManualDecision{Token: view.ApprovalToken, Approve: true, Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := worker.manualStep(ctx, transport); err != nil || fixture.removals != 1 || fixture.searches != 0 {
		t.Fatalf("expected one removal and no search before effect verification: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}

	local, err = store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	observations = observeservice.New(local, clock, transport)
	worker = newEmptyPackWorker(ctx, observations, local, clock, transport, factory)
	if err := worker.manualStep(ctx, transport); err != nil {
		t.Fatal(err)
	}
	key := EmptyPackKey{CaseID: view.Case.ID, Job: view.Case.Job.Ref}
	record, found, err := worker.journal.Load(ctx, key)
	if err != nil || !found || record.Stage != EmptyPackDone || record.CommandID != 81 ||
		!slices.Equal(record.SearchIDs, []int64{12}) || fixture.removals != 1 || fixture.searches != 1 {
		t.Fatalf("unexpected recovery: %+v, %d removals, %d searches, %v", record, fixture.removals, fixture.searches, err)
	}
	var state string
	if err := local.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT state FROM manual_reviews WHERE case_id = ?`, key.CaseID).Scan(&state)
	}); err != nil || state != "completed" {
		t.Fatalf("review did not advance on verified result: %q, %v", state, err)
	}
}
